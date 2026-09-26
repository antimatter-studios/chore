package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// chore:manual concurrency
// title: Concurrency groups
// summary: concurrency: — one heavy task at a time, across every chore on the machine
// aliases: concurrency lock serialise serialize queue parallel overload
// order: 11
//
// # Concurrency groups
//
// ```yaml
// check:    { concurrency: cpu }        # types, suite, benches
// release:  { concurrency: cpu }        # runs the suite too
// playtest: { concurrency: browser }    # one chrome at a time
// shots:    { concurrency: browser }    # the same chrome
// web:      { desc: the dev server }    # contends for neither; never waits
// ```
//
// A task with a `concurrency:` group waits until nothing else on this machine is
// running a task in the same group, then runs. `chore check` twice at once is two
// runs one after the other rather than two runs fighting each other.
//
// ## The group names the RESOURCE, not the task
//
// `cpu` means "this hammers the processor, and only one thing here may". That is
// why `shots` and `playtest` share `browser` — they queue against each other
// because they both want a browser, while `web` runs alongside either because it
// wants neither. One global lock would make the dev server wait for a bench; a
// lock per task would serialise nothing at all.
//
// ## Two chores do not have to find each other
//
// Nothing is registered, discovered or announced. The group name is turned into a
// path — `$XDG_RUNTIME_DIR/chore/<group>.lock` — and every invocation computes the
// same path from the same string. Both open that one file; the kernel, which can
// see both, makes the second wait. Neither process learns the other exists.
//
// It follows that the path must not contain anything that differs between two
// runs you want serialised, and in particular **not the project directory**. Three
// checkouts of one repository on one machine are three paths and one set of cores;
// keyed by directory they would each take their own lock and melt the box
// together, which is the exact thing this is for. Namespace it yourself where you
// do want that — `concurrency: myproject:build`.
//
// It is per user. `XDG_RUNTIME_DIR` is, and the fallback under the temp directory
// is made per uid on purpose: one user's queue is not another's, and a lock file
// one user cannot open is worse than no lock at all.
//
// ## Nothing has to be cleaned up
//
// The lock lives on an open file, not on anything written in one. The kernel drops
// it when the process ends — normally, on a failure, on Ctrl-C, on SIGKILL, on the
// machine losing power. There is no stale "running" record to reap and no pid to
// check for liveness, which is the failure every hand-rolled queue file has: killed
// at the wrong moment, it is wedged until somebody writes the reaper.
//
// ## A task never waits for itself
//
// `check` is built out of `typecheck`, `test` and the benches. If those are in the
// group too, the naive version takes the lock for `check` and then waits for
// `typecheck` to get the lock it is itself holding — a deadlock, on the most-used
// task in the file.
//
// So a held group is inherited. Inside one chore it travels on the context; into a
// chore that a task STARTS — `{{.CHORE_BIN}} install` — it travels as `CHORE_HELD`
// in the environment. Either way a task that finds its group already held runs
// straight away, because the thing the lock protects is already protected.
//
// ## It says when it is waiting
//
// A task blocked in silence looks like a task that has hung, and the first thing
// anybody does to a hung build is kill it. So a wait of more than a moment prints
// which group it is waiting for and which process holds it.
//
// ## What it does not cover
//
// Only what goes through chore. A browser somebody started by hand, a compile in
// another project, anything on the machine that does not take the lock, is
// invisible to it. This keeps chore from being the thing that overloads a machine;
// it cannot stop everything else.

// heldKey carries the groups this subtree already holds. Set only, never cleared:
// a task inside a held group cannot give the lock back early, because its parent
// is still inside the work the lock was taken for.
type heldKey struct{}

// heldIn returns the groups already held on this path, innermost last. The
// environment seeds it so that a chore started BY a task — `{{.CHORE_BIN}} check`
// — knows what its parent is holding and does not queue behind it.
func heldIn(ctx context.Context) map[string]bool {
	held, _ := ctx.Value(heldKey{}).(map[string]bool)
	if held != nil {
		return held
	}
	held = map[string]bool{}
	for _, g := range strings.Split(os.Getenv("CHORE_HELD"), ",") {
		if g = strings.TrimSpace(g); g != "" {
			held[g] = true
		}
	}
	return held
}

// holding returns ctx with one more group held, leaving the original alone: a
// sibling task must not see a group its brother took.
func holding(ctx context.Context, held map[string]bool, group string) context.Context {
	next := make(map[string]bool, len(held)+1)
	for g := range held {
		next[g] = true
	}
	next[group] = true
	return context.WithValue(ctx, heldKey{}, next)
}

