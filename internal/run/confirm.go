package run

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/tmpl"
)

// chore:manual confirm
// title: Confirm
// summary: confirm: — ask a person before the task does its work
// aliases: confirmation, yes, ask
//
// # Confirm
//
// `confirm:` asks the person who ran the task, once, immediately before its own
// commands. Nothing runs until they answer.
//
// ```yaml
// tasks:
//   up:
//     confirm: 'Deploy {{.STACK}}?'
//     cmds: [pulumi up --yes]
//
//   install:
//     confirm:
//       text: 'Install a mail server into {{.CONTEXT}}?'
//       show: kubectl config get-contexts {{.CONTEXT}}
//       default: no
//     cmds: [./install.sh]
// ```
//
// The short form is the whole feature for most tasks. `show:` runs a command
// and prints its output first, for the question you can only answer after
// looking at something. `default:` is the answer a bare Enter gives — `no`
// unless the file says otherwise, so the key pressed without reading cannot do
// anything. `y`, `yes`, `n` and `no` are accepted in any case; anything else is
// asked again.
//
// ## Why it is not a precondition
//
// `preconditions:` answers "may this run?" — a fact about the world. This
// answers "do you want this?" — intent from a person. Two consequences follow
// from keeping them apart, and the second is the reason:
//
//   - `--yes` means exactly one thing. Were the question a precondition, the
//     flag would have to bypass SOME preconditions and not others.
//   - Preconditions run before `before:` and `deps:`. A question asked there
//     would be asked before the thing that informs it — the preview, the diff,
//     the context — had been produced. `confirm:` is asked last instead.
//
// ## Where it sits
//
// ```
// preconditions → before → up-to-date → deps → CONFIRM → cmds → ...
// ```
//
// So: everything that could refuse the task has already refused it, every
// dependency has been built, and a task that was up to date never asks —
// there is no point confirming work that is not going to happen.
//
// ## Answering without a keyboard
//
// `--yes` (`-y`), or `CHORE_YES=1` for a caller whose command line you do not
// control, answers yes in advance. `-v` reports each question it answered that
// way.
//
// **With no terminal and no `--yes`, the task is REFUSED** — never assumed to
// be a yes. A pipeline that grew a confirm fails loudly on its next run rather
// than quietly proceeding, which is the one outcome a confirmation exists to
// prevent. The refusal prints the question, so the log says what was being
// asked.
//
// `--dry` prints a `confirm:` line and asks nothing, exactly as it lists
// `check:` lines without running them. `--force` and `--no-lifecycle` do not
// answer it: one is about up-to-date checks and the other about hooks.
//
// ## Details
//
// The question and anything `show:` prints go to **stderr**, so a confirmation
// never lands in piped output, and `chore deploy > log` still shows you the
// diff and the question.
//
// An answer is remembered for the invocation by task and rendered question, so
// a confirm on a shared dependency is asked once. Questions are asked one at a
// time even when concurrent `deps:` reach them together: there is one keyboard.
// A failing `show:` fails the task — there is nothing left to decide on.

// DeclineError is a task that was not approved: either answered no, or asked
// where there was nobody to answer.
//
// Separate from RefusalError because the two are different events that happen
// to stop the same run. A precondition says the task could not have worked; this
// says it could have, and someone said not to.
type DeclineError struct {
	Task    string
	Message string
}

func (e *DeclineError) Error() string { return e.Task + ": " + e.Message }

