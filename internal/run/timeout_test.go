package run

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// These tests hang a real task and let a real deadline kill it. The budgets are
// short (a few hundred milliseconds) and the sleeps they interrupt are long, so
// a test that passes for the wrong reason — the command finishing on its own —
// cannot be mistaken for one that passed for the right one.

func ms(n int) chorefile.Duration { return chorefile.Duration(time.Duration(n) * time.Millisecond) }

// mustTimeOut runs a task that is expected to time out and returns the error, so
// each test can assert on the part it is about.
func (f *fixture) mustTimeOut(name string) *TimeoutError {
	f.t.Helper()
	err := f.run(name, nil, nil)
	if err == nil {
		f.t.Fatalf("run %s: want a timeout, got nil\nstdout:\n%s\nstderr:\n%s", name, f.out, f.err)
	}
	var to *TimeoutError
	if !errors.As(err, &to) {
		f.t.Fatalf("run %s: err = %v, want a *TimeoutError\nstderr:\n%s", name, err, f.err)
	}
	return to
}

// The headline: a task that HANGS is stopped, where a `defer:` would have waited
// for it to reach a step it never reaches.
func TestTimeoutStopsAHungTask(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"hang": {Timeout: ms(300), Cmds: steps("sleep 60")},
	})
	start := time.Now()
	to := f.mustTimeOut("hang")
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("the task ran for %s — it was not the deadline that ended it", elapsed)
	}
	if to.Task != "hang" {
		t.Errorf("TimeoutError.Task = %q, want %q", to.Task, "hang")
	}
	// 124 is what timeout(1) reports, so a caller can tell "it never finished"
	// from "it failed" without parsing a message.
	if code := ExitCode(to); code != TimeoutExitCode {
		t.Errorf("ExitCode = %d, want %d", code, TimeoutExitCode)
	}
	mustContain(t, f.err.String(), "timed out after 300ms", "stderr")
}

// The detail the whole design turns on: the handler is given the process GROUP,
// and it is given it while that group is still alive.
//
// `vagrant up` forks qemu and virtiofsd. Killing the pid orphans both — which is
// how a qemu process ended up holding a global lock for thirteen hours with no
// parent to clean up after it — so the handler has to be told the group, and the
// group has to still be there when it is told.
func TestTimeoutHandsTheHandlerTheLiveProcessGroup(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"hang": {
			Timeout: ms(400),
			// The handler records what chore told it, and — from that same live
			// moment — what the kernel says the GRANDCHILD's group is. If the two
			// agree, the value names the whole tree and not just the shell.
			OnTimeout: steps(
				`echo "$TIMEOUT_PGID" > reported.txt`,
				`ps -o pgid= -p "$(cat grandchild.txt)" > actual.txt`,
			),
			Cmds: steps(`
                sleep 60 &
                echo $! > grandchild.txt
                sleep 60
            `),
		},
	})
	f.mustTimeOut("hang")

	reported := strings.TrimSpace(f.read("reported.txt"))
	actual := strings.TrimSpace(f.read("actual.txt"))
	if reported == "" {
		t.Fatal("$TIMEOUT_PGID was empty — the handler was told nothing to signal")
	}
	if reported != actual {
		t.Errorf("$TIMEOUT_PGID = %q, but the grandchild's process group is %q —"+
			" the handler was given something narrower than the tree", reported, actual)
	}

	// And the tree is actually gone. A handler that is told the right group is
	// only half of it: chore signals the group itself, so a handler that forgets
	// to — or that is not declared at all — cannot leave an orphan behind.
	grandchild := strings.TrimSpace(f.read("grandchild.txt"))
	mustDie(t, grandchild)
}

// A hung process very often still logs — progress ticks, keepalives, retries — so
// a timer that watched the OUTPUT would be silent on the case worth catching.
func TestTimeoutIsWallClockNotIdleOutput(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"chatty": {
			Timeout: ms(400),
			Cmds:    steps(`while true; do echo tick; sleep 0.05; done`),
		},
	})
	f.mustTimeOut("chatty")
	if !strings.Contains(f.out.String(), "tick") {
		t.Fatal("the task never produced output, so this proves nothing about idle timers")
	}
}

// The timeout puts the run on the same teardown path an interrupt does, which is
// the reason it is worth having at all: the thing that fires the net is also what
// releases whatever the task was holding.
func TestTimeoutStillRunsTeardown(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"hang": {
			Timeout: ms(300),
			Cmds: chorefile.Cmds{
				{Cmd: "echo up >> order.txt"},
				{Cmd: "echo down >> order.txt", Defer: true},
				{Cmd: "sleep 60"},
			},
			OnTimeout: steps("echo handler >> order.txt"),
			OnFailure: steps("echo failure >> order.txt"),
			After:     steps(`echo "after $EXIT_CODE" >> order.txt`),
		},
	})
	f.mustTimeOut("hang")
	// The handler goes first, because the group it is handed has to be alive.
	// Then the ordinary unwinding: the defers, the outcome hook, the finish.
	want := "up handler down failure after 124"
	if got := strings.Join(strings.Fields(f.read("order.txt")), " "); got != want {
		t.Errorf("order.txt = %q, want %q", got, want)
	}
}