// heldList is what goes into the environment of everything this task starts, in a
// fixed order so that two runs of the same tree produce the same string.
func heldList(held map[string]bool) string {
	groups := make([]string, 0, len(held))
	for g := range held {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	return strings.Join(groups, ",")
}

// lockDir is where a group's lock lives: the user's runtime directory if there is
// one, and a per-uid directory under the temp directory otherwise. Per user
// either way — a lock file another user cannot open would be worse than none.
func lockDir() string {
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		return filepath.Join(run, "chore")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("chore-%d", os.Getuid()))
}

// lockPath turns a group name into the one path every invocation agrees on.
// Separators are folded so that `concurrency: build/linux` cannot escape the
// directory or ask for one that does not exist.
func lockPath(group string) string {
	safe := strings.Map(func(r rune) rune {
		if r == os.PathSeparator || r == '/' || r == '\\' || r == 0 {
			return '-'
		}
		return r
	}, group)
	return filepath.Join(lockDir(), safe+".lock")
}

// saidWaiting is how long a task may block before it says so. Short enough that a
// person does not wonder, long enough that the common case — a lock that is free —
// prints nothing at all.
const saidWaiting = 300 * time.Millisecond

// askEvery is how often a waiting task asks for the lock again. Small enough that
// handing over feels immediate, large enough that a task queued behind an hour of
// benches costs a few thousand cheap syscalls rather than a spin.
const askEvery = 50 * time.Millisecond

// waitFor takes the lock, and gives up the moment the context says to.
//
// Asked for repeatedly rather than waited on, and that is the whole of it rather
// than a detail. `syscall.Flock(LOCK_EX)` parks the process inside the kernel
// where nothing can reach it: a task queued on the lock could not be interrupted
// by Ctrl-C, by its own `timeout:`, or by a `timeout 8` outside it — measured at
// twenty-five minutes against a budget of eight seconds, with chore's own signal
// handling then waiting on the task it could not stop.
//
// A task that cannot be cancelled while it waits is worse than a task that never
// waited, because the queue has turned a busy machine into a stuck one. So the
// lock is asked for without blocking, on a short clock, between checks of the
// context — which is the one thing that is allowed to end the wait early.
func (r *Runner) waitFor(ctx context.Context, fd int, path, group, name string) error {
	said := false
	for began := time.Now(); ; {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return fmt.Errorf("%s: concurrency %q: %w", name, group, err)
		}
		// Said once, and only after a wait somebody would notice: a lock that
		// frees immediately is not announced to a task that never really waited.
		if !said && time.Since(began) >= saidWaiting {
			said = true
			fmt.Fprintf(r.Out, "  %s: waiting for %q%s\n", name, group, heldBy(path))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: concurrency %q: %w", name, group, ctx.Err())
		case <-time.After(askEvery):
		}
	}
}

// hold takes the task's concurrency group, if it has one it is not already inside.
//
// It returns the context children should run under and the release. Both are
// always safe to use: a task with no group gets the context it was handed and a
// release that does nothing.
func (r *Runner) hold(ctx context.Context, group, name string) (context.Context, func(), error) {
	if group == "" {
		return ctx, func() {}, nil
	}
	held := heldIn(ctx)
	if held[group] {
		// Already inside it — see "a task never waits for itself" above.
		return ctx, func() {}, nil
	}
	if err := os.MkdirAll(lockDir(), 0o700); err != nil {
		return ctx, func() {}, fmt.Errorf("%s: concurrency %q: %w", name, group, err)
	}
	path := lockPath(group)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return ctx, func() {}, fmt.Errorf("%s: concurrency %q: %w", name, group, err)
	}
	fd := int(f.Fd())

	if err := r.waitFor(ctx, fd, path, group, name); err != nil {
		f.Close()
		return ctx, func() {}, err
	}

	// Who is in there, for the next caller's message. Advisory only: it is read
	// by a waiter that has not got the lock, so it may already be out of date.
	// Nothing depends on it — the lock itself is the kernel's, not this line's.
	f.Truncate(0)
	f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)

	return holding(ctx, held, group), func() {
		// Closing is what releases it. Explicit rather than left to the process
		// ending, so a long run does not hold every group it ever took.
		syscall.Flock(fd, syscall.LOCK_UN)
		f.Close()
	}, nil
}

// heldBy reads the pid a lock file was last stamped with, for the waiting message.
// Empty when there is nothing readable there, which is not an error: the stamp is
// a courtesy and the lock is what actually matters.
func heldBy(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	pid := strings.TrimSpace(string(b))
	if pid == "" {
		return ""
	}
	return " (held by pid " + pid + ")"
}
