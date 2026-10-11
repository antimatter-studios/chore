package run

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

func TestRefusalDoesNotEnterTaskButFinishesRun(t *testing.T) {
	f := newFixture(t, &chorefile.File{Lifecycle: &chorefile.Lifecycle{
		OnFailure: steps("echo failed >> order.txt"),
		AfterAll:  steps("echo finished >> order.txt"),
	}}, map[string]*chorefile.Task{
		"work": {
			Preconditions: []chorefile.Precondition{
				{Sh: "echo hidden; echo hidden >&2; exit 7", Msg: "Commit first"},
				{Sh: "echo later >> order.txt"},
			},
			Before: steps("echo before >> order.txt"), Deps: depsOn("dep"),
			Cmds:      steps("echo body >> order.txt"),
			OnSuccess: steps("echo success >> order.txt"),
			OnFailure: steps("echo failure >> order.txt"),
			After:     steps("echo after >> order.txt"),
		},
		"dep": {Cmds: steps("echo dep >> order.txt")},
	})
	err := f.invoke("work", nil, nil)
	if ExitCode(err) != 7 || err.Error() != "work: Commit first" {
		t.Fatalf("refusal = %v, code %d", err, ExitCode(err))
	}
	if got := strings.TrimSpace(f.read("order.txt")); got != "failed\nfinished" {
		t.Fatalf("refused task entered: %q", got)
	}
	if f.out.String() != "" || f.err.String() != "" {
		t.Fatalf("check leaked output: stdout=%q stderr=%q", f.out, f.err)
	}
}

func TestPreconditionsRemainActiveUnderSkipsAndSuppression(t *testing.T) {
	for _, mode := range []string{"up to date", "force", "no lifecycle", "child hooks"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, nil, map[string]*chorefile.Task{
				"parent": {ChildHooks: no(), Cmds: chorefile.Cmds{{Task: "work"}}},
				"work": {
					Status:        []string{"true"},
					Preconditions: []chorefile.Precondition{{Sh: "exit 3"}},
					Before:        steps("touch before"), Cmds: steps("touch body"),
				},
			})
			f.r.Force = mode == "force"
			f.r.NoLifecycle = mode == "no lifecycle"
			name := "work"
			if mode == "child hooks" {
				name = "parent"
			}
			if err := f.run(name, nil, nil); ExitCode(err) != 3 {
				t.Fatalf("check bypassed: %v", err)
			}
			if f.exists("before") || f.exists("body") {
				t.Fatal("refused task entered")
			}
		})
	}
}

func TestRequiresAndPlatformPrecedePreconditions(t *testing.T) {
	for _, platformSkip := range []bool{false, true} {
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"work": {Requires: []string{"CHORE_PRECONDITION_MISSING"},
				Preconditions: []chorefile.Precondition{{Sh: "touch checked; echo {{.CHORE_PRECONDITION_MISSING}}"}}},
		})
		t.Setenv("CHORE_PRECONDITION_MISSING", "")
		if platformSkip {
			f.file.Tasks["work"].Platforms = []string{"not-a-platform"}
		}
		err := f.run("work", nil, nil)
		if platformSkip && err != nil {
			t.Fatalf("platform skipped task refused: %v", err)
		}
		if !platformSkip && (err == nil || !strings.Contains(err.Error(), "required variable")) {
			t.Fatalf("requires did not run first: %v", err)
		}
		if f.exists("checked") {
			t.Fatal("precondition ran too soon")
		}
	}
}

func TestPreconditionsPassBeforePreparationAndUseTaskScope(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"work": {Dir: "sub", Args: chorefile.Args{{Name: "version"}},
			Preconditions: []chorefile.Precondition{{Sh: "test '{{.VERSION}}' = 2; echo check >> order.txt"}},
			Before:        steps("echo before >> order.txt"), Cmds: steps("echo body >> order.txt")},
	})
	f.mkdir("sub")
	f.mustRun("work", []string{"2"}, nil)
	if got := strings.TrimSpace(f.read("sub/order.txt")); got != "check\nbefore\nbody" {
		t.Fatalf("order = %q", got)
	}
	f.file.Tasks["work"].Preconditions = []chorefile.Precondition{{Sh: "exit 9", Msg: "Version {{.VERSION}} refused"}}
	if err := f.run("work", []string{"2"}, nil); err == nil || err.Error() != "work: Version 2 refused" {
		t.Fatalf("message not rendered: %v", err)
	}
}

