package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// capture runs script in dir and returns stdout, stderr and the error, which is
// what most tests here want to look at together.
func capture(t *testing.T, dir, script string) (string, string, error) {
	t.Helper()
	var stderr strings.Builder
	sh := Shell{Dir: dir, Err: &stderr}
	out, err := sh.Capture(context.Background(), script)
	return out, stderr.String(), err
}

// TestScriptIsOneProgram is the behaviour the whole package exists for: state
// set on one line is visible on the next, because the script is interpreted as
// a single shell file rather than line by line.
func TestScriptIsOneProgram(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte("in sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		script string
		want   string
	}{
		{
			name:   "variable set on one line is read on the next",
			script: "x=1\necho $x",
			want:   "1",
		},
		{
			name:   "later lines see an exported variable",
			script: "export GREETING=hello\nprintf '%s world' \"$GREETING\"",
			want:   "hello world",
		},
		{
			name:   "a function defined on one line is called on another",
			script: "greet() { echo \"hi $1\"; }\ngreet bob",
			want:   "hi bob",
		},
		{
			name:   "cd persists to the following lines",
			script: "cd sub\ncat f.txt",
			want:   "in sub",
		},
		{
			name:   "a heredoc spans lines",
			script: "cat <<'EOF'\nline1\nline2\nEOF",
			want:   "line1\nline2",
		},
		{
			name:   "an expanding heredoc sees earlier variables",
			script: "who=world\ncat <<EOF\nhello $who\nEOF",
			want:   "hello world",
		},
		{
			name:   "a multi-line if reads a variable from above",
			script: "n=2\nif [ \"$n\" -gt 1 ]; then\n  echo many\nelse\n  echo one\nfi",
			want:   "many",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, stderr, err := capture(t, dir, tt.script)
			if err != nil {
				t.Fatalf("Capture: %v (stderr %q)", err, stderr)
			}
			if out != tt.want {
				t.Errorf("out = %q, want %q", out, tt.want)
			}
		})
	}
}

// TestSetOptionsPersist covers `set -euo pipefail`, which only works if the
// options outlive the line that set them.
func TestSetOptionsPersist(t *testing.T) {
	const strict = "set -euo pipefail\n"

	tests := []struct {
		name     string
		script   string
		wantCode int
		wantOut  string
	}{
		{
			name:     "errexit stops the script at the first failure",
			script:   strict + "false\necho unreachable",
			wantCode: 1,
		},
		{
			name:     "errexit exits with the failing command's code",
			script:   strict + "(exit 42)\necho unreachable",
			wantCode: 42,
		},
		{
			name:     "pipefail reports a failure inside a pipeline",
			script:   strict + "(exit 3) | cat\necho unreachable",
			wantCode: 3,
		},
		{
			name:     "nounset rejects an undefined variable",
			script:   strict + "echo \"[${MISSING}]\"\necho unreachable",
			wantCode: 1,
		},
		{
			name:     "errexit does not fire on a tested failure",
			script:   strict + "if false; then echo yes; fi\necho done",
			wantCode: 0,
			wantOut:  "done",
		},
		{
			name:     "without errexit the script continues past a failure",
			script:   "false\necho done",
			wantCode: 0,
			wantOut:  "done",
		},
		{
			name:     "without nounset an undefined variable is empty",
			script:   "echo \"[${MISSING}]\"",
			wantCode: 0,
			wantOut:  "[]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := capture(t, t.TempDir(), tt.script)
			if got := ExitCode(err); got != tt.wantCode {
				t.Fatalf("ExitCode = %d (err %v), want %d", got, err, tt.wantCode)
			}
			if out != tt.wantOut {
				t.Errorf("out = %q, want %q", out, tt.wantOut)
			}
			if strings.Contains(out, "unreachable") {
				t.Errorf("script kept going after a failure: out = %q", out)
			}
		})
	}
}

// TestExitCodePropagation checks that tsk can exit with the code its command
// exited with, which is the whole contract of ExitError.
func TestExitCodePropagation(t *testing.T) {
	for _, code := range []int{0, 1, 2, 42, 127} {
		t.Run(fmt.Sprintf("exit_%d", code), func(t *testing.T) {
			sh := Shell{Dir: t.TempDir()}
			err := sh.Run(context.Background(), fmt.Sprintf("exit %d", code))

			if code == 0 {
				if err != nil {
					t.Fatalf("Run = %v, want nil", err)
				}
				return
			}

			var exit *ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("Run = %v (%T), want *ExitError", err, err)
			}
			if exit.ExitCode() != code {
				t.Errorf("ExitCode() = %d, want %d", exit.ExitCode(), code)
			}
			if ExitCode(err) != code {
				t.Errorf("ExitCode(err) = %d, want %d", ExitCode(err), code)
			}
			if want := fmt.Sprintf("exit status %d", code); exit.Error() != want {
				t.Errorf("Error() = %q, want %q", exit.Error(), want)
			}
		})
	}
}

