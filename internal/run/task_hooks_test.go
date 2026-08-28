package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// Per-task hooks, driven the same way lifecycle_test.go drives the file-level
// ones: every step appends a word to order.txt, and the file read back is the
// only evidence a user ever gets about what ran and when.

func no() *bool { b := false; return &b }

func yes() *bool { b := true; return &b }

func TestTaskHooksRunInOrderAroundTheBody(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"work": {
			Before:    steps("echo before >> order.txt"),
			Cmds:      steps("echo body >> order.txt"),
			OnSuccess: steps("echo success >> order.txt"),
			After:     steps("echo after >> order.txt"),
		},
	})
	if err := f.invoke("work", nil, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	if got := strings.Fields(f.read("order.txt")); strings.Join(got, " ") != "before body success after" {
		t.Fatalf("order.txt = %q, want %q", strings.Join(got, " "), "before body success after")
	}
}

// The defers unwind BEFORE the outcome branch, so `after` is a finishing step
// that runs once the thing it is finishing is already down.
func TestDefersUnwindBeforeTheOutcomeHooks(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"work": {
			Cmds: chorefile.Cmds{
				{Cmd: "echo up >> order.txt"},
				{Cmd: "echo down >> order.txt", Defer: true},
				{Cmd: "echo body >> order.txt"},
			},
			OnSuccess: steps("echo success >> order.txt"),
			After:     steps("echo after >> order.txt"),
		},
	})
	if err := f.invoke("work", nil, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	want := "up body down success after"
	if got := strings.Join(strings.Fields(f.read("order.txt")), " "); got != want {
		t.Fatalf("order.txt = %q, want %q", got, want)
	}
}

// after runs IN ADDITION to the outcome hook, not instead of it — the reading
// most likely to be assumed wrongly, in both directions.
func TestAfterRunsAlongsideTheOutcomeHookOnBothPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"success", "true", "success after"},
		{"failure", "exit 1", "failure after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
				"work": {
					Cmds:      steps(tc.body),
					OnSuccess: steps("echo success >> order.txt"),
					OnFailure: steps("echo failure >> order.txt"),
					After:     steps("echo after >> order.txt"),
				},
			})
			err := f.invoke("work", nil, nil)
			if tc.name == "failure" && err == nil {
				t.Fatal("a failing task must still fail the run")
			}
			if tc.name == "success" && err != nil {
				t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
			}
			if got := strings.Join(strings.Fields(f.read("order.txt")), " "); got != tc.want {
				t.Fatalf("order.txt = %q, want %q", got, tc.want)
			}
		})
	}
}

// before is a gate, and its failure is an outcome like any other: cmds do not
// run, and on_failure fires for the gate.
func TestBeforeGatesTheBodyAndFiresOnFailure(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"work": {
			Before:    steps("echo gate >> order.txt", "exit 3"),
			Cmds:      steps("echo body >> order.txt"),
			OnFailure: steps("echo failure >> order.txt"),
			After:     steps("echo after >> order.txt"),
		},
	})
	err := f.invoke("work", nil, nil)
	if err == nil {
		t.Fatal("a failing before must fail the task")
	}
	if code := ExitCode(err); code != 3 {
		t.Fatalf("ExitCode = %d, want 3 — the gate's own status", code)
	}
	got := strings.Join(strings.Fields(f.read("order.txt")), " ")
	if strings.Contains(got, "body") {
		t.Fatalf("a failed gate must not let the body run:\n%s", got)
	}
	if got != "gate failure after" {
		t.Fatalf("order.txt = %q, want %q", got, "gate failure after")
	}
}

// The one thing on_failure must not be able to do.
func TestOutcomeHooksCannotChangeTheExitStatus(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"work": {
			Cmds:      steps("exit 7"),
			OnFailure: steps("true"), // a hook that "handles" it
			After:     steps("true"),
		},
	})
	err := f.invoke("work", nil, nil)
	if err == nil {
		t.Fatal("a hook must not be able to swallow the failure")
	}
	if code := ExitCode(err); code != 7 {
		t.Fatalf("ExitCode = %d, want 7 — the task's own status", code)
	}

	// And a failing best-effort hook must not invent a failure either.
	g := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"work": {Cmds: steps("true"), After: steps("exit 9")},
	})
	if err := g.invoke("work", nil, nil); err != nil {
		t.Fatalf("a failing after must not fail the task: %v", err)
	}
	if !strings.Contains(g.err.String(), "after") {
		t.Fatalf("a failing after must be reported on stderr:\n%s", g.err)
	}
}

