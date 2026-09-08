package run

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/tmpl"
)

// chore:manual timeouts
// title: Timeouts
// summary: timeout:/on_timeout: — the net for a task that hangs rather than ends
// aliases: timeout on_timeout hang hangs deadline
// order: 10
//
// # Timeouts
//
// ```yaml
// e2e:
//   timeout: 20m
//   on_timeout:
//     - ./vm.sh destroy               # $TIMEOUT_PGID is the hung process GROUP
//   cmds:
//     - vagrant up
//     - defer: vagrant destroy -f
//     - ./run-tests.sh
// ```
//
// `defer:` covers a task that ENDS — normally or with an error. `timeout:` covers
// a task that does not end at all, which is the common failure and the one every
// other net here misses: a build stalled on a lock, an ssh that never returns, a
// test deadlocked. A `defer:` runs when the task reaches the step that registered
// it, and a hung task reaches nothing.
//
// ## What it does when the budget is spent
//
// ```
// on_timeout    -> the handler runs, with the hung process group still alive
// SIGTERM       -> to that group, so the tree goes and not just the shell
// SIGKILL       -> 2s later, to anything still there
// exit 124      -> the task fails, with the status timeout(1) uses
// ```
//
// Then the ordinary unwinding happens: `defer:` steps, `on_failure`, `after` —
// all of it, on a context the timeout cannot then cancel, since the teardown is
// what it fired to have done. `{{.EXIT_CODE}}` in `after` reads `124`.
//
// ## The two details that matter
//
// **The handler is given a process GROUP, not a pid.** `$TIMEOUT_PGID` is the
// group of the script in flight, and it is handed over while that group is still
// alive. Killing a pid orphans whatever the script forked, which is precisely how
// a `vagrant up` left qemu and virtiofsd behind holding a global lock with no
// parent to clean up after them. `kill -TERM -"$TIMEOUT_PGID"` takes the tree.
//
// **The clock is wall-clock from the start of the task**, not time since the last
// output. A hung process very often still logs — progress ticks, keepalives,
// retries — so an idle-output timer is quiet on exactly the case worth catching.
//
// ## What the handler is told
//
// ```
// $TIMEOUT        the budget that was spent, e.g. 20m0s
// $TIMEOUT_PGID   the process group to signal
// $TIMEOUT_PID    the shell's own pid inside it
// ```
//
// Each is `{{.TIMEOUT_PGID}}` in a template too, like any other variable. Two
// cases make the group plural or empty, and a handler that signals is better
// written for all three:
//
// ```bash
// for g in $TIMEOUT_PGID; do kill -TERM -"$g" 2>/dev/null || true; done
// ```
//
// - **Concurrent `deps:`** can have several scripts in flight, so the value is
//   space-separated. chore signals all of them.
// - **`interactive: true`** has no group of its own: such a task deliberately
//   shares chore's process group, so `-pgid` would name chore itself. The
//   variable is EMPTY there, `$TIMEOUT_PID` is all there is, and chore's own
//   escalation is limited the same way. A task that must prompt is a poor
//   candidate for a timeout for that reason.
//
// ## Rules
//
// - **It is not a hook, so nothing suppresses it.** `--no-lifecycle` and
//   `child_hooks: false` silence advice; a safety net is not advice, and neither
//   is the teardown it triggers. This is the same rule that keeps `defer:` running
//   inside a suppressed subtree.
// - **The budget covers the task's forward progress** — its `before`, its `deps:`
//   (a hang is usually in a dep, and its scripts are tracked too) and its `cmds:`.
//   It is switched off before the deferred steps unwind: a deadline that killed
//   the teardown it just triggered would be worse than no deadline.
// - **The handler gets 60 seconds, and so does the teardown behind it.** Bounded,
//   because the handler runs BEFORE the kill — that is what hands it a live group
//   — and one that hung would defeat the timeout it serves. Sixty rather than the
//   fifteen an interrupt's teardown gets, because this is the specific work the
//   net fired to have done.
// - **A failing handler cannot change the outcome.** It is reported on stderr; the
//   task's status is the timeout either way. `on_timeout` without `timeout:` is
//   refused when the file loads, since nothing could ever fire it.
// - **`timeout:` is a duration with a unit** — `20m`, `90s`, `1h30m` — parsed when
//   the file loads. A typo in a safety net has to fail on the way in, not twenty
//   minutes into the task it was meant to guard.
// - **A task that was up to date has nothing to time out.** The clock stops with
//   the skip.
// - **A `defer:` the hang would have swallowed runs after all.** This is the part
//   that surprises: a deferred step is registered positionally, so a task that
//   never returns never unwinds — until the budget ends the hang, at which point
//   the ordinary unwinding happens and the teardown paired with what was brought
//   up finally runs. A hang was the one case where `defer:` was unreachable, and
//   with a `timeout:` it no longer is.
//
// ## It does not replace a backstop outside the process
//
// It will look as though it does — same purpose, better precision, fires far
// sooner. But this is a timer inside chore, and a timer dies with the process that
// owns it: `SIGKILL` chore, lose the lid on the machine, and nothing fires. The
// thirteen-hour lock this exists to prevent was held by a VM whose parent had
// already been killed.
//
// So keep the dumb net as well as this one. A guest-side deadline — the VM
// scheduling its own poweroff at boot — survives the host being killed outright,
// because nothing on the host has to be alive for it to happen. `timeout:` is the
// fast, precise net; something out of process is the unkillable one. Both, not
// either.
//
// Not a theory. Both were observed working on one machine on one day,
// independently of each other: `timeout:` reclaimed a hung task's VM in 23
// seconds, and a guest booted at 11:24 with a 120-minute bound was confirmed
// from inside itself to have its own poweroff scheduled for 13:25 — a bound that
// holds with nothing on the host alive to notice. Neither net covers the other's
// case. Keep both.