// A deadline that killed the teardown it had just triggered would leave behind
// exactly the resource it fired to reclaim. So the clock stops when the body
// does, before the defers unwind.
func TestTimeoutDoesNotKillTheDeferredSteps(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		// The body finishes at once; the teardown then takes longer than the whole
		// budget. It must still complete.
		"quick": {
			Timeout: ms(200),
			Cmds: chorefile.Cmds{
				{Cmd: "sleep 0.5; echo swept > teardown.txt", Defer: true},
				{Cmd: "true"},
			},
		},
	})
	f.mustRun("quick", nil, nil)
	if got := strings.TrimSpace(f.read("teardown.txt")); got != "swept" {
		t.Errorf("teardown.txt = %q — the deadline killed the teardown it exists to trigger", got)
	}
}

// A hang is usually in a dep rather than in the coordinator, so the coordinator's
// budget has to cover its dependencies and know their process groups.
func TestTimeoutCoversDependencies(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"all": {
			Timeout:   ms(400),
			Deps:      chorefile.Deps{{Task: "hang"}},
			Cmds:      steps("echo body >> order.txt"),
			OnTimeout: steps(`echo "$TIMEOUT_PGID" > reported.txt`),
		},
		"hang": {Cmds: steps("sleep 60")},
	})
	to := f.mustTimeOut("all")
	if to.Task != "all" {
		t.Errorf("TimeoutError.Task = %q, want %q — the budget belongs to the coordinator", to.Task, "all")
	}
	if got := strings.TrimSpace(f.read("reported.txt")); got == "" {
		t.Error("$TIMEOUT_PGID was empty — a dep's script was not registered with the deadline")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "order.txt")); err == nil {
		t.Error("the body ran, but the dep it waited on was killed")
	}
}

// A safety net is not advice. --no-lifecycle silences hooks; it must not silence
// the thing standing between a hang and thirteen hours of it.
func TestNoLifecycleDoesNotSuppressTheTimeout(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"hang": {
			Timeout:   ms(300),
			Cmds:      steps("sleep 60"),
			OnTimeout: steps("echo handler > handler.txt"),
			After:     steps("echo after > after.txt"),
		},
	})
	f.r.NoLifecycle = true
	f.mustTimeOut("hang")
	if got := strings.TrimSpace(f.read("handler.txt")); got != "handler" {
		t.Errorf("handler.txt = %q — --no-lifecycle switched off the timeout handler", got)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "after.txt")); err == nil {
		t.Error("--no-lifecycle did not switch off `after`, which it is supposed to")
	}
}

// The same rule one level down: a coordinator silencing its subtree's hooks is
// silencing advice, and cannot silence a child's deadline.
func TestChildHooksDoNotSuppressTheTimeout(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"all": {ChildHooks: no(), Cmds: chorefile.Cmds{{Task: "hang"}}},
		"hang": {
			Timeout:   ms(300),
			Cmds:      steps("sleep 60"),
			OnTimeout: steps("echo handler > handler.txt"),
			After:     steps("echo after > after.txt"),
		},
	})
	f.mustFail("all", nil, nil)
	if got := strings.TrimSpace(f.read("handler.txt")); got != "handler" {
		t.Errorf("handler.txt = %q — a suppressed subtree lost its timeout handler", got)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "after.txt")); err == nil {
		t.Error("child_hooks: false did not suppress `after`, which it is supposed to")
	}
}

// A task that beats its budget is untouched, and the timer it armed does not
// outlive it: the next task's work must not be killed by the last one's clock.
func TestTimeoutLeavesAQuickTaskAlone(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"quick": {Timeout: ms(200), Cmds: steps("echo done > quick.txt")},
		"slow":  {Cmds: steps("sleep 0.6; echo done > slow.txt")},
	})
	f.mustRun("quick", nil, nil)
	f.mustRun("slow", nil, nil)
	if got := strings.TrimSpace(f.read("slow.txt")); got != "done" {
		t.Errorf("slow.txt = %q — a finished task's deadline reached into the next one", got)
	}
	if s := f.err.String(); strings.Contains(s, "timed out") {
		t.Errorf("a task that finished in time reported a timeout:\n%s", s)
	}
}

// A task skipped as up to date has nothing to time out, so the clock stops with
// the skip rather than running over whatever comes next.
func TestTimeoutStopsWhenTheTaskIsUpToDate(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"build": {
			Timeout:   ms(200),
			Status:    []string{"true"},
			Cmds:      steps("echo built > built.txt"),
			OnTimeout: steps("echo handler > handler.txt"),
		},
	})
	f.mustRun("build", nil, nil)
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(f.dir, "handler.txt")); err == nil {
		t.Error("the deadline fired for a task that was skipped as up to date")
	}
}

// Every step failing with `ignore_error` used to be the one way a killed task
// could report success: nothing propagated, so nothing said the budget ran out.
func TestTimeoutOutranksIgnoreError(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"hang": {
			Timeout:     ms(300),
			IgnoreError: true,
			Cmds:        steps("sleep 60", "echo after-the-hang >> order.txt"),
		},
	})
	f.mustTimeOut("hang")
	if _, err := os.Stat(filepath.Join(f.dir, "order.txt")); err == nil {
		t.Error("a step after the killed one ran — the task carried on past its own deadline")
	}
}