// confirm asks the task's question, if it has one. Called from execute after
// deps and the up-to-date check, so it is the last thing between a task and its
// commands.
func (r *Runner) confirm(ctx context.Context, t *chorefile.Task, scope *tmpl.Scope, dir string) error {
	if t.Confirm == nil {
		return nil
	}
	question, err := scope.Render(t.Confirm.Text)
	if err != nil {
		return fmt.Errorf("%s: rendering confirm: %w", t.Name, err)
	}
	if strings.TrimSpace(question) == "" {
		return fmt.Errorf("%s: confirm rendered an empty question", t.Name)
	}
	if r.DryRun {
		fmt.Fprintf(r.output(ctx), "confirm: %s\n", question)
		return nil
	}
	if r.Yes {
		if r.Verbose {
			fmt.Fprintf(r.errorOutput(ctx), "chore: %s: answered yes in advance: %s\n", t.Name, question)
		}
		return nil
	}

	// One keyboard, so one question at a time. Held across the whole exchange —
	// the `show:` output included, since a diff interleaved with another task's
	// is not a diff anybody can read — which also makes the memo below safe.
	r.askMu.Lock()
	defer r.askMu.Unlock()
	key := t.Name + "\x00" + question
	if answer, asked := r.answers[key]; asked {
		if answer {
			return nil
		}
		return &DeclineError{Task: t.Name, Message: "declined"}
	}

	// The question goes to stderr rather than through r.output: inside a
	// precondition's quiet context that writer is io.Discard, and a question
	// nobody can see, asked by a program that then waits for an answer, is the
	// worst thing this could do.
	if !r.canAsk() {
		return &DeclineError{
			Task:    t.Name,
			Message: fmt.Sprintf("needs an answer and there is no terminal to ask at: %q — pass --yes to answer it in advance", question),
		}
	}
	if t.Confirm.Show != "" {
		if err := r.show(ctx, t, scope, dir); err != nil {
			return err
		}
	}
	answer, err := askYesNo(ctx, r.Err, r.Stdin, question, t.Confirm.Default)
	if err != nil {
		return err
	}
	if r.answers == nil {
		r.answers = map[string]bool{}
	}
	r.answers[key] = answer
	if !answer {
		return &DeclineError{Task: t.Name, Message: "declined"}
	}
	return nil
}

// show runs the confirm's `show:` command, putting its output where the
// question goes. A failure is the task's failure: the point of the command is
// that the answer depends on what it prints, so an answer given without it
// would be an answer to a different question.
func (r *Runner) show(ctx context.Context, t *chorefile.Task, scope *tmpl.Scope, dir string) error {
	cmd, err := scope.Render(t.Confirm.Show)
	if err != nil {
		return fmt.Errorf("%s: rendering confirm `show:`: %w", t.Name, err)
	}
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("%s: confirm `show:` rendered an empty command", t.Name)
	}
	sh := r.shell(ctx, dir, scope)
	sh.Out, sh.Err = r.Err, r.Err
	if r.Verbose {
		fmt.Fprintf(r.Err, "%s\n", cmd)
	}
	if err := sh.Run(ctx, cmd); err != nil {
		return fmt.Errorf("%s: confirm `show:` failed, so there is nothing to decide on: %w", t.Name, err)
	}
	return nil
}

// canAsk reports whether there is somebody to ask: chore's own stdin is a
// terminal. A pipe is not a person, and reading an answer out of one would mean
// a `yes` could arrive from a file nobody meant as consent.
func (r *Runner) canAsk() bool {
	f, ok := r.Stdin.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// askYesNo puts the question and reads the answer, re-asking anything that is
// neither. A cancelled context — Ctrl-C at the prompt — ends the run rather
// than counting as an answer.
func askYesNo(ctx context.Context, out io.Writer, in io.Reader, question string, def bool) (bool, error) {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		fmt.Fprintf(out, "%s %s ", question, hint)
		line, err := readLine(ctx, in)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(out)
				return false, ctx.Err()
			}
			// EOF: the terminal went away mid-question, which is not a yes.
			fmt.Fprintln(out)
			return false, nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(out, "Please answer y or n.")
	}
}

// readLine reads one line, a byte at a time, and stops at the newline.
//
// Deliberately unbuffered. A bufio.Reader reads AHEAD of the line it returns,
// so a task that asks a question and then hands the terminal to an
// `interactive:` command would have had the first of that command's input eaten
// before it started — a bug that would look like a dropped keystroke.
func readLine(ctx context.Context, in io.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var b []byte
		var one [1]byte
		for {
			n, err := in.Read(one[:])
			if n > 0 {
				if one[0] == '\n' {
					done <- result{line: strings.TrimSuffix(string(b), "\r")}
					return
				}
				b = append(b, one[0])
			}
			if err != nil {
				done <- result{line: string(b), err: err}
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		// The read is still blocked on the terminal and will stay there until the
		// process exits, which is where a cancelled run is going anyway.
		return "", ctx.Err()
	case res := <-done:
		return res.line, res.err
	}
}