func TestAfterSeesTheExitCode(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"true", "0"},
		{"exit 7", "7"},
	} {
		f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
			"work": {
				Cmds:  steps(tc.body),
				After: steps("echo {{.EXIT_CODE}} >> tmpl.txt", "echo $EXIT_CODE >> env.txt"),
			},
		})
		_ = f.invoke("work", nil, nil)
		if got := strings.TrimSpace(f.read("tmpl.txt")); got != tc.want {
			t.Errorf("{{.EXIT_CODE}} after %q = %q, want %q", tc.body, got, tc.want)
		}
		// The template and the environment must agree, or a hook reads one value
		// and the script it calls reads another.
		if got := strings.TrimSpace(f.read("env.txt")); got != tc.want {
			t.Errorf("$EXIT_CODE after %q = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// A hook runs in the TASK's scope, so it can read the arguments the task was
// called with. This is the difference from a file-level hook.
func TestTaskHookSeesTheTasksOwnVariables(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"build": {
			Args:  chorefile.Args{{Name: "target"}},
			Cmds:  steps("true"),
			After: steps("echo {{.TARGET}} >> which.txt"),
		},
	})
	if err := f.invoke("build", []string{"ext4"}, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	if got := strings.TrimSpace(f.read("which.txt")); got != "ext4" {
		t.Fatalf("{{.TARGET}} in a task hook = %q, want %q", got, "ext4")
	}
}

// A task's hooks are the task's, so they fire wherever it runs — including as
// somebody else's dependency or `- task:` step.
func TestTaskHooksFireWhenInvokedAsASubtask(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"top": {
			Deps: chorefile.Deps{{Task: "dep"}},
			Cmds: chorefile.Cmds{{Task: "sub"}},
		},
		"dep": {Cmds: steps("true"), After: steps("echo dep-after >> order.txt")},
		"sub": {Cmds: steps("true"), After: steps("echo sub-after >> order.txt")},
	})
	if err := f.invoke("top", nil, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	got := f.read("order.txt")
	if !strings.Contains(got, "dep-after") {
		t.Errorf("a dependency's hooks must fire:\n%s", got)
	}
	if !strings.Contains(got, "sub-after") {
		t.Errorf("a `- task:` step's hooks must fire:\n%s", got)
	}
}

// The headline reason `before` is not a slower spelling of `deps:` — it is not
// the task's prerequisite, so it runs even when the task is skipped.
func TestTaskHooksRunEvenWhenTheTaskIsUpToDate(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"build": {
			Before:    steps("echo before >> order.txt"),
			Cmds:      steps("echo built >> order.txt"),
			After:     steps("echo after >> order.txt"),
			Sources:   []string{"src.txt"},
			Generates: []string{"out.txt"},
		},
	})
	f.write("src.txt", "x")
	f.write("out.txt", "y")
	if err := f.invoke("build", nil, nil); err != nil {
		t.Fatalf("first invoke: %v\nstderr:\n%s", err, f.err)
	}
	if err := os.Remove(filepath.Join(f.dir, "order.txt")); err != nil {
		t.Fatal(err)
	}
	if err := f.invoke("build", nil, nil); err != nil {
		t.Fatalf("second invoke: %v\nstderr:\n%s", err, f.err)
	}
	got := strings.Join(strings.Fields(f.read("order.txt")), " ")
	if strings.Contains(got, "built") {
		t.Fatalf("the task should have been skipped as up to date:\n%s", got)
	}
	if got != "before after" {
		t.Fatalf("order.txt = %q, want %q — hooks are not prerequisites", got, "before after")
	}
}

