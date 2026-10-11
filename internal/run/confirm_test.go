package run

import (
	"errors"
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// The rule the whole feature rests on: with nobody to ask, a task that asks is
// REFUSED. Never assumed to be a yes — a pipeline that grows a confirm fails on
// its next run rather than quietly going ahead, which is the one outcome a
// confirmation exists to prevent. The fixture's stdin is not a terminal, so
// every test here that does not set Yes is this case.
func TestConfirmWithNobodyToAskRefuses(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {
			Confirm: &chorefile.Confirm{Text: "Deploy to {{.STACK}}?"},
			Vars:    map[string]chorefile.Var{"STACK": {Value: "production"}},
			Cmds:    steps("touch body"),
		},
	})

	err := f.mustFail("deploy", nil, nil)

	var declined *DeclineError
	if !errors.As(err, &declined) {
		t.Fatalf("got %T %v, want a DeclineError", err, err)
	}
	// The log has to say what was being asked, or nobody can act on it.
	if !strings.Contains(err.Error(), "Deploy to production?") || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("refusal = %q", err)
	}
	if ExitCode(err) != 1 {
		t.Errorf("exit %d, want 1", ExitCode(err))
	}
	if f.exists("body") {
		t.Error("an unanswered task ran its commands")
	}
}

func TestConfirmAnsweredInAdvanceRuns(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {Confirm: &chorefile.Confirm{Text: "Deploy?"}, Cmds: steps("touch body")},
	})
	f.r.Yes = true

	f.mustRun("deploy", nil, nil)

	if !f.exists("body") {
		t.Error("--yes did not run the task")
	}
	// Silent by default: --yes on a run of twenty tasks must not narrate.
	if f.err.String() != "" {
		t.Errorf("stderr = %q", f.err)
	}
}

// -v says which questions were answered without being asked, because a run that
// skipped a confirmation should be able to say so afterwards.
func TestConfirmVerboseReportsWhatYesAnswered(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {Confirm: &chorefile.Confirm{Text: "Deploy?"}, Cmds: steps("true")},
	})
	f.r.Yes, f.r.Verbose = true, true

	f.mustRun("deploy", nil, nil)

	if !strings.Contains(f.err.String(), "answered yes in advance: Deploy?") {
		t.Errorf("stderr = %q", f.err)
	}
}

// --dry lists the question exactly as it lists `check:` lines, and asks nothing
// — so a dry run never blocks, with or without a terminal.
func TestConfirmDryRunListsTheQuestion(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {Confirm: &chorefile.Confirm{Text: "Deploy?"}, Cmds: steps("touch body")},
	})
	f.r.DryRun = true

	f.mustRun("deploy", nil, nil)

	if !strings.Contains(f.out.String(), "confirm: Deploy?") {
		t.Errorf("stdout = %q", f.out)
	}
	if f.exists("body") {
		t.Error("--dry ran the body")
	}
}

// --force is about up-to-date checks and --no-lifecycle about hooks. Neither is
// an answer, and neither may act as one.
func TestConfirmIsNotBypassedByForceOrNoLifecycle(t *testing.T) {
	for _, mode := range []string{"force", "no lifecycle", "child hooks"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, nil, map[string]*chorefile.Task{
				"parent": {ChildHooks: no(), Cmds: chorefile.Cmds{{Task: "deploy"}}},
				"deploy": {Confirm: &chorefile.Confirm{Text: "Deploy?"}, Cmds: steps("touch body")},
			})
			f.r.Force = mode == "force"
			f.r.NoLifecycle = mode == "no lifecycle"
			name := "deploy"
			if mode == "child hooks" {
				name = "parent"
			}

			f.mustFail(name, nil, nil)

			if f.exists("body") {
				t.Errorf("%s answered the question", mode)
			}
		})
	}
}

// A task that is up to date is not going to do anything, so there is nothing to
// confirm. The question sits after the up-to-date check for exactly this.
func TestConfirmSkippedWhenTheTaskIsUpToDate(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {
			Status:  []string{"true"},
			Confirm: &chorefile.Confirm{Text: "Deploy?"},
			Cmds:    steps("touch body"),
		},
	})

	f.mustRun("deploy", nil, nil)

	if f.exists("body") {
		t.Error("an up-to-date task ran its body")
	}
}

// Preconditions run first and `deps:` before the question, so by the time
// anyone is asked, everything that could refuse the task has, and everything
// worth looking at has been built. That ordering is the reason this is not a
// precondition.
func TestConfirmComesAfterChecksAndDeps(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {
			Preconditions: []chorefile.Precondition{{Sh: "echo check >> order.txt"}},
			Before:        steps("echo before >> order.txt"),
			Deps:          depsOn("dep"),
			Confirm:       &chorefile.Confirm{Text: "Deploy?"},
			Cmds:          steps("echo body >> order.txt"),
		},
		"dep": {Cmds: steps("echo dep >> order.txt")},
	})

	f.mustFail("deploy", nil, nil)

	if got := strings.TrimSpace(f.read("order.txt")); got != "check\nbefore\ndep" {
		t.Errorf("order = %q, want the question asked after check, before and dep, and no body", got)
	}
}

func TestConfirmRenderErrors(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {Confirm: &chorefile.Confirm{Text: "{{.NOPE"}, Cmds: steps("true")},
	})
	if err := f.mustFail("deploy", nil, nil); !strings.Contains(err.Error(), "rendering confirm") {
		t.Errorf("got %v", err)
	}
}