// The handler cannot change the outcome, exactly like every other best-effort
// hook: the task's status is the timeout whether the teardown worked or not.
func TestAFailingHandlerDoesNotChangeTheOutcome(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"hang": {Timeout: ms(300), OnTimeout: steps("exit 9"), Cmds: steps("sleep 60")},
	})
	to := f.mustTimeOut("hang")
	if code := ExitCode(to); code != TimeoutExitCode {
		t.Errorf("ExitCode = %d, want %d — the handler's status replaced the timeout's", code, TimeoutExitCode)
	}
	mustContain(t, f.err.String(), "on_timeout failed", "stderr")
}

// A dry run starts nothing, so there is nothing for a deadline to be about.
func TestDryRunArmsNoDeadline(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"hang": {Timeout: ms(100), Cmds: steps("sleep 60"), OnTimeout: steps("echo handler > handler.txt")},
	})
	f.r.DryRun = true
	f.mustRun("hang", nil, nil)
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(f.dir, "handler.txt")); err == nil {
		t.Error("a dry run fired a timeout handler for work it never started")
	}
}

// An interactive task shares chore's own process group, so there is no group of
// its own to hand out — and -pgid would name chore. The variable is empty rather
// than dangerous, and the pid is what the handler gets instead.
func TestAnInteractiveTaskReportsNoProcessGroup(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"prompt": {
			Timeout:     ms(300),
			Interactive: true,
			Cmds:        steps("sleep 60"),
			OnTimeout:   steps(`echo "pgid=[$TIMEOUT_PGID] pid=[$TIMEOUT_PID]" > reported.txt`),
		},
	})
	f.r.Stdin = strings.NewReader("")
	f.mustTimeOut("prompt")
	got := strings.TrimSpace(f.read("reported.txt"))
	if !strings.Contains(got, "pgid=[]") {
		t.Errorf("reported %q — an interactive task must report no process group, since chore's is not its own", got)
	}
	if strings.Contains(got, "pid=[]") {
		t.Errorf("reported %q — the pid is all such a task has, so it has to be there", got)
	}
}

// mustDie waits for a pid to be gone. Signal 0 asks the kernel whether it exists
// without touching it; a killed process is not necessarily reaped the instant its
// group is signalled, so this allows for that rather than racing it.
func mustDie(t *testing.T, pid string) {
	t.Helper()
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 0 {
		t.Fatalf("no pid to check: %q", pid)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(n, 0); err != nil {
			return // gone
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d is still running — the process group was orphaned, not killed", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// SIGTERM is the polite ask and it is not the last word. A child that ignores it
// outlives the shell as an orphan still holding whatever it held — a lock, a VM —
// which is the state the whole feature exists to prevent, so the group is
// SIGKILLed after a grace.
//
// The `while` loop is what makes this test mean anything: the inner `sleep` dies
// on the group's SIGTERM and the loop starts another, so the group survives
// exactly the signal that ends every other test here.
func TestTimeoutEscalatesToSIGKILLForAGroupThatIgnoresSIGTERM(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"stubborn": {
			Timeout: ms(300),
			Cmds: steps(`
                sh -c 'trap "" TERM; while true; do sleep 1; done' &
                echo $! > stubborn.txt
                sleep 60
            `),
		},
	})
	f.mustTimeOut("stubborn")

	// Asserted AFTER the run returned, which is itself the point: a task does not
	// report itself over while chore is still killing what timed out, so the
	// escalation's own message cannot land against whatever ran next.
	mustContain(t, f.err.String(), "killed process group", "stderr")
	mustDie(t, strings.TrimSpace(f.read("stubborn.txt")))
}

// The recommended handler kills the group itself, and that is exactly what used
// to break the teardown behind it: the body returns as soon as the handler
// signals it, the unwinding starts on a context that is still healthy, and the
// timeout's own cancellation then lands halfway through the very teardown it
// fired to trigger. Measured as `deferred step failed: context canceled`, with
// the thing that was supposed to be torn down still up.
func TestTeardownSurvivesAHandlerThatKillsTheGroup(t *testing.T) {
	f := newFixture(t, &chorefile.File{}, map[string]*chorefile.Task{
		"hang": {
			Timeout:   ms(300),
			OnTimeout: steps(`for g in $TIMEOUT_PGID; do kill -TERM -"$g" 2>/dev/null || true; done`),
			Cmds: chorefile.Cmds{
				{Cmd: "echo down > teardown.txt", Defer: true},
				{Cmd: "sleep 60"},
			},
		},
	})
	f.mustTimeOut("hang")
	if got := strings.TrimSpace(f.read("teardown.txt")); got != "down" {
		t.Errorf("teardown.txt = %q — the timeout cancelled the teardown it fired to trigger", got)
	}
	if s := f.err.String(); strings.Contains(s, "deferred step failed") {
		t.Errorf("a deferred step failed after the timeout:\n%s", s)
	}
}