func TestTaskPreconditionsShareResultsAndKeepVariablesSeparate(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"all":   {Deps: depsOn("a", "b", "c")},
		"a":     {Preconditions: []chorefile.Precondition{{Task: "check", Vars: vars("VALUE", "a")}}},
		"b":     {Preconditions: []chorefile.Precondition{{Task: "check", Vars: vars("VALUE", "a")}}},
		"c":     {Preconditions: []chorefile.Precondition{{Task: "check", Vars: vars("VALUE", "b")}}},
		"check": {Cmds: steps("echo {{.VALUE}} >> checks.txt; echo hidden; echo hidden >&2")},
	})
	f.mustRun("all", nil, nil)
	got := strings.Fields(f.read("checks.txt"))
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("shared checks = %q, want a and b once each", got)
	}
	if f.out.String() != "" || f.err.String() != "" {
		t.Fatalf("task check leaked output: %s %s", f.out, f.err)
	}
}

func TestTaskPreconditionFailuresKeepExitStatusAndDefaultMessage(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"work":  {Preconditions: []chorefile.Precondition{{Task: "check"}}},
		"check": {Before: steps("echo check; echo warning >&2"), Cmds: steps("exit 8"), After: steps("echo finish")},
	})
	err := f.mustFail("work", nil, nil)
	if ExitCode(err) != 8 || err.Error() != "work: precondition failed: task check" {
		t.Fatalf("refusal = %v, code %d", err, ExitCode(err))
	}
	if f.out.String() != "" || f.err.String() != "" {
		t.Fatal("check output was not quiet")
	}
}

func TestPreconditionDryRunDoesNotExecuteChecks(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"work":  {Preconditions: []chorefile.Precondition{{Sh: "touch shell"}, {Task: "check"}}, Cmds: steps("touch body")},
		"check": {Cmds: steps("touch task")},
	})
	f.r.DryRun = true
	f.mustRun("work", nil, nil)
	if got := f.out.String(); got != "check: touch shell\ncheck: task check\ntouch body\n" {
		t.Fatalf("dry output = %q", got)
	}
	if f.exists("shell") || f.exists("task") || f.exists("body") {
		t.Fatal("dry run executed work")
	}
}

func TestPreconditionTimeoutRefusesWithoutTaskFinishHooks(t *testing.T) {
	for _, taskCheck := range []bool{false, true} {
		p := chorefile.Precondition{Sh: "sleep 20"}
		if taskCheck {
			p = chorefile.Precondition{Task: "check"}
		}
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"work": {Timeout: chorefile.Duration(150 * time.Millisecond), Preconditions: []chorefile.Precondition{p},
				Before: steps("touch before"), After: steps("touch after"), OnFailure: steps("touch failure")},
			"check": {Cmds: steps("sleep 20")},
		})
		err := f.run("work", nil, nil)
		var timeout *TimeoutError
		if !errors.As(err, &timeout) || ExitCode(err) != 124 {
			t.Fatalf("hang did not time out: %v", err)
		}
		if f.exists("before") || f.exists("after") || f.exists("failure") {
			t.Fatal("timed out precondition entered owning task")
		}
	}
}

func TestPreconditionWaitHonorsCallerCancellation(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{"check": {}})
	f.r.checks = map[string]*checkEntry{"check\x00null": {done: make(chan struct{})}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.r.checkTask(ctx, "check", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting check ignored cancellation: %v", err)
	}
}

func TestPreconditionsAreInsideRunOnce(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"all":  {Cmds: chorefile.Cmds{{Task: "work"}, {Task: "work"}}},
		"work": {Run: "once", Preconditions: []chorefile.Precondition{{Sh: "echo check >> order.txt"}}, Cmds: steps("echo body >> order.txt")},
	})
	f.mustRun("all", nil, nil)
	if got := strings.TrimSpace(f.read("order.txt")); got != "check\nbody" {
		t.Fatalf("once task rechecked: %q", got)
	}
}
