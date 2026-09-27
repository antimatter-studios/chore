<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/run/concurrency.go:15 -->
---
title: Concurrency groups
summary: concurrency: — one heavy task at a time, across every chore on the machine
aliases: lock serialise serialize queue parallel overload
---

# Concurrency groups

```yaml
check:    { concurrency: cpu }        # types, suite, benches
release:  { concurrency: cpu }        # runs the suite too
playtest: { concurrency: browser }    # one chrome at a time
shots:    { concurrency: browser }    # the same chrome
web:      { desc: the dev server }    # contends for neither; never waits
```

A task with a `concurrency:` group waits until nothing else on this machine is
running a task in the same group, then runs. `chore check` twice at once is two
runs one after the other rather than two runs fighting each other.

## The group names the RESOURCE, not the task

`cpu` means "this hammers the processor, and only one thing here may". That is
why `shots` and `playtest` share `browser` — they queue against each other
because they both want a browser, while `web` runs alongside either because it
wants neither. One global lock would make the dev server wait for a bench; a
lock per task would serialise nothing at all.

## Two chores do not have to find each other

Nothing is registered, discovered or announced. The group name is turned into a
path — `$XDG_RUNTIME_DIR/chore/<group>.lock` — and every invocation computes the
same path from the same string. Both open that one file; the kernel, which can
see both, makes the second wait. Neither process learns the other exists.

It follows that the path must not contain anything that differs between two
runs you want serialised, and in particular **not the project directory**. Three
checkouts of one repository on one machine are three paths and one set of cores;
keyed by directory they would each take their own lock and melt the box
together, which is the exact thing this is for. Namespace it yourself where you
do want that — `concurrency: myproject:build`.

It is per user. `XDG_RUNTIME_DIR` is, and the fallback under the temp directory
is made per uid on purpose: one user's queue is not another's, and a lock file
one user cannot open is worse than no lock at all.

## Nothing has to be cleaned up

The lock lives on an open file, not on anything written in one. The kernel drops
it when the process ends — normally, on a failure, on Ctrl-C, on SIGKILL, on the
machine losing power. There is no stale "running" record to reap and no pid to
check for liveness, which is the failure every hand-rolled queue file has: killed
at the wrong moment, it is wedged until somebody writes the reaper.

## A task never waits for itself

`check` is built out of `typecheck`, `test` and the benches. If those are in the
group too, the naive version takes the lock for `check` and then waits for
`typecheck` to get the lock it is itself holding — a deadlock, on the most-used
task in the file.

So a held group is inherited. Inside one chore it travels on the context; into a
chore that a task STARTS — `{{.CHORE_BIN}} install` — it travels as `CHORE_HELD`
in the environment. Either way a task that finds its group already held runs
straight away, because the thing the lock protects is already protected.

## It says when it is waiting

A task blocked in silence looks like a task that has hung, and the first thing
anybody does to a hung build is kill it. So a wait of more than a moment prints
which group it is waiting for and which process holds it.

## What it does not cover

Only what goes through chore. A browser somebody started by hand, a compile in
another project, anything on the machine that does not take the lock, is
invisible to it. This keeps chore from being the thing that overloads a machine;
it cannot stop everything else.
