//go:build linux

package run

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// `confirm:` against a real terminal rather than a pipe, because the terminal is
// the whole of its contract: with one, the question is asked and the answer is a
// person's; without one, the task is refused. Linux only because opening a pty
// pair by hand is platform-specific — the refusal path, --yes and --dry are
// covered everywhere in confirm_test.go.
//
// Input is written ahead of the run. The terminal's line discipline holds it, so
// the read returns at once and a broken implementation fails the test instead of
// hanging it.
func TestConfirmReadsTheAnswerFromTheTerminal(t *testing.T) {
	for name, c := range map[string]struct {
		typed   string
		def     bool
		wantRun bool
	}{
		"yes":                     {"y\n", false, true},
		"the word":                {"YES\n", false, true},
		"no":                      {"n\n", false, false},
		"enter takes the no":      {"\n", false, false},
		"enter takes the yes":     {"\n", true, true},
		"nonsense then an answer": {"maybe\nyes\n", false, true},
		"padding is forgiven":     {"  y  \n", false, true},
		"a terminal that closed":  {"", false, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil, map[string]*chorefile.Task{
				"deploy": {
					Confirm: &chorefile.Confirm{Text: "Deploy?", Default: c.def},
					Cmds:    steps("touch body"),
				},
			})
			ptmx, tty := openPTYPair(t)
			f.r.Stdin = tty
			if c.typed != "" {
				if _, err := ptmx.WriteString(c.typed); err != nil {
					t.Fatalf("typing: %v", err)
				}
			} else {
				// Nothing to read and never will be: the far end is gone, which the
				// read sees as EOF. Not an answer, so not a yes.
				_ = ptmx.Close()
			}

			err := f.r.Run(t.Context(), "deploy", nil, nil)

			if c.wantRun {
				if err != nil {
					t.Fatalf("answered yes but: %v\nstderr: %s", err, f.err)
				}
				if !f.exists("body") {
					t.Error("answered yes, body did not run")
				}
				return
			}
			var declined *DeclineError
			if !errors.As(err, &declined) {
				t.Fatalf("got %T %v, want a decline", err, err)
			}
			if f.exists("body") {
				t.Error("answered no, body ran anyway")
			}
		})
	}
}

// The prompt says which way Enter goes, so nobody has to read the manual to
// know what the key they are about to press means.
func TestConfirmPromptShowsWhereEnterGoes(t *testing.T) {
	for _, c := range []struct {
		def  bool
		want string
	}{{false, "Deploy? [y/N]"}, {true, "Deploy? [Y/n]"}} {
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"deploy": {Confirm: &chorefile.Confirm{Text: "Deploy?", Default: c.def}, Cmds: steps("true")},
		})
		ptmx, tty := openPTYPair(t)
		f.r.Stdin = tty
		if _, err := ptmx.WriteString("y\n"); err != nil {
			t.Fatal(err)
		}

		if err := f.r.Run(t.Context(), "deploy", nil, nil); err != nil {
			t.Fatal(err)
		}

		// On stderr, never stdout: a confirmation must not land in piped output.
		if !strings.Contains(f.err.String(), c.want) {
			t.Errorf("stderr = %q, want %q", f.err, c.want)
		}
		if f.out.String() != "" {
			t.Errorf("the question reached stdout: %q", f.out)
		}
	}
}

// An unreadable answer is asked again rather than guessed at.
func TestConfirmReAsksWhatIsNeitherYesNorNo(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {Confirm: &chorefile.Confirm{Text: "Deploy?"}, Cmds: steps("touch body")},
	})
	ptmx, tty := openPTYPair(t)
	f.r.Stdin = tty
	if _, err := ptmx.WriteString("what\nn\n"); err != nil {
		t.Fatal(err)
	}

	err := f.r.Run(t.Context(), "deploy", nil, nil)

	var declined *DeclineError
	if !errors.As(err, &declined) {
		t.Fatalf("got %v", err)
	}
	if got := strings.Count(f.err.String(), "Deploy? [y/N]"); got != 2 {
		t.Errorf("asked %d times, want 2:\n%s", got, f.err)
	}
	if !strings.Contains(f.err.String(), "Please answer y or n.") {
		t.Errorf("stderr = %q", f.err)
	}
}

