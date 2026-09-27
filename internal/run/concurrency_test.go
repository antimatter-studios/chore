package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// These tests take real locks on real files. XDG_RUNTIME_DIR is pointed at a
// temp directory for each one, so a test never queues behind the developer's own
// `chore check` — and two tests never queue behind each other.

func inItsOwnLockDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("CHORE_HELD", "")
}

// The headline: two tasks in one group do not run at the same time. Each writes
// a line on the way in and on the way out, so an overlap is visible as
// "in,in,out,out" rather than having to be inferred from a clock.
func TestConcurrencyRunsOneAtATime(t *testing.T) {
	inItsOwnLockDir(t)
	log := filepath.Join(t.TempDir(), "order")
	body := func(who string) chorefile.Cmds {
		return steps(
			"echo in-"+who+" >> "+log,
			"sleep 0.3",
			"echo out-"+who+" >> "+log,
		)
	}
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"one": {Concurrency: "cpu", Cmds: body("one")},
		"two": {Concurrency: "cpu", Cmds: body("two")},
	})

	var wg sync.WaitGroup
	for _, name := range []string{"one", "two"} {
		wg.Add(1)
		go func(n string) { defer wg.Done(); f.run(n, nil, nil) }(name)
		time.Sleep(20 * time.Millisecond) // so "one" is reliably first to the door
	}
	wg.Wait()

	got := readLines(t, log)
	if len(got) != 4 {
		t.Fatalf("lines = %q, want four", got)
	}
	// Whichever went first, it must have come out before the other went in.
	if !strings.HasPrefix(got[1], "out-") {
		t.Fatalf("the two overlapped: %q", got)
	}
	if got[0][3:] != got[1][4:] {
		t.Fatalf("a task let go of the lock before it finished: %q", got)
	}
}

// And the other half, which is what keeps the feature from being a global stop
// switch: a task in a different group does not wait at all.
func TestADifferentGroupDoesNotWait(t *testing.T) {
	inItsOwnLockDir(t)
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"slow": {Concurrency: "cpu", Cmds: steps("sleep 0.5")},
		"web":  {Concurrency: "port:5173", Cmds: steps("true")},
	})
	go f.run("slow", nil, nil)
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	f.mustRun("web", nil, nil)
	if waited := time.Since(start); waited > 200*time.Millisecond {
		t.Fatalf("a task in another group waited %v", waited)
	}
}

// A task with no group at all is untouched — the common case, and the one that
// must cost nothing.
func TestNoGroupNeverWaits(t *testing.T) {
	inItsOwnLockDir(t)
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"slow":  {Concurrency: "cpu", Cmds: steps("sleep 0.5")},
		"plain": {Cmds: steps("true")},
	})
	go f.run("slow", nil, nil)
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	f.mustRun("plain", nil, nil)
	if waited := time.Since(start); waited > 200*time.Millisecond {
		t.Fatalf("a task with no group waited %v", waited)
	}
}

// The deadlock this would otherwise have on day one: a coordinator built out of
// sub-tasks in its own group. Without inheritance `check` holds `cpu` and then
// waits for `typecheck` to get it, for ever.
func TestACoordinatorDoesNotWaitForItself(t *testing.T) {
	inItsOwnLockDir(t)
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"check":     {Concurrency: "cpu", Cmds: chorefile.Cmds{{Task: "typecheck"}, {Task: "suite"}}},
		"typecheck": {Concurrency: "cpu", Cmds: steps("true")},
		"suite":     {Concurrency: "cpu", Cmds: steps("true")},
	})

	done := make(chan error, 1)
	go func() { done <- f.run("check", nil, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("check: %v\nstderr:\n%s", err, f.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("check deadlocked on its own sub-tasks")
	}
}

// And the same inheritance across a process boundary, which is how a task that
// shells out to chore stays out of its own way.
func TestHeldGroupsTravelInTheEnvironment(t *testing.T) {
	inItsOwnLockDir(t)
	held := holding(context.Background(), map[string]bool{}, "cpu")
	held = holding(held, heldIn(held), "browser")
	if got := heldList(heldIn(held)); got != "browser,cpu" {
		t.Fatalf("heldList = %q, want browser,cpu (sorted, so two runs agree)", got)
	}
}

func TestTheEnvironmentSeedsWhatIsHeld(t *testing.T) {
	inItsOwnLockDir(t)
	t.Setenv("CHORE_HELD", "cpu, browser ,")
	held := heldIn(context.Background())
	if !held["cpu"] || !held["browser"] {
		t.Fatalf("held = %v, want both cpu and browser read from CHORE_HELD", held)
	}
	if held[""] {
		t.Fatal("an empty group was read out of the trailing comma")
	}
}

// A group named with a separator must not name a file outside the lock
// directory, or `concurrency: ../../etc/passwd` is a task that fails oddly.
func TestAGroupNameCannotEscapeTheLockDirectory(t *testing.T) {
	inItsOwnLockDir(t)
	dir := lockDir()
	for _, group := range []string{"build/linux", "a\\b", "../escape"} {
		path := lockPath(group)
		if filepath.Dir(path) != dir {
			t.Fatalf("lockPath(%q) = %q, which is outside %q", group, path, dir)
		}
	}
}

// Two invocations agree on the path without being told anything, which is the
// whole mechanism: the path IS the agreement.
func TestTheSameGroupIsTheSamePathEverywhere(t *testing.T) {
	inItsOwnLockDir(t)
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	first := lockPath("cpu")
	t.Chdir(t.TempDir())
	second := lockPath("cpu")
	os.Chdir(here)
	if first != second {
		t.Fatalf("lockPath moved with the working directory: %q then %q", first, second)
	}
}

// A task waiting for the lock can still be stopped, which is the difference
// between a queue and a hang.
//
// `syscall.Flock(LOCK_EX)` parks a process inside the kernel where nothing can
// reach it. With the wait written that way a queued task ignored Ctrl-C, ignored
// its own `timeout:`, and ignored a `timeout 8` outside it — measured at
// twenty-five minutes against a budget of eight seconds, because chore's signal
// handling then waits on the task it cannot stop. A busy machine had been turned
// into a stuck one.
func TestAWaitingTaskCanBeCancelled(t *testing.T) {
	inItsOwnLockDir(t)
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"hog":    {Concurrency: "cpu", Cmds: steps("sleep 30")},
		"queued": {Concurrency: "cpu", Cmds: steps("true")},
	})
	go f.run("hog", nil, nil)
	time.Sleep(200 * time.Millisecond) // long enough that "hog" certainly holds it

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.r.Run(ctx, "queued", nil, nil) }()

	time.Sleep(200 * time.Millisecond) // and long enough that "queued" is certainly waiting
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled wait returned success; it should report why it stopped")
		}
		if !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("err = %v, want it to name the cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a waiting task ignored cancellation — the wait is not interruptible")
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.Fields(strings.TrimSpace(string(b)))
}