// TestFailingCommandCode covers the codes that come from real processes and
// builtins rather than an explicit `exit`.
func TestFailingCommandCode(t *testing.T) {
	tests := []struct {
		name     string
		script   string
		wantCode int
	}{
		{name: "false", script: "false", wantCode: 1},
		{name: "command not found", script: "definitely-not-a-real-command", wantCode: 127},
		{name: "external command's code", script: "sh -c 'exit 9'", wantCode: 9},
		{name: "last command wins", script: "exit 5\n", wantCode: 5},
		{name: "status of the last line, not the failing one", script: "false\ntrue", wantCode: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := capture(t, t.TempDir(), tt.script)
			if got := ExitCode(err); got != tt.wantCode {
				t.Fatalf("ExitCode = %d (err %v), want %d", got, err, tt.wantCode)
			}
		})
	}
}

func TestCaptureTrimsTrailingNewlines(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   string
	}{
		{name: "single line", script: "echo hi", want: "hi"},
		{name: "no trailing newline at all", script: "printf 'hi'", want: "hi"},
		{name: "several trailing newlines", script: "printf 'hi\\n\\n\\n'", want: "hi"},
		{name: "interior newlines are kept", script: "printf 'a\\nb\\nc\\n'", want: "a\nb\nc"},
		{name: "blank interior line is kept", script: "printf 'a\\n\\nb\\n'", want: "a\n\nb"},
		{name: "leading whitespace is kept", script: "printf '  spaced  \\n'", want: "  spaced  "},
		{name: "empty output", script: "true", want: ""},
		{name: "only newlines", script: "printf '\\n\\n'", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := capture(t, t.TempDir(), tt.script)
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			if out != tt.want {
				t.Errorf("out = %q, want %q", out, tt.want)
			}
		})
	}
}

func TestCaptureSeparatesStderr(t *testing.T) {
	var stderr strings.Builder
	sh := Shell{Dir: t.TempDir(), Err: &stderr}

	out, err := sh.Capture(context.Background(), "echo to-stdout\necho to-stderr >&2")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if out != "to-stdout" {
		t.Errorf("out = %q, want %q", out, "to-stdout")
	}
	if stderr.String() != "to-stderr\n" {
		t.Errorf("stderr = %q, want %q", stderr.String(), "to-stderr\n")
	}
}

// TestCaptureReturnsOutputBeforeFailure matters for `status:` checks, which may
// print an explanation and then exit non-zero.
func TestCaptureReturnsOutputBeforeFailure(t *testing.T) {
	out, _, err := capture(t, t.TempDir(), "echo partial\nexit 3")
	if ExitCode(err) != 3 {
		t.Fatalf("ExitCode = %d (err %v), want 3", ExitCode(err), err)
	}
	if out != "partial" {
		t.Errorf("out = %q, want %q", out, "partial")
	}
}

func TestRunStreamsToOutAndErr(t *testing.T) {
	var stdout, stderr strings.Builder
	sh := Shell{Dir: t.TempDir(), Out: &stdout, Err: &stderr}

	if err := sh.Run(context.Background(), "echo one\necho two >&2\necho three"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stdout.String() != "one\nthree\n" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "one\nthree\n")
	}
	if stderr.String() != "two\n" {
		t.Errorf("stderr = %q, want %q", stderr.String(), "two\n")
	}
}