// `show:` is the reason the question is asked here and not in a precondition:
// it runs something, and the answer depends on what it printed.
func TestConfirmShowPrintsBeforeAsking(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {
			Confirm: &chorefile.Confirm{Text: "Deploy to {{.STACK}}?", Show: "echo '+ 3 to create' '{{.STACK}}'"},
			Vars:    map[string]chorefile.Var{"STACK": {Value: "production"}},
			Cmds:    steps("touch body"),
		},
	})
	ptmx, tty := openPTYPair(t)
	f.r.Stdin = tty
	if _, err := ptmx.WriteString("y\n"); err != nil {
		t.Fatal(err)
	}

	if err := f.r.Run(t.Context(), "deploy", nil, nil); err != nil {
		t.Fatalf("%v\nstderr: %s", err, f.err)
	}

	shown := strings.Index(f.err.String(), "3 to create production")
	asked := strings.Index(f.err.String(), "Deploy to production?")
	if shown < 0 || asked < 0 || shown > asked {
		t.Errorf("want the diff before the question:\n%s", f.err)
	}
}

// A `show:` that fails means the thing the decision rests on never appeared.
// Asking anyway would collect an answer to a different question.
func TestConfirmShowFailureFailsTheTask(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {
			Confirm: &chorefile.Confirm{Text: "Deploy?", Show: "echo broken >&2; exit 4"},
			Cmds:    steps("touch body"),
		},
	})
	ptmx, tty := openPTYPair(t)
	f.r.Stdin = tty
	if _, err := ptmx.WriteString("y\n"); err != nil {
		t.Fatal(err)
	}

	err := f.r.Run(t.Context(), "deploy", nil, nil)

	if err == nil || !strings.Contains(err.Error(), "nothing to decide on") {
		t.Fatalf("got %v", err)
	}
	var declined *DeclineError
	if errors.As(err, &declined) {
		t.Error("reported as a decline; nobody declined anything")
	}
	if ExitCode(err) != 4 {
		t.Errorf("exit %d, want the command's 4", ExitCode(err))
	}
	if f.exists("body") {
		t.Error("body ran")
	}
}

// Asked once per invocation, by task and rendered question: a confirm on a task
// two others call is one question, not two.
func TestConfirmIsAskedOncePerRun(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"all":    {Cmds: chorefile.Cmds{{Task: "deploy"}, {Task: "deploy"}}},
		"deploy": {Confirm: &chorefile.Confirm{Text: "Deploy?"}, Cmds: steps("echo ran >> order.txt")},
	})
	ptmx, tty := openPTYPair(t)
	f.r.Stdin = tty
	if _, err := ptmx.WriteString("y\n"); err != nil {
		t.Fatal(err)
	}

	if err := f.r.Run(t.Context(), "all", nil, nil); err != nil {
		t.Fatalf("%v\nstderr: %s", err, f.err)
	}

	if got := strings.Count(f.err.String(), "Deploy? [y/N]"); got != 1 {
		t.Errorf("asked %d times, want 1:\n%s", got, f.err)
	}
	if got := strings.Count(f.read("order.txt"), "ran"); got != 2 {
		t.Errorf("the task ran %d times, want 2", got)
	}
}

// A different rendered question is a different question, even on the same task.
func TestConfirmRememberedPerQuestionNotPerTask(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"all": {Cmds: chorefile.Cmds{
			{Task: "deploy", Vars: map[string]chorefile.Var{"STACK": {Value: "staging"}}},
			{Task: "deploy", Vars: map[string]chorefile.Var{"STACK": {Value: "production"}}},
		}},
		"deploy": {
			Vars:    map[string]chorefile.Var{"STACK": {Value: "none"}},
			Confirm: &chorefile.Confirm{Text: "Deploy {{.STACK}}?"},
			Cmds:    steps("echo {{.STACK}} >> order.txt"),
		},
	})
	ptmx, tty := openPTYPair(t)
	f.r.Stdin = tty
	if _, err := ptmx.WriteString("y\ny\n"); err != nil {
		t.Fatal(err)
	}

	if err := f.r.Run(t.Context(), "all", nil, nil); err != nil {
		t.Fatalf("%v\nstderr: %s", err, f.err)
	}

	if !strings.Contains(f.err.String(), "Deploy staging?") || !strings.Contains(f.err.String(), "Deploy production?") {
		t.Errorf("both stacks should have been asked about:\n%s", f.err)
	}
}

// openPTYPair returns the controller end and the terminal end of a fresh pty.
// The terminal end stands in for chore's stdin; the controller end is the
// keyboard.
func openPTYPair(t *testing.T) (ptmx, tty *os.File) {
	t.Helper()
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals here: %v", err)
	}
	t.Cleanup(func() { _ = ptmx.Close() })
	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlocking the pty: %v", err)
	}
	n, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("naming the pty: %v", err)
	}
	tty, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("opening the terminal end: %v", err)
	}
	t.Cleanup(func() { _ = tty.Close() })
	return ptmx, tty
}