// TimeoutExitCode is what a timed-out task exits with. 124 is what GNU
// timeout(1) reports for the same event, so a caller checking `$?` already has a
// name for it — and it is distinguishable from an ordinary failure (the command's
// own status) and from an interrupt (128+signal), which is the whole point of not
// reusing either.
const TimeoutExitCode = 124

// timeoutCleanupGrace is what each piece of teardown gets after a budget is
// spent: the `on_timeout:` handler, and then the task's deferred steps.
//
// Bounded, because the handler runs BEFORE the kill — that is what lets it act on
// a live process group — and a handler that hangs there would defeat the timeout
// it was written to serve, which is the exact failure that motivated any of this.
//
// A minute rather than the fifteen seconds an interrupt's teardown gets, because
// the work here is the specific work the timeout fired to have done: `vagrant
// destroy`, `docker rm -f`, releasing a lock. The task has already cost its whole
// budget by this point, so a generous allowance for cleaning up after it is
// cheap; an unbounded one is not.
const timeoutCleanupGrace = 60 * time.Second

// timeoutKillGrace is how long the process group has to end after the SIGTERM
// cancellation sends before chore escalates to SIGKILL. It matches the shell's
// own WaitDelay: long enough for a shell to run its traps, short enough that a
// process ignoring SIGTERM is not the thing keeping someone at the terminal.
const timeoutKillGrace = 2 * time.Second

// timeoutKillPoll is how often that grace is checked. The wait ends as soon as
// the group is gone, which in the ordinary case is almost at once: a budget spent
// is already a slow enough day without adding two fixed seconds to it.
const timeoutKillPoll = 25 * time.Millisecond

// hookOnTimeout names the handler in the one place taskHook has to treat it
// differently from every other hook: it is the only one that runs while the
// task's own body is still going, so it must not be handed the task's terminal.
const hookOnTimeout = "on_timeout"

// TimeoutError is a task stopped by its own `timeout:`.
//
// A distinct type rather than a message, because the status is what a caller acts
// on: a timeout is not the same event as the command failing, and reporting both
// as 1 makes a retry loop unable to tell "it broke" from "it never finished".
type TimeoutError struct {
	Task  string
	After time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s: timed out after %s", e.Task, e.After)
}

// ExitCode is read by shell.ExitCode, which asks any error what status it
// deserves rather than knowing the list itself.
func (e *TimeoutError) ExitCode() int { return TimeoutExitCode }