// TestNilWritersDiscard: a caller that does not care about output should not
// have to supply writers.
func TestNilWritersDiscard(t *testing.T) {
	sh := Shell{Dir: t.TempDir()}
	if err := sh.Run(context.Background(), "echo out\necho err >&2"); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestDir(t *testing.T) {
	t.Run("relative paths resolve against Dir", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello from dir\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		out, stderr, err := capture(t, dir, "cat note.txt")
		if err != nil {
			t.Fatalf("Capture: %v (stderr %q)", err, stderr)
		}
		if out != "hello from dir" {
			t.Errorf("out = %q, want %q", out, "hello from dir")
		}
	})

	t.Run("pwd reports Dir", func(t *testing.T) {
		dir := t.TempDir()
		out, _, err := capture(t, dir, "pwd")
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}
		if !samePath(t, out, dir) {
			t.Errorf("pwd = %q, want %q", out, dir)
		}
	})

	t.Run("empty Dir uses the process working directory", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)

		out, _, err := capture(t, "", "pwd")
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}
		if !samePath(t, out, dir) {
			t.Errorf("pwd = %q, want %q", out, dir)
		}
	})

	// A `dir:` pointing at nothing is a Taskfile bug, and must not silently run
	// the script somewhere else.
	t.Run("missing Dir is an error", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nope")
		_, _, err := capture(t, missing, "true")
		if err == nil {
			t.Fatal("Capture = nil, want an error")
		}
		var exit *ExitError
		if errors.As(err, &exit) {
			t.Errorf("setup failure surfaced as *ExitError: %v", err)
		}
		if ExitCode(err) != 1 {
			t.Errorf("ExitCode = %d, want 1", ExitCode(err))
		}
	})
}

func TestEnv(t *testing.T) {
	// A variable in the process environment, so "inherited" and "replaced" can
	// be told apart.
	t.Setenv("TSK_SHELL_TEST_LEAK", "leaked")

	tests := []struct {
		name string
		env  []string
		want string
	}{
		{
			name: "nil inherits the process environment",
			env:  nil,
			want: "leaked",
		},
		{
			name: "a set environment replaces the process environment",
			env:  []string{"TSK_SHELL_TEST_OWN=own"},
			want: "own/absent",
		},
		{
			name: "an empty but non-nil environment is empty",
			env:  []string{},
			want: "absent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr strings.Builder
			sh := Shell{Dir: t.TempDir(), Env: tt.env, Err: &stderr}

			// echo is a builtin, so this works even with no PATH.
			script := "echo \"${TSK_SHELL_TEST_LEAK:-absent}\""
			if tt.env != nil && len(tt.env) > 0 {
				script = "echo \"${TSK_SHELL_TEST_OWN:-absent}/${TSK_SHELL_TEST_LEAK:-absent}\""
			}

			out, err := sh.Capture(context.Background(), script)
			if err != nil {
				t.Fatalf("Capture: %v (stderr %q)", err, stderr.String())
			}
			if out != tt.want {
				t.Errorf("out = %q, want %q", out, tt.want)
			}
		})
	}

	// Env reaches external processes too, not just the interpreter's own
	// expansions.
	t.Run("external commands see Env", func(t *testing.T) {
		sh := Shell{
			Dir: t.TempDir(),
			Env: []string{"PATH=" + os.Getenv("PATH"), "TSK_SHELL_TEST_OWN=own"},
		}
		out, err := sh.Capture(context.Background(), "sh -c 'echo $TSK_SHELL_TEST_OWN'")
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}
		if out != "own" {
			t.Errorf("out = %q, want %q", out, "own")
		}
	})
}

func TestContextCancellation(t *testing.T) {
	// The interpreter interrupts a running external command on cancellation and
	// kills it if it ignores the interrupt; `sleep` dies on the interrupt, so
	// this returns in milliseconds, not the 5 seconds it asked for.
	t.Run("cancelling kills a running command", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var stdout strings.Builder
		sh := Shell{Dir: t.TempDir(), Out: &stdout}

		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		err := sh.Run(ctx, "sleep 5\necho unreachable")
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("Run = nil, want an error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled", err)
		}
		if elapsed > 2*time.Second {
			t.Errorf("Run took %v, want prompt cancellation", elapsed)
		}
		if strings.Contains(stdout.String(), "unreachable") {
			t.Errorf("script continued after cancellation: %q", stdout.String())
		}
	})

	t.Run("cancelling breaks a builtin loop", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		start := time.Now()
		err := Shell{Dir: t.TempDir()}.Run(ctx, "while true; do :; done\necho unreachable")
		if err == nil {
			t.Fatal("Run = nil, want an error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Run = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("Run took %v, want prompt cancellation", elapsed)
		}
	})

	t.Run("an already cancelled context runs nothing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var stdout strings.Builder
		sh := Shell{Dir: t.TempDir(), Out: &stdout}

		err := sh.Run(ctx, "echo ran")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
		if stdout.String() != "" {
			t.Errorf("stdout = %q, want nothing to have run", stdout.String())
		}
	})
}