func TestNoLifecycleSuppressesTaskHooksButNotDepsOrDefer(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"work": {
			Deps: chorefile.Deps{{Task: "dep"}},
			Cmds: chorefile.Cmds{
				{Cmd: "echo cleanup >> order.txt", Defer: true},
				{Cmd: "echo body >> order.txt"},
			},
			Before: steps("echo before >> order.txt"),
			After:  steps("echo after >> order.txt"),
		},
		"dep": {Cmds: steps("echo dep >> order.txt")},
	})
	f.r.NoLifecycle = true
	if err := f.invoke("work", nil, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	got := f.read("order.txt")
	for _, gone := range []string{"before", "after"} {
		if strings.Contains(got, gone) {
			t.Errorf("--no-lifecycle must suppress %q:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"dep", "body", "cleanup"} {
		if !strings.Contains(got, kept) {
			t.Errorf("--no-lifecycle must leave %q alone:\n%s", kept, got)
		}
	}
}

// ---------- child_hooks ----------

// The driving case: a coordinator sweeps once for the whole tree instead of
// letting every task in it sweep for itself. Its OWN hooks still run.
func TestChildHooksFalseSuppressesTheSubtreeButNotTheDeclaringTask(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"all": {
			ChildHooks: no(),
			Deps:       chorefile.Deps{{Task: "prep"}},
			Cmds:       chorefile.Cmds{{Task: "driver"}},
			After:      steps("echo mine >> order.txt"),
		},
		"prep":   {Cmds: steps("true"), After: steps("echo prep-after >> order.txt")},
		"driver": {Cmds: chorefile.Cmds{{Task: "staticlib"}}, After: steps("echo driver-after >> order.txt")},
		// Two levels down — the depth a one-level suppression would miss, and where
		// the hook that actually matters usually lives.
		"staticlib": {Cmds: steps("true"), After: steps("echo lib-after >> order.txt")},
	})
	if err := f.invoke("all", nil, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	got := strings.Join(strings.Fields(f.read("order.txt")), " ")
	if got != "mine" {
		t.Fatalf("order.txt = %q, want %q — only the coordinator's own hook runs", got, "mine")
	}
}

// A dep is just a task invocation, so there is no second rule for it. Asserted
// on its own because it is the case a per-call-site flag could not have reached.
func TestChildHooksFalseReachesDepsAtEveryDepth(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"all":  {ChildHooks: no(), Deps: chorefile.Deps{{Task: "mid"}}, Cmds: steps("true")},
		"mid":  {Deps: chorefile.Deps{{Task: "leaf"}}, Cmds: steps("true"), After: steps("echo mid >> order.txt")},
		"leaf": {Cmds: steps("true"), After: steps("echo leaf >> order.txt")},
	})
	if err := f.invoke("all", nil, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	if f.exists("order.txt") {
		t.Fatalf("hooks reached through deps must be suppressed at every depth:\n%s", f.read("order.txt"))
	}
}

// Suppression is the coordinator's statement about a tree it owns, so a task
// inside that tree cannot lift it — otherwise the guarantee is not readable
// where it is written.
func TestAChildCannotOptBackIn(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"all": {ChildHooks: no(), Cmds: chorefile.Cmds{{Task: "mid"}}},
		// `child_hooks: true` is the explicit opt-in, and it does not work from
		// inside a suppressed subtree — neither for mid nor for what it calls.
		"mid":  {ChildHooks: yes(), Cmds: chorefile.Cmds{{Task: "leaf"}}, After: steps("echo mid >> order.txt")},
		"leaf": {Cmds: steps("true"), After: steps("echo leaf >> order.txt")},
	})
	if err := f.invoke("all", nil, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	if f.exists("order.txt") {
		t.Fatalf("a child must not be able to re-enable its own hooks:\n%s", f.read("order.txt"))
	}
}

// What makes deep suppression safe: it silences advice, never a teardown that
// pairs with something already brought up.
func TestChildHooksFalseNeverSuppressesDefer(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"all": {ChildHooks: no(), Cmds: chorefile.Cmds{{Task: "mid"}}},
		"mid": {Cmds: chorefile.Cmds{{Task: "leaf"}}},
		"leaf": {
			Cmds: chorefile.Cmds{
				{Cmd: "echo cleanup >> order.txt", Defer: true},
				{Cmd: "echo body >> order.txt"},
			},
			After: steps("echo after >> order.txt"),
		},
	})
	if err := f.invoke("all", nil, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	got := strings.Join(strings.Fields(f.read("order.txt")), " ")
	if got != "body cleanup" {
		t.Fatalf("order.txt = %q, want %q — defer survives, the hook does not", got, "body cleanup")
	}
}

// child_hooks says nothing about the file-level block, which is per invocation
// and not part of anybody's subtree.
func TestChildHooksFalseLeavesTheFileLevelHooksAlone(t *testing.T) {
	f := newFixture(t, &chorefile.File{
		Lifecycle: &chorefile.Lifecycle{AfterAll: steps("echo after_all >> order.txt")},
	}, map[string]*chorefile.Task{
		"all": {ChildHooks: no(), Cmds: chorefile.Cmds{{Task: "leaf"}}},
		"leaf": {
			Cmds:  steps("true"),
			After: steps("echo leaf >> order.txt"),
		},
	})
	if err := f.invoke("all", nil, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	got := strings.Join(strings.Fields(f.read("order.txt")), " ")
	if got != "after_all" {
		t.Fatalf("order.txt = %q, want %q", got, "after_all")
	}
}

// A task marked `run: once` runs once, and so do its hooks — they are part of
// that one run, not of each reference to it.
func TestRunOnceFiresTheHooksOnce(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"top": {Cmds: chorefile.Cmds{{Task: "shared"}, {Task: "shared"}}},
		"shared": {
			Run:   "once",
			Cmds:  steps("true"),
			After: steps("echo after >> order.txt"),
		},
	})
	if err := f.invoke("top", nil, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	if n := strings.Count(f.read("order.txt"), "after"); n != 1 {
		t.Fatalf("a `run: once` task's hooks must fire once, fired %d times:\n%s", n, f.read("order.txt"))
	}
}