// deadline is one task's wall-clock budget, and the record of what is in flight
// under it.
//
// The second half is the part that makes it useful. A timer that only knows the
// time can cancel; to hand a handler something it can act on, the deadline has to
// know which process groups are alive at the moment it fires — so every script
// chore starts registers here (see Runner.shell) and unregisters when it ends.
type deadline struct {
	task  *chorefile.Task
	after time.Duration
	// parent is the deadline this one is nested inside, if any. A registration
	// propagates UP the chain: a task with a 20m budget whose dep declares its own
	// 30s one must still know about the dep's processes, or its own timer fires
	// holding nothing but a pid that ended long ago.
	parent *deadline

	mu sync.Mutex
	// live maps the pid of each script in flight to its process group — 0 for an
	// interactive script, which has none of its own.
	live  map[int]int
	fired bool
	// off marks the budget as spent-or-irrelevant: the task's forward progress is
	// over and the timer must not fire during the teardown that follows. Set under
	// the same mutex fire() checks it under, which is what makes disarm final
	// rather than a race with a timer already on its way.
	off   bool
	timer *time.Timer
	// settled is closed when a FIRED deadline has finished its own work — the
	// handler, the cancellation, the escalation. The task waits on it rather than
	// returning while all that is still going, so nothing the timeout prints or
	// signals arrives after the run has moved on to something else.
	settled chan struct{}
}

// deadlineKey carries the innermost deadline in the context, so a script started
// anywhere below the task — a dep, a `- task:` step, a `sh:` capture — can find
// it without every layer passing it by hand.
type deadlineKey struct{}

func withDeadline(ctx context.Context, d *deadline) context.Context {
	return context.WithValue(ctx, deadlineKey{}, d)
}

// withoutDeadline hides the deadline from a context. The `on_timeout:` handler
// runs under one: its own scripts must not be registered as the hung work the
// timeout is about, and its own group must not be the one chore then kills.
func withoutDeadline(ctx context.Context) context.Context {
	return context.WithValue(ctx, deadlineKey{}, (*deadline)(nil))
}

func deadlineFrom(ctx context.Context) *deadline {
	d, _ := ctx.Value(deadlineKey{}).(*deadline)
	return d
}

// start records a script that has just begun, up the whole chain.
//
// A disarmed deadline records nothing: what runs after the budget is over is
// teardown, and teardown is not what a timeout is allowed to act on. It still
// propagates, because an ancestor whose own budget is still running does have to
// know about it.
func (d *deadline) start(pid, pgid int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if !d.off {
		d.live[pid] = pgid
	}
	d.mu.Unlock()
	d.parent.start(pid, pgid)
}

// end forgets a script that has finished. Kept accurate rather than left to grow,
// because a pid held after its process is gone is a pid the kernel may since have
// given to something else — and this list is a list of things to signal.
func (d *deadline) end(pid int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	delete(d.live, pid)
	d.mu.Unlock()
	d.parent.end(pid)
}

// inFlight returns the pids and the process groups currently registered, each
// sorted so a message and a variable name them in the same order every time.
// Group 0 — an interactive script sharing chore's own group — is not a group
// anybody may signal, so it is left out of the second list entirely.
func (d *deadline) inFlight() (pids, pgids []int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for pid, pgid := range d.live {
		pids = append(pids, pid)
		if pgid > 1 {
			pgids = append(pgids, pgid)
		}
	}
	sort.Ints(pids)
	sort.Ints(pgids)
	return pids, pgids
}

// disarm ends the budget. Called when the task's forward progress is over — which
// is BEFORE its deferred steps unwind, since teardown must not be killed by the
// deadline that triggered it — and again on the way out of the task, so an early
// return cannot leave a timer running over somebody else's work.
func (d *deadline) disarm() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.off = true
	if d.timer != nil {
		d.timer.Stop()
	}
}

// settle waits for a fired deadline to finish what firing started: the handler,
// the cancellation, and the escalation behind it. Nothing to wait for on a
// deadline that never fired, which is nearly all of them.
//
// The task waits here rather than returning, because a run that says a task is
// over while chore is still killing what timed out is lying about the one thing
// it was asked to be precise about — and a message printed after that line reads
// as belonging to whatever came next.
func (d *deadline) settle() {
	if d == nil {
		return
	}
	d.mu.Lock()
	fired := d.fired
	d.mu.Unlock()
	if !fired {
		return
	}
	<-d.settled
}