// TestShellFeatures is a smoke test of the constructs rest-mail's Taskfiles use,
// so a change of interpreter or dialect shows up here.
func TestShellFeatures(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   string
	}{
		{name: "pipe", script: "printf 'a\\nb\\nc\\n' | grep b", want: "b"},
		{name: "pipe chain", script: "printf 'b\\na\\n' | sort | head -1", want: "a"},
		{name: "command substitution", script: "echo \"x$(echo y)z\"", want: "xyz"},
		{name: "nested command substitution", script: "echo \"$(echo \"$(echo deep)\")\"", want: "deep"},
		{name: "backquote substitution", script: "echo `echo old-style`", want: "old-style"},
		{name: "if/else/fi", script: "if [ 1 = 1 ]; then echo yes; else echo no; fi", want: "yes"},
		{name: "bash test brackets", script: "if [[ abc == a* ]]; then echo glob; fi", want: "glob"},
		{name: "and-or lists", script: "false && echo no || echo fallback", want: "fallback"},
		{name: "subshell keeps its own cwd", script: "(cd /; pwd) >/dev/null; echo back", want: "back"},
		{name: "for loop", script: "for i in 1 2 3; do printf '%s' \"$i\"; done", want: "123"},
		{name: "arithmetic", script: "echo $((2 + 3 * 4))", want: "14"},
		{name: "default expansion", script: "echo \"${UNSET:-fallback}\"", want: "fallback"},
		{name: "heredoc into a pipe", script: "grep -c . <<'EOF'\na\nb\nEOF", want: "2"},
		{name: "here-string style redirect", script: "cat <<<'inline'", want: "inline"},
		{name: "case", script: "case abc in a*) echo matched;; *) echo no;; esac", want: "matched"},
		{name: "exported var reaches a child process", script: "export V=1\nsh -c 'echo $V'", want: "1"},
		{name: "script name is $0", script: "echo $0", want: scriptName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, stderr, err := capture(t, t.TempDir(), tt.script)
			if err != nil {
				t.Fatalf("Capture: %v (stderr %q)", err, stderr)
			}
			if out != tt.want {
				t.Errorf("out = %q, want %q", out, tt.want)
			}
		})
	}
}

// TestSyntaxError: a broken script must fail before anything runs.
//
// The system shell reports this itself — bash exits 2 with a message on stderr
// naming the line — so unlike an embedded interpreter it IS an exit status, and
// tsk propagates the shell's own code rather than inventing one.
func TestSyntaxError(t *testing.T) {
	var stdout, stderr strings.Builder
	sh := Shell{Dir: t.TempDir(), Out: &stdout, Err: &stderr}

	err := sh.Run(context.Background(), "echo before\nif true; then")
	if err == nil {
		t.Fatal("Run = nil, want a syntax error")
	}
	if code := ExitCode(err); code == 0 {
		t.Errorf("ExitCode = 0, want non-zero")
	}
	// Note the real-shell behaviour: bash reads a -c string incrementally, so
	// commands before the broken construct DO run. An embedded interpreter that
	// parses the whole script first would have run nothing — a difference worth
	// knowing when a task's last line is malformed.
	if !strings.Contains(stderr.String(), "syntax error") {
		t.Errorf("stderr = %q, want it to mention a syntax error", stderr.String())
	}
}

func TestExitCodeHelper(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil is success", err: nil, want: 0},
		{name: "exit error carries its code", err: &ExitError{Code: 7}, want: 7},
		{name: "wrapped exit error", err: fmt.Errorf("task foo: %w", &ExitError{Code: 42}), want: 42},
		{name: "any other error is 1", err: errors.New("boom"), want: 1},
		{name: "cancellation is 1", err: context.Canceled, want: 1},
		{name: "zero-code exit error", err: &ExitError{Code: 0}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExitCode(tt.err); got != tt.want {
				t.Errorf("ExitCode = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestStateDoesNotLeakBetweenScripts: a Shell is a value, not a session. Two
// scripts run through the same Shell must not see each other's variables or
// `cd`.
func TestStateDoesNotLeakBetweenScripts(t *testing.T) {
	dir := t.TempDir()
	sh := Shell{Dir: dir}
	ctx := context.Background()

	if err := sh.Run(ctx, "leaked=yes\ncd /"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out, err := sh.Capture(ctx, "echo \"[${leaked:-}]\"")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if out != "[]" {
		t.Errorf("variable leaked into the next script: %q", out)
	}

	out, err = sh.Capture(ctx, "pwd")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !samePath(t, out, dir) {
		t.Errorf("cd leaked into the next script: pwd = %q, want %q", out, dir)
	}
}

// samePath compares two paths with symlinks resolved, because macOS temp
// directories live under a symlinked /var.
func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", a, err)
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", b, err)
	}
	return ra == rb
}
