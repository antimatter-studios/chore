<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/chorefile/schema.go:88 internal/run/run.go:470 -->
---
title: Hooks
summary: before/on_success/on_failure/after, on a task or the whole run
aliases: lifecycle-hooks lifecycle
---

# Hooks

Nine hooks in three families. The distinction that decides which one you want
is not what they do but WHEN they are scoped.

| hook | written as | scope |
|---|---|---|
| `before` | a task field | once per task run |
| `on_success` | a task field | once per task run |
| `on_failure` | a task field | once per task run |
| `after` | a task field | once per task run |
| `lifecycle.before_all` | a top-level block | once per run |
| `lifecycle.on_success_all` | a top-level block | once per run |
| `lifecycle.on_failure_all` | a top-level block | once per run |
| `lifecycle.after_all` | a top-level block | once per run |
| `defer:` | a step inside `cmds:` | once per task run |

Every `lifecycle:` name is its per-task name plus `_all`, and `_all` is the
whole mnemonic: it marks the hook that fires once for the `chore` invocation
rather than once for each task in it.

## Order, for one task run

```
before
  -> deps (concurrent)
  -> cmds
  -> deferred steps, reverse order, only those reached
  -> on_success | on_failure
  -> after
```

The body unwinds BEFORE the outcome branch, so `after` is a finishing step
that runs once the thing it is finishing is already down.

`before` precedes `deps:`, not the other way round: a gate is cheaper the
earlier it fails, and hooks fire for a task that was skipped as up to date
while `deps:` do not — so any other order would make the gate's position
depend on the up-to-date result.

## The four task fields

```yaml
build:
  before:     [ ./check-toolchain.sh ]
  cmds:       [ make ]
  on_success: [ ./publish.sh ]
  on_failure: [ ./collect-logs.sh ]
  after:      [ 'echo "ended {{.EXIT_CODE}}"' ]
```

- **`before` gates.** If it fails, `cmds:` do not run and the task fails with
  the gate's own status. `on_failure` fires for that failure.
- **`after` runs as well as the outcome hook, never instead of it.** On failure
  the order is `on_failure`, then `after`.
- **`after` reads `{{.EXIT_CODE}}`** — `"0"`, or the task's own status. It is
  `$EXIT_CODE` in the script too, from the same value, and exists only in
  `after` and `after_all`.
- **The last three cannot change the exit status.** They are best-effort: a
  failure is reported on stderr and the task's own status stands. `on_failure`
  is where someone will reach to swallow a failure, and it cannot.
- **They run in the TASK's scope** — its variables, parameters and `dir:` — so
  `after: echo done {{.TARGET}}` reads the argument the task was called with.
- **They fire wherever the task runs**, as a dependency or a `- task:` step
  included. A hook that must fire once per invocation belongs in `lifecycle:`.
- **They run even when the task is up to date**, because a hook is not the
  task's prerequisite. That is the whole reason `before` is not a slower
  spelling of `deps:`.
- **A `- defer:` inside a hook is refused.** A hook runs to completion at one
  point in the task's life, so there is nothing for it to defer to.

## `lifecycle:` — once around the whole run

```yaml
lifecycle:
  before_all:     [ {task: hooks:ensure} ]
  on_success_all: [ ./notify-green.sh ]
  on_failure_all: [ ./notify-failure.sh ]
  after_all:      [ 'echo done with {{.TASK}}, status {{.EXIT_CODE}}' ]
```

The canonical use is a self-installing guard: `before_all` activates a repo's
git hooks the first time anyone runs any task, with no per-task boilerplate,
and it fires even when the task it wraps is up to date — which a `deps:` entry
could not, being skipped along with the task.

- `before_all` is a gate. If it fails the task never starts and `after_all`
  never runs, but `on_failure_all` still fires.
- `{{.TASK}}` is the invoked task's name.
- Only the ROOT file's block runs. A `lifecycle:` in an included file is
  ignored, silently.
- Skipped for `--list`, `--help` and `--version`, and for a whole run with
  `--no-lifecycle`.
- A MISTYPED task name still runs them: the name is resolved inside the run.

## `child_hooks:` — one task speaking for its whole subtree

```yaml
build:all:
  child_hooks: false                # everything BELOW me runs no hooks
  deps:  [ prep ]
  cmds:  [ {task: driver}, {task: driver} ]
  after: ./sweep.sh                 # mine still runs — once, not twice
```

- **It does not touch the declaring task's own hooks.** A task that did not
  want those would delete them. What it silences is the tree below, which
  cannot be deleted: the same library task is right to run its hooks when it is
  the top of a run and wrong when nested inside a bigger one, and only the
  caller knows which.
- **It reaches every depth**, through `deps:` and `- task:` alike. A dep is a
  task invocation, so there is no second rule for it.
- **A child cannot opt back in.** `child_hooks: true` inside a suppressed
  subtree does nothing.
- **It never suppresses `defer:`.** That is what makes deep suppression safe:
  all it can silence is advice, never a teardown paired with something already
  brought up.
- **It says nothing about the `lifecycle:` block**, which is per invocation and
  not part of anybody's subtree.

## `defer:` — the positional one

`defer:` is a STEP inside `cmds:`, not a field, and that is the point: where
you put it is information.

```yaml
cmds:
  - docker compose up -d
  - defer: docker compose down     # only registers if `up` was reached
  - ./run-tests.sh
```

If `up` fails the `defer:` is never reached, so `down` never registers and
never runs — correct, because nothing came up. An unconditional `after:` field
would tear down a topology that never existed. Move the `defer:` above the
`up` and it runs unconditionally; that choice is what the position is for.

```
defer registered BEFORE a failing step   -> it RUNS
defer registered AFTER  a failing step   -> it does NOT run, and never registered
```

Defers accumulate as a LIFO stack — `D1 D2 D3` unwinds `D3 D2 D1` — and one
that fails does not stop the others, because a cleanup stack that stopped at
the first failure would leak everything registered beneath it.

Two things `defer:` does not do:

- **It does not run for a task that was up to date.** Nothing was entered, so
  nothing registered. Hooks DO still run in that case; the asymmetry is
  deliberate.
- **It does not see the script's shell.** A deferred step runs in a fresh
  process, so it reads chore's variables and none of the script's.

A failing `defer:` FAILS an otherwise-green task, with the defer's own status.
A failing best-effort hook only prints. That difference is deliberate: a
teardown that did not happen is a resource left behind.