// cleanupContext hands a task's teardown a context the timeout cannot pull out
// from under it.
//
// A fired deadline cancels the task's context, and it does so from another
// goroutine — after the handler, which in the recommended shape is the thing that
// killed the body in the first place. So the body can return, the unwinding can
// begin on a context that is still healthy, and the cancellation can then land
// halfway through the very teardown the timeout fired to trigger. Measured
// exactly that way: `task: vm: deferred step failed: context canceled`, with the
// VM still up.
//
// Safe against a timer that is mid-flight, because `fired` is decided under the
// same mutex `disarm` sets `off` under, and the task disarms before it asks. So
// either this sees the firing and protects the teardown, or the firing sees the
// disarm and never cancels at all.
func (d *deadline) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if d == nil {
		return cleanupContext(ctx)
	}
	d.mu.Lock()
	fired := d.fired
	d.mu.Unlock()
	if !fired {
		// Nothing has cancelled anything: teardown runs on the run's own context,
		// interruptible and unbounded, exactly as for a task with no deadline.
		return cleanupContext(ctx)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeoutCleanupGrace)
}

// explain replaces the error a killed task reports with the reason it was killed.
//
// Without it the task fails with "context canceled", which is true and useless:
// the step was signalled, so what surfaces is either that or the shell's 143, and
// neither says the budget ran out. It also answers for the task whose steps all
// declared `ignore_error` — killed, still on the list, and otherwise reported as
// a success.
func (d *deadline) explain(err error) error {
	if d == nil {
		return err
	}
	d.mu.Lock()
	fired := d.fired
	d.mu.Unlock()
	if !fired {
		return err
	}
	return &TimeoutError{Task: d.task.Name, After: d.after}
}

// arm starts the clock for a task that declares `timeout:`, returning a context
// for the task's own work and the deadline governing it. Both are nil-safe
// no-ops for the overwhelming majority of tasks, which declare none.
//
// The returned context is a cancellable child: when the budget is spent, the
// handler runs first and cancellation follows, which is what sends SIGTERM to
// each script's process group through the shell's own Cancel hook and puts the
// run on the teardown path it already has for an interrupt.
func (r *Runner) arm(ctx context.Context, t *chorefile.Task, scope *tmpl.Scope, dir string) (context.Context, *deadline) {
	if t.Timeout <= 0 {
		return ctx, nil
	}
	// Nothing is started in a dry run, so there is nothing for a deadline to be
	// about; arming one would only print a timeout for work that never ran.
	if r.DryRun {
		return ctx, nil
	}
	after := time.Duration(t.Timeout)
	d := &deadline{
		task:    t,
		after:   after,
		parent:  deadlineFrom(ctx),
		live:    map[int]int{},
		settled: make(chan struct{}),
	}
	inner, cancel := context.WithCancel(ctx)
	// The handler is deliberately given ctx, the PARENT — it runs before the
	// cancellation, on a context that is still healthy, because a hook issued on a
	// cancelled one cannot start a process at all.
	d.timer = time.AfterFunc(after, func() { r.fire(ctx, d, scope, dir, cancel) })
	return withDeadline(inner, d), d
}

