package run

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/shell"
	"github.com/antimatter-studios/chore/internal/tmpl"
)

// chore:manual preconditions
// title: Preconditions
// summary: refuse a task before preparation or work starts
// aliases: precondition checks
//
// # Preconditions
//
// `preconditions:` lists what must be true before a task can run. Each entry is
// a shell string, `{sh: command, msg: explanation}`, or a task check:
//
// ```yaml
// tasks:
//   release:
//     preconditions:
//       - sh: git diff --quiet HEAD
//         msg: 'Commit first: a release needs a clean tree'
//       - task: on-main
//         msg: 'Release what is on main: check out main and pull'
//       - test -z "$(git tag -l v{{.VERSION}})"
//     before: [./prepare-release.sh]
//     cmds: [./release.sh]
//   on-main:
//     desc: HEAD is origin/main
//     cmd: git fetch -q origin main && test "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)"
// ```
//
// Platform restrictions and `requires:` are checked first. Preconditions then
// run in order, before `before:`, dependencies, up-to-date checks and commands.
// The first failure refuses the task with the check's exit status. Its task
// hooks and deferred steps never run; run-level failure and finish hooks do.
// A task may have both `preconditions:` and `before:`: the checks permit it to
// run, then `before:` prepares it.
//
// Output is quiet unless `-v` asks for commands and their output. A custom
// `msg:` is rendered in the task's scope; without one the error names the
// rendered command or task. Shell checks run locally in the task's `dir:`.
// Task checks accept `vars:` like a task step and use ordinary references:
// bare names in this file, `:name` in the root, `global:<file>:<name>` globally.
// A task check cannot have `route:`. Its result is remembered for the invocation
// by task and passed variables, so shared checks run once.
//
// Neither `--force`, `--no-lifecycle` nor `child_hooks: false` bypasses checks.
// A task's `timeout:` includes them. `--dry` prints `check:` lines and executes
// no checks. The shell and `{sh, msg}` forms accept go-task Taskfiles, but chore
// always checks before dependencies and never bypasses them with `--force`.

// RefusalError carries the explanation without appending the underlying shell
// error, while preserving its exit status for callers.
type RefusalError struct {
	Task    string
	Message string
	Cause   error
}

func (e *RefusalError) Error() string { return e.Task + ": " + e.Message }
func (e *RefusalError) Unwrap() error { return e.Cause }
func (e *RefusalError) ExitCode() int { return shell.ExitCode(e.Cause) }

type checkOutputKey struct{}
type checkStackKey struct{}

// Output suppression belongs to the context, so concurrent dependency tasks
// never change the runner's shared writers. Capture commands through their
// existing exports path where needed; otherwise consume their output quietly.
func (r *Runner) output(ctx context.Context) io.Writer {
	if quiet, _ := ctx.Value(checkOutputKey{}).(bool); quiet {
		return io.Discard
	}
	return r.Out
}

func (r *Runner) errorOutput(ctx context.Context) io.Writer {
	if quiet, _ := ctx.Value(checkOutputKey{}).(bool); quiet {
		return io.Discard
	}
	return r.Err
}

func checkStack(ctx context.Context) map[string]bool {
	stack, _ := ctx.Value(checkStackKey{}).(map[string]bool)
	return stack
}

func (r *Runner) preconditions(ctx context.Context, t *chorefile.Task, scope *tmpl.Scope, dir string) error {
	if len(t.Preconditions) == 0 {
		return nil
	}
	stack := make(map[string]bool, len(checkStack(ctx))+1)
	for name := range checkStack(ctx) {
		stack[name] = true
	}
	stack[t.Name] = true
	ctx = context.WithValue(ctx, checkStackKey{}, stack)
	for _, c := range t.Preconditions {
		check, err := scope.Render(c.Sh)
		if c.Task != "" {
			check, err = scope.Render(c.Task)
		}
		if err != nil {
			return fmt.Errorf("%s: rendering precondition: %w", t.Name, err)
		}
		if strings.TrimSpace(check) == "" {
			return fmt.Errorf("%s: precondition rendered an empty command or task", t.Name)
		}
		message := "precondition failed: " + check
		if c.Task != "" {
			message = "precondition failed: task " + check
		}
		if c.Msg != "" {
			message, err = scope.Render(c.Msg)
			if err != nil {
				return fmt.Errorf("%s: rendering precondition message: %w", t.Name, err)
			}
		}
		if r.DryRun {
			label := check
			if c.Task != "" {
				label = "task " + check
			}
			fmt.Fprintf(r.output(ctx), "check: %s\n", label)
			continue
		}
		inner := context.WithValue(ctx, answeringKey{}, true)
		if !r.Verbose {
			inner = context.WithValue(inner, checkOutputKey{}, true)
		}
		sh := r.shell(inner, dir, scope)
		if c.Task != "" {
			name := chorefile.Reference(t.File, check)
			var cv map[string]string
			cv, err = scope.Resolve(inner, c.Vars, sh)
			if err == nil {
				err = r.checkTask(inner, name, cv)
			}
		} else {
			if r.Verbose {
				fmt.Fprintf(r.output(inner), "%s\n", check)
			}
			err = sh.Run(inner, check)
		}
		if err != nil {
			return &RefusalError{Task: t.Name, Message: message, Cause: err}
		}
	}
	return nil
}

type checkEntry struct {
	done chan struct{}
	err  error
}

func (r *Runner) checkTask(ctx context.Context, name string, vars map[string]string) error {
	t := r.Project.Tasks[name]
	if t == nil {
		return fmt.Errorf("no precondition task %q%s", name, r.suggest(name))
	}
	if t.Route != "" {
		return fmt.Errorf("precondition task %q has `route: %s` — a precondition must run on this machine", name, t.Route)
	}
	if checkStack(ctx)[t.Name] {
		return fmt.Errorf("precondition cycle through task %q", name)
	}
	vars, err := foldCallVars(t, vars)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(vars)
	if err != nil {
		return err
	}
	key := t.Name + "\x00" + string(encoded)
	r.checkMu.Lock()
	if r.checks == nil {
		r.checks = map[string]*checkEntry{}
	}
	e := r.checks[key]
	first := e == nil
	if first {
		e = &checkEntry{done: make(chan struct{})}
		r.checks[key] = e
	}
	r.checkMu.Unlock()
	if first {
		e.err = r.Run(ctx, name, nil, vars)
		close(e.done)
	}
	select {
	case <-e.done:
		return e.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
