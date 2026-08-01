package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// These tests drive Invoke (the top-level entry) against a real shell, exactly
// like run_test.go drives Run. Each step appends a word to order.txt, so the file
// read back is the ground-truth record of what ran and in what order.

func (f *fixture) invoke(name string, args []string, callVars map[string]string) error {
	f.t.Helper()
	return f.r.Invoke(context.Background(), name, args, callVars)
}

// steps builds a Cmds list of shell lines.
func steps(lines ...string) chorefile.Cmds {
	out := make(chorefile.Cmds, len(lines))
	for i, l := range lines {
		out[i] = chorefile.Cmd{Cmd: l}
	}
	return out
}

func TestBeforeAllRunsOnceBeforeTheTaskTree(t *testing.T) {
	f := newFixture(t, &chorefile.File{
		Lifecycle: &chorefile.Lifecycle{BeforeAll: steps("echo before >> order.txt")},
	}, map[string]*chorefile.Task{
		"work": {Cmds: steps("echo work >> order.txt"), Deps: chorefile.Deps{{Task: "sub"}}},
		"sub":  {Cmds: steps("echo sub >> order.txt")},
	})
	if err := f.invoke("work", nil, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	got := f.read("order.txt")
	if n := strings.Count(got, "before"); n != 1 {
		t.Fatalf("before_all must run exactly once, ran %d times:\n%s", n, got)
	}
	if !strings.HasPrefix(got, "before") {
		t.Fatalf("before_all must run before the task tree (incl. its deps):\n%s", got)
	}
}

// The headline reason lifecycle beats a per-task `deps:` entry: it runs even when
// the task itself is up to date and thus skipped. A dep would be skipped with it.
func TestBeforeAllRunsEvenWhenTheTaskIsUpToDate(t *testing.T) {
	f := newFixture(t, &chorefile.File{
		Lifecycle: &chorefile.Lifecycle{BeforeAll: steps("echo before >> order.txt")},
	}, map[string]*chorefile.Task{
		"build": {
			Cmds:      steps("echo built >> order.txt"),
			Sources:   []string{"src.txt"},
			Generates: []string{"out.txt"},
		},
	})
	f.write("src.txt", "x")
	f.write("out.txt", "y")

	// First run: not up to date → task runs and records its fingerprint.
	if err := f.invoke("build", nil, nil); err != nil {
		t.Fatalf("first invoke: %v\nstderr:\n%s", err, f.err)
	}
	// Wipe the log; sources are unchanged so the task is now up to date.
	if err := os.Remove(filepath.Join(f.dir, "order.txt")); err != nil {
		t.Fatal(err)
	}
	if err := f.invoke("build", nil, nil); err != nil {
		t.Fatalf("second invoke: %v\nstderr:\n%s", err, f.err)
	}
	got := f.read("order.txt")
	if strings.Contains(got, "built") {
		t.Fatalf("the task should have been skipped as up to date, but it ran:\n%s", got)
	}
	if !strings.Contains(got, "before") {
		t.Fatalf("before_all must still run when the task is skipped:\n%s", got)
	}
}

func TestBeforeAllFailureAbortsBeforeTaskAndAfterAll(t *testing.T) {
	f := newFixture(t, &chorefile.File{
		Lifecycle: &chorefile.Lifecycle{
			BeforeAll: steps("exit 3"),
			AfterAll:  steps("echo after >> order.txt"),
		},
	}, map[string]*chorefile.Task{
		"work": {Cmds: steps("echo work >> order.txt")},
	})
	if err := f.invoke("work", nil, nil); err == nil {
		t.Fatal("a failing before_all must fail the run")
	}
	if f.exists("order.txt") {
		t.Fatalf("a failed setup gate must run neither the task nor after_all:\n%s", f.read("order.txt"))
	}
}

func TestAfterAllRunsAfterTheTaskEvenOnFailure(t *testing.T) {
	f := newFixture(t, &chorefile.File{
		Lifecycle: &chorefile.Lifecycle{AfterAll: steps("echo after >> order.txt")},
	}, map[string]*chorefile.Task{
		"work": {Cmds: steps("echo work >> order.txt", "exit 1")},
	})
	if err := f.invoke("work", nil, nil); err == nil {
		t.Fatal("the failing task must still fail the run")
	}
	got := f.read("order.txt")
	if !strings.Contains(got, "work") || !strings.Contains(got, "after") {
		t.Fatalf("both the task and after_all should have run:\n%s", got)
	}
	if strings.Index(got, "after") < strings.Index(got, "work") {
		t.Fatalf("after_all must run AFTER the task:\n%s", got)
	}
}

func TestOnErrorRunsOnFailureNotOnSuccess(t *testing.T) {
	// failure → on_error runs.
	f := newFixture(t, &chorefile.File{
		Lifecycle: &chorefile.Lifecycle{OnError: steps("echo boom >> order.txt")},
	}, map[string]*chorefile.Task{
		"work": {Cmds: steps("exit 1")},
	})
	if err := f.invoke("work", nil, nil); err == nil {
		t.Fatal("expected the task to fail")
	}
	if strings.TrimSpace(f.read("order.txt")) != "boom" {
		t.Fatalf("on_error should have run once: %q", f.read("order.txt"))
	}

	// success → on_error does not run.
	g := newFixture(t, &chorefile.File{
		Lifecycle: &chorefile.Lifecycle{OnError: steps("echo boom >> order.txt")},
	}, map[string]*chorefile.Task{
		"work": {Cmds: steps("true")},
	})
	if err := g.invoke("work", nil, nil); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if g.exists("order.txt") {
		t.Fatalf("on_error must not run on success:\n%s", g.read("order.txt"))
	}
}

func TestHookSeesInvokedTaskNameAsTASK(t *testing.T) {
	f := newFixture(t, &chorefile.File{
		Lifecycle: &chorefile.Lifecycle{BeforeAll: steps("echo {{.TASK}} >> which.txt")},
	}, map[string]*chorefile.Task{
		"deploy": {Cmds: steps("true")},
	})
	if err := f.invoke("deploy", nil, nil); err != nil {
		t.Fatalf("invoke: %v\nstderr:\n%s", err, f.err)
	}
	if got := strings.TrimSpace(f.read("which.txt")); got != "deploy" {
		t.Fatalf("{{.TASK}} in a hook = %q, want %q", got, "deploy")
	}
}

func TestNoLifecycleDisablesHooks(t *testing.T) {
	f := newFixture(t, &chorefile.File{
		Lifecycle: &chorefile.Lifecycle{BeforeAll: steps("echo before >> order.txt")},
	}, map[string]*chorefile.Task{
		"work": {Cmds: steps("echo work >> order.txt")},
	})
	f.r.NoLifecycle = true
	if err := f.invoke("work", nil, nil); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if got := f.read("order.txt"); strings.Contains(got, "before") {
		t.Fatalf("--no-lifecycle must skip the hooks:\n%s", got)
	}
}

func TestInvokeWithoutLifecycleIsJustRun(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"work": {Cmds: steps("echo work >> order.txt")},
	})
	if err := f.invoke("work", nil, nil); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if got := strings.TrimSpace(f.read("order.txt")); got != "work" {
		t.Fatalf("with no lifecycle block, Invoke must behave exactly like Run: %q", got)
	}
}