// fire is the budget running out: report it, run the handler while there is still
// something for it to act on, then stop the task.
//
// The order is the whole design. The handler goes FIRST, because the process
// group it is handed has to be alive for it to take the tree — a handler told
// about a group that has already been killed can only clean up whatever the kill
// orphaned, which is the failure that motivated the feature. chore kills anyway,
// afterwards, so a handler that forgets to (or fails, or is not declared at all)
// cannot leave the hang running.
func (r *Runner) fire(ctx context.Context, d *deadline, scope *tmpl.Scope, dir string, cancel context.CancelFunc) {
	d.mu.Lock()
	if d.off || d.fired {
		// Disarmed, or already fired. Both are decided under this mutex, so a
		// timer that was already on its way when the task finished stops here
		// rather than killing whatever ran next.
		d.mu.Unlock()
		return
	}
	d.fired = true
	d.mu.Unlock()
	// From here the task is waiting on this: settle() blocks until it is done.
	defer close(d.settled)

	t := d.task
	pids, pgids := d.inFlight()
	fmt.Fprintf(r.Err, "chore: %s: timed out after %s%s\n", t.Name, d.after, describeInFlight(pgids))

	if len(t.OnTimeout) > 0 {
		// WithoutCancel, so the handler survives a Ctrl-C arriving at the same
		// moment — the same reason every other teardown here runs on a fresh
		// context. Bounded, because it is holding up the kill. And with the
		// deadline hidden, so the handler's own scripts are neither counted as the
		// hung work nor killed as it.
		hctx, stop := context.WithTimeout(context.WithoutCancel(withoutDeadline(ctx)), timeoutCleanupGrace)
		defer stop()
		if err := r.taskHook(hctx, t, scope, dir, hookOnTimeout, t.OnTimeout, timeoutVars(d.after, pids, pgids)); err != nil {
			// Best effort, like every other outcome hook: the task's status is the
			// timeout whether the handler worked or not.
			fmt.Fprintf(r.Err, "task: %v\n", err)
		}
	}

	// SIGTERM to each script's own process group, via the shell's Cancel hook, and
	// the rest of the run onto the path it already takes after an interrupt.
	cancel()

	// Then the escalation the shell does not do. Its WaitDelay kills the SHELL,
	// which is not enough: `vagrant up` forks qemu and virtiofsd, and a child that
	// ignored SIGTERM outlives the shell as an orphan still holding whatever it
	// held. That orphan IS the failure this feature exists to prevent, so the last
	// word has to be SIGKILL to the whole group.
	//
	// Polled rather than slept out: SIGTERM works almost always and works in
	// milliseconds, and a run should not sit for two seconds proving it. Only the
	// groups that were hung when the budget ran out are candidates — the teardown
	// now unwinding has groups of its own, and killing those would undo the point.
	for waited := time.Duration(0); ; waited += timeoutKillPoll {
		remaining := aliveGroups(pgids)
		if len(remaining) == 0 {
			return
		}
		if waited >= timeoutKillGrace {
			for _, pgid := range remaining {
				if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil {
					fmt.Fprintf(r.Err, "chore: %s: killed process group %d\n", t.Name, pgid)
				}
			}
			return
		}
		time.Sleep(timeoutKillPoll)
	}
}

// aliveGroups returns those of pgids that still have a member.
//
// Signal 0 asks the kernel whether a target exists without touching it, and
// `-pgid` asks about the GROUP rather than one process — which is the question
// worth asking here, since what is left after the shell dies is whatever it
// forked, and that is exactly what the registry of shells cannot see.
func aliveGroups(pgids []int) []int {
	var out []int
	for _, pgid := range pgids {
		if pgid > 1 && syscall.Kill(-pgid, syscall.Signal(0)) == nil {
			out = append(out, pgid)
		}
	}
	return out
}

// timeoutVars is what the handler is told: the budget that was spent, and what to
// signal. Published as variables rather than arguments so the same values read as
// $TIMEOUT_PGID in a script and {{.TIMEOUT_PGID}} in a template, like everything
// else a hook is given.
func timeoutVars(after time.Duration, pids, pgids []int) map[string]string {
	return map[string]string{
		"TIMEOUT":      after.String(),
		"TIMEOUT_PID":  joinInts(pids),
		"TIMEOUT_PGID": joinInts(pgids),
	}
}

func joinInts(ns []int) string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.Itoa(n)
	}
	return strings.Join(out, " ")
}

// describeInFlight names what the timeout found running, because "timed out" on
// its own leaves the reader unable to tell a hung command from a task that had
// already finished its work and was waiting on something chore does not see.
func describeInFlight(pgids []int) string {
	switch len(pgids) {
	case 0:
		return " — no process group of its own was running"
	case 1:
		return fmt.Sprintf(" — signalling process group %d", pgids[0])
	default:
		return fmt.Sprintf(" — signalling process groups %s", joinInts(pgids))
	}
}
