<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/run/timeout.go:17 -->
---
title: Timeouts
summary: timeout:/on_timeout: — the net for a task that hangs rather than ends
aliases: timeout on_timeout hang hangs deadline
---

# Timeouts

```yaml
e2e:
  timeout: 20m
  on_timeout:
    - ./vm.sh destroy               # $TIMEOUT_PGID is the hung process GROUP
  cmds:
    - vagrant up
    - defer: vagrant destroy -f
    - ./run-tests.sh
```

`defer:` covers a task that ENDS — normally or with an error. `timeout:` covers
a task that does not end at all, which is the common failure and the one every
other net here misses: a build stalled on a lock, an ssh that never returns, a
test deadlocked. A `defer:` runs when the task reaches the step that registered
it, and a hung task reaches nothing.

## What it does when the budget is spent

```
on_timeout    -> the handler runs, with the hung process group still alive
SIGTERM       -> to that group, so the tree goes and not just the shell
SIGKILL       -> 2s later, to anything still there
exit 124      -> the task fails, with the status timeout(1) uses
```

Then the ordinary unwinding happens: `defer:` steps, `on_failure`, `after` —
all of it, on a context the timeout cannot then cancel, since the teardown is
what it fired to have done. `{{.EXIT_CODE}}` in `after` reads `124`.

## The two details that matter

**The handler is given a process GROUP, not a pid.** `$TIMEOUT_PGID` is the
group of the script in flight, and it is handed over while that group is still
alive. Killing a pid orphans whatever the script forked, which is precisely how
a `vagrant up` left qemu and virtiofsd behind holding a global lock with no
parent to clean up after them. `kill -TERM -"$TIMEOUT_PGID"` takes the tree.

**The clock is wall-clock from the start of the task**, not time since the last
output. A hung process very often still logs — progress ticks, keepalives,
retries — so an idle-output timer is quiet on exactly the case worth catching.

## What the handler is told

```
$TIMEOUT        the budget that was spent, e.g. 20m0s
$TIMEOUT_PGID   the process group to signal
$TIMEOUT_PID    the shell's own pid inside it
```

Each is `{{.TIMEOUT_PGID}}` in a template too, like any other variable. Two
cases make the group plural or empty, and a handler that signals is better
written for all three:

```bash
for g in $TIMEOUT_PGID; do kill -TERM -"$g" 2>/dev/null || true; done
```

- **Concurrent `deps:`** can have several scripts in flight, so the value is
  space-separated. chore signals all of them.
- **`interactive: true`** has no group of its own: such a task deliberately
  shares chore's process group, so `-pgid` would name chore itself. The
  variable is EMPTY there, `$TIMEOUT_PID` is all there is, and chore's own
  escalation is limited the same way. A task that must prompt is a poor
  candidate for a timeout for that reason.

## Rules

- **It is not a hook, so nothing suppresses it.** `--no-lifecycle` and
  `child_hooks: false` silence advice; a safety net is not advice, and neither
  is the teardown it triggers. This is the same rule that keeps `defer:` running
  inside a suppressed subtree.
- **The budget covers the task's forward progress** — its `before`, its `deps:`
  (a hang is usually in a dep, and its scripts are tracked too) and its `cmds:`.
  It is switched off before the deferred steps unwind: a deadline that killed
  the teardown it just triggered would be worse than no deadline.
- **The handler gets 60 seconds, and so does the teardown behind it.** Bounded,
  because the handler runs BEFORE the kill — that is what hands it a live group
  — and one that hung would defeat the timeout it serves. Sixty rather than the
  fifteen an interrupt's teardown gets, because this is the specific work the
  net fired to have done.
- **A failing handler cannot change the outcome.** It is reported on stderr; the
  task's status is the timeout either way. `on_timeout` without `timeout:` is
  refused when the file loads, since nothing could ever fire it.
- **`timeout:` is a duration with a unit** — `20m`, `90s`, `1h30m` — parsed when
  the file loads. A typo in a safety net has to fail on the way in, not twenty
  minutes into the task it was meant to guard.
- **A task that was up to date has nothing to time out.** The clock stops with
  the skip.
- **A `defer:` the hang would have swallowed runs after all.** This is the part
  that surprises: a deferred step is registered positionally, so a task that
  never returns never unwinds — until the budget ends the hang, at which point
  the ordinary unwinding happens and the teardown paired with what was brought
  up finally runs. A hang was the one case where `defer:` was unreachable, and
  with a `timeout:` it no longer is.

## It does not replace a backstop outside the process

It will look as though it does — same purpose, better precision, fires far
sooner. But this is a timer inside chore, and a timer dies with the process that
owns it: `SIGKILL` chore, lose the lid on the machine, and nothing fires. The
thirteen-hour lock this exists to prevent was held by a VM whose parent had
already been killed.

So keep the dumb net as well as this one. A guest-side deadline — the VM
scheduling its own poweroff at boot — survives the host being killed outright,
because nothing on the host has to be alive for it to happen. `timeout:` is the
fast, precise net; something out of process is the unkillable one. Both, not
either.
