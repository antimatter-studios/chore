// Package shell runs shell scripts. It is the only place in the program that
// knows how a command becomes a process.
//
// One rule shapes everything here: a script is ONE program. A `cmds:` entry like
//
//	set -euo pipefail
//	id=$(docker ps -q --filter name=mail)
//	if [ -n "$id" ]; then docker stop "$id"; fi
//
// is one shell program, so `set -e` stays in force, `id` is still set on the next
// line, and a heredoc spans lines. Runners that hand each line to a fresh shell
// get all three wrong, and the Taskfiles this has to run depend on the correct
// behaviour.
//
// The shell is the SYSTEM shell, not an embedded interpreter. Task embeds
// mvdan.cc/sh because it supports Windows, and pays for it in two ways this
// program cannot accept: that interpreter does not implement `set -o pipefail`,
// so a failing pipeline reports success — silently, which is the exact class of
// bug chore exists to remove — and its builtins differ subtly, e.g. `printf`
// pads by runes rather than bytes, so aligned output drifts. Targeting macOS and
// Linux only means the real shell is available, with exactly the semantics a
// developer gets at their own prompt.
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Shell is a shell to run scripts in. It is a value, not a session: each Run or
// Capture starts a fresh process, so nothing — variables, options, `cd` — leaks
// from one script into the next. Sharing state within a script is the point;
// sharing it between tasks is not.
type Shell struct {
	// Dir is the working directory. Empty means the process's own, which is what
	// a task without `dir:` wants.
	Dir string
	// Env is the complete environment in K=V form. Nil inherits the process
	// environment; empty means an empty environment, not an inherited one.
	Env []string
	Out io.Writer
	Err io.Writer
	// In is the reader an interactive task gets as stdin. Unused otherwise: a
	// task that does not ask for the terminal must not be able to eat the
	// keystrokes meant for chore itself.
	In io.Reader
	// Interactive gives the script the terminal — see exec for what that costs.
	Interactive bool
	// Bin overrides the shell binary. Empty prefers bash, falling back to sh:
	// bash is a superset, and scripts in the wild assume more than POSIX more
	// often than they admit.
	Bin string
}

// Run executes a script, streaming stdout and stderr to Out and Err.
func (s Shell) Run(ctx context.Context, script string) error {
	return s.exec(ctx, script, s.Out)
}

// Capture executes a script and returns its stdout with trailing newlines
// trimmed — the form a variable wants. stderr still reaches s.Err, so a script
// that warns while producing a value does not lose the warning.
func (s Shell) Capture(ctx context.Context, script string) (string, error) {
	// A captured value is chore reading a command, not a human answering one.
	// Inheriting stdin here would let a `sh:` var or a `sources:` check swallow
	// the keystrokes intended for the task itself. The receiver is a value, so
	// this is local to the call.
	s.Interactive = false
	var buf strings.Builder
	err := s.exec(ctx, script, &buf)
	return strings.TrimRight(buf.String(), "\n"), err
}

// scriptName is what a script sees as $0, and what the shell prefixes its own
// diagnostics with. `sh -c script name` sets argv[0] for the script.
const scriptName = "chore"

func (s Shell) exec(ctx context.Context, script string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, s.bin(), "-c", script, scriptName)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	cmd.Stdout = out
	cmd.Stderr = s.Err

	if s.Interactive {
		// The task asked for the terminal, so give it one — both halves matter.
		//
		// stdin, because a nil Stdin is /dev/null: `read -rs token` then returns
		// EOF at once and the script proceeds with an empty answer it never
		// prompted for.
		//
		// And NO new process group. A child in its own group is a BACKGROUND
		// group as far as the terminal is concerned: it cannot become the
		// foreground, so a full-screen program draws nothing until it dies, and
		// reading /dev/tty raises SIGTTIN and stops it. Sharing chore's group is
		// what lets it take the terminal at all.
		cmd.Stdin = s.In
		// Which is why cancelling must signal the PROCESS. -pid here would name
		// chore's own process group — the shell that launched it included.
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			return cmd.Process.Signal(syscall.SIGTERM)
		}
		cmd.WaitDelay = 2 * time.Second
		return s.wait(ctx, cmd)
	}

	// Own process group, so cancelling kills what the script started rather than
	// only the shell: a task running `docker logs -f` would otherwise leave the
	// child holding the terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 2 * time.Second

	return s.wait(ctx, cmd)
}

// wait runs the command and translates how it ended.
func (s Shell) wait(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Run(); err != nil {
		// A cancelled context is not a script failure: the caller stopped the
		// work, so report that rather than the SIGTERM exit status (143) the
		// shell died with, which a caller would otherwise have to decode.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return exitError(err)
	}
	return nil
}

// bin picks the shell to run scripts with.
//
// $SHELL is deliberately ignored. On macOS it is zsh, and zsh does not word-split
// unquoted expansions: `x="a b c"; for i in $x` iterates ONCE there and three
// times in bash. Taskfile scripts are written against POSIX/bash semantics — one
// in this project's own corpus builds a list of project prefixes and loops over
// it unquoted — so running them in the user's interactive shell would change
// their meaning without any error to show for it.
//
// PATH comes first among the bash candidates: macOS still ships bash 3.2 (2007)
// at /bin/bash, whose parser mishandles a `case` pattern inside `$( … )` — it
// ends the command substitution at the pattern's closing paren and then reports a
// syntax error at the `;;`. Real Taskfiles contain exactly that construct, so a
// developer's newer bash on PATH is preferred over the system copy.
func (s Shell) bin() string {
	if s.Bin != "" {
		return s.Bin
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	for _, candidate := range []string{"/bin/bash", "/usr/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "sh"
}

// ExitError is a script that ran and failed. Code is the shell exit status, so
// chore can exit with the same code its command did.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// ExitCode reports the status the script exited with. The name matches
// exec.ExitError's, so callers can treat either the same way.
func (e *ExitError) ExitCode() int { return e.Code }

// Unwrap exposes the underlying process error.
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode returns the exit status err represents: 0 for nil, the script's own
// code for an *ExitError, and 1 for anything else — a cancelled context or a
// shell that could not start is still a failure, and chore must exit non-zero.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *ExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	return 1
}

// exitError translates the process error. A script that exited non-zero is a
// script failure carrying its code; anything else (shell missing, bad working
// directory) is an operational failure and passes through untouched.
func exitError(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if code < 0 {
			// Killed by a signal, so it has no exit status of its own.
			// 128+signal is the convention every shell uses.
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
			} else {
				code = 1
			}
		}
		return &ExitError{Code: code, Err: err}
	}
	return err
}
