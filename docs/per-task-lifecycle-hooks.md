# Per-task lifecycle hooks — design, as agreed

**Status: IMPLEMENTED**, on top of `main` at `d8a38e2`. Everything below is
built, tested and documented in `SPEC.md`; this file is kept as the record of why
each decision went the way it did. Where it says "proposed" or "must be decided",
read it as "was, and now is".

Three things the build settled that this document did not:

- **`{{.EXIT_CODE}}` alone**, not `{{.TASK_STATUS}}` and not both (§2.7).
- **A `- defer:` inside any hook is refused at parse time.** Not covered here at
  all. A hook runs to completion at one point in the task's life, so there is
  nothing for it to defer to, and silently running it as an ordinary step is the
  class of failure this program exists to remove.
- **A failing best-effort hook is reported and does not change the exit status**,
  matching `after_all`, and a failing `before` fails the task with the gate's own
  status.
- **`before` runs BEFORE `deps:`, reversing §2.3.** The golden-file examples
  caught the disagreement. A gate is cheaper the earlier it fails, and the
  decisive reason is one §2.3 did not consider: hooks fire for a task skipped as
  up to date and `deps:` do not, so "deps then before" could only hold for a
  task that was not skipped — the gate's position would depend on the up-to-date
  result. A fixed position is worth more than the tidier diagram.

**Revised 2026-08-27, after review.** Three changes from the version first
written: subtree suppression moved from `hooks: false` on the call site to
`child_hooks: false` on the task, and now excludes the declaring task and covers
`deps:` (§2.6, Appendix A item 15); the exit-status variable is settled as
`{{.EXIT_CODE}}` alone (§2.7); §5 is closed out. Superseded reasoning is kept
where it was, marked, so a reversal is not re-reversed.

The design was settled in conversation on 2026-08-27 between 09:18 and 10:09
UTC. It was agreed and then not built: the work was queued behind the
diskjockey sibling-checkout conversion, that conversion ran, and the chore half
never started. This file exists so the feature can be built from the decisions
already taken rather than re-argued.

Source: session transcript
`~/.claude/projects/-Volumes-sdcard256gb-projects-period-tracker/e2a2b657-69e7-4e8a-9adf-ee2b749eff36.jsonl`,
messages timestamped 2026-08-27T09:18:56Z through 2026-08-27T10:09:32Z.

What *did* ship from that conversation is documentation only: commit `f659804`,
"Hooks are a family of four, and defer is one of them", which gathers the three
global hooks and `defer:` into one SPEC.md section. That commit describes
shipped 0.6.0 behaviour. It is not this feature, and reading it is the most
likely way to conclude, wrongly, that this feature landed.

---

## 1. What exists today (the baseline this changes)

| mechanism | shape | scope | file |
|---|---|---|---|
| `lifecycle.before_all` | top-level block | once per invocation, gates | `internal/chorefile/schema.go:112` |
| `lifecycle.after_all` | top-level block | once per invocation, best-effort | same |
| `lifecycle.on_error` | top-level block | once per invocation, on non-zero | same |
| `defer:` | a **step** inside `cmds:` | per task run, positional | `internal/chorefile/schema.go:229`, `internal/chorefile/decode.go:188` |

Facts about the baseline that the design depends on, each measured rather than
assumed:

- Only the **root file's** `lifecycle:` block runs. A `lifecycle:` in an
  included file parses and is silently ignored (`SPEC.md:177`).
- `Task` (`internal/chorefile/schema.go:147`) has **no hook fields at all**.
- `--no-lifecycle` suppresses the global hooks and **leaves `deps:` and
  `defer:` untouched**. Measured:

  ```
  normal            HOOK-RAN  DEP-RAN  MAIN-RAN
  --no-lifecycle              DEP-RAN  MAIN-RAN
  ```

- `on_error` is registered as the **outermost defer** in `Invoke`, deliberately,
  so it also catches a `before_all` failure (`internal/run/run.go:137`).
- `after_all` runs on a fresh bounded context (`cleanupContext`,
  `internal/run/run.go:346`) so teardown survives an interrupt.

### `defer:` semantics, measured

```yaml
cmds:
  - defer: echo CLEANUP      # registered at step 1
  - 'false'                  # fails at step 2
```
```
CLEANUP ran.        exit status 1
```

```yaml
cmds:
  - 'false'                  # fails at step 1
  - defer: echo CLEANUP      # never reached
```
```
CLEANUP did NOT run.   exit status 1
```

Three properties, all verified:

```
accumulate      LIFO, one entry per `- defer:` REACHED
positional      registers only if execution got to that step
fault-isolated  one failing defer does not stop the others
```

```
- defer: echo D1
- defer: echo D2
- defer: echo D3
- echo body
->  body  D3  D2  D1
```

```
body
D-INNER
task: failing_defer: deferred step failed: … exit status 1
D-OUTER                                       <- still ran
chore: failing_defer: exit status 1
```

This is Go's `defer` exactly, and it is why `defer:` is **not** replaceable by
an `after:` field: a task-level field has no position, so it either always runs
or never does. `defer:` sits inside the sequence and can therefore be paired
with the step immediately before it:

```yaml
cmds:
  - docker compose up -d
  - defer: docker compose down     # only tears down if `up` was reached
  - ./run-tests.sh
```

If `up` fails, `down` never registers — correct, because there is nothing to
tear down. An unconditional `after:` would run `down` against a topology that
never came up.

---

## 2. The feature

### 2.1 Field set (final)

```
lifecycle:   before_all   on_success_all   on_failure_all   after_all
task:        before       on_success       on_failure       after
task:        child_hooks: false
defer:       unchanged — positional, paired, always, reverse order
```

Every global name is its per-task name plus `_all`. The `_all` suffix is not
disambiguation — YAML scope already disambiguates — it is a readability marker
saying "per invocation". Today `on_error` is the odd one out, carrying no
suffix; this change removes that inconsistency in the same stroke that adds the
per-task level.

### 2.2 Rename: `on_error` → `on_failure_all`

A clean rename, **no alias, no deprecation window**. Justified by measurement,
not taste:

```
chores.yml on this machine using on_error      0
references inside chore (Go, non-test)         6
shipped in                                     #16
```

Zero users, so the rename is free now and will never be this free again. An
alias is the right call when users exist; with none it just ships two spellings
forever, and two spellings is the problem being fixed.

`chore_min_version` is how the break is made loud: a file using the new names
against an older chore otherwise gets a confusing `unknown field`, where
`chore_min_version: 0.8.0` says "this file needs a newer chore" instead.

### 2.3 Ordering

For one task run:

```
before
  -> deps (concurrent)
  -> cmds
  -> deferred steps, reverse order, only those reached
  -> on_success | on_failure
  -> after
```

**Corrected during implementation:** this section originally put `deps` first.
See the status note at the top.

The body unwinds **before** the outcome branch, then the unconditional finish.
The alternative — `after` before the defers — means the finishing step runs
while the thing it is finishing is still up, which is the wrong way round for
the docker-compose case.

At the global level, matching:

```
before_all -> <task> -> on_success_all | on_failure_all -> after_all
```

### 2.4 Decisions that must be written into SPEC.md, because each will
otherwise be assumed wrongly

1. **`after` runs *in addition to* the outcome hook, not instead of it.** On
   failure: `on_failure` fires, then `after`. "Any status" reads equally well
   as "the fallback when neither matched", so it has to be stated.
2. **The same answer applies at both levels.** `on_failure_all` first,
   `after_all` last. Otherwise the symmetry is skin deep.
3. **`before` gates.** If a task's `before` fails, the task's `cmds:` do not
   run. This mirrors `before_all`.
4. **`on_failure` fires when the task's own `before` gate fails.** The global
   behaviour is already this (`on_error` is the outermost defer); matching it is
   a decision, not a detail.
5. **`after` / `on_success` / `on_failure` cannot change the exit status.** They
   are best-effort, like `after_all`. `on_failure` is exactly where someone will
   want to swallow a failure, so the refusal must be explicit.
6. **Per-task hooks fire whenever the task runs**, including when it is invoked
   as a subtask by another task. There is no "only when invoked directly" mode.
   If a hook must fire once per invocation, that is what `lifecycle:` is for.
   `run: once` already bounds repetition for a task depended on many times.
7. **Per-task hooks obey `--no-lifecycle`.** `deps:` and `defer:` do not, as
   today.
8. **`child_hooks: false` propagates to the whole subtree** below the task that
   declares it — through `deps:` and `- task:` steps alike — and a child
   **cannot opt back in**. See 2.6.
9. **`child_hooks: false` never suppresses `defer:`**, at any depth.
10. **`child_hooks: false` does not suppress the declaring task's own hooks.**
    A task that did not want its own hooks would delete them; the field exists
    to say "I am handling this for the whole tree", which requires the
    coordinator's own hooks to still fire. See 2.6.

### 2.5 `before` earns its place — and not for the reason first argued

The first argument against `before` was that `deps:` already runs things first,
and two ways to do one thing is how a spec rots. That argument is wrong, and
the tool itself contains the counter-evidence: `--no-lifecycle` suppresses hooks
and leaves deps alone. chore has therefore **already committed to the principle
that deps are requirements and hooks are advice.** `before` and `deps:` differ
in **suppressibility**, not timing, and that is a real semantic difference the
tool already honours.

The secondary difference, same as the one `SPEC.md` gives for `before_all`
existing: deps run concurrently and are prerequisites, so they are **skipped
when the task is up to date**. A hook is not the task's prerequisite and runs
anyway.

### 2.6 `child_hooks: false` on the task

The suppression control is a **field on the coordinating task**, and it excludes
that task itself:

```yaml
build:all:
  child_hooks: false                     # everything BELOW me runs no hooks
  deps: [prep]                           # covered
  cmds:
    - { task: driver, vars: {NAME: ext4}  }
    - { task: driver, vars: {NAME: ntfs}  }
    - { task: driver, vars: {NAME: erofs} }
  after: ./scripts/sweep-target.sh        # mine still runs — once, not three times
```

Read as a sentence: *this task is doing something that encompasses the whole
tree beneath it, so nothing beneath it should do its own version of that.* One
line at the top of the task def answers "do the hooks below here fire?" for the
entire subtree.

**It excludes the declaring task.** A task that suppressed its own hooks while
declaring them would be a contradiction resolved by deleting them, so
`child_hooks: false` would buy nothing at that level. What it buys is the tree
*below*, which cannot be edited away — a library task's hook is correct when
that task is the top of a run and wrong when it is inside a bigger one, and only
the caller knows which. The name carries this: *child*.

**`deps:` are not a special case.** A dep *is* a task invocation — `deps` calls
`Run` exactly as a `- task:` step does (`internal/run/run.go:372`) — so a dep's
hooks are just that task's hooks, and suppression reaches them by descending
with the runner rather than by being declared on the edge. There is no route
into the subtree that escapes it, because there is only one route: running a
task.

**It propagates all the way down.** A shallow (one-level) suppression cannot
express the case above: `driver` calls the library's `staticlib`, which is where
the hook actually lives, so one-level suppression stops at a task that has no
hooks and leaves the three real ones firing. Deep is also the **consistent**
reading, because `--no-lifecycle` already works that way — it does not suppress
only the outermost hook. `child_hooks: false` is that same idea scoped to one
task's subtree rather than the whole process. Shallow would be the novel
semantic, not deep.

A child **cannot opt back in**. Suppression is the coordinator's statement about
a tree it owns for the duration; a task re-enabling itself from inside would
make the coordinator's guarantee unverifiable from where it is written.

Deep suppression is safe **only because `defer:` is never suppressed.** Paired
teardown is a guarantee, not advice; all `child_hooks: false` can silence is
advice.

**Known cost: granularity.** A coordinator that calls three drivers (suppress)
and one `notify` (keep) cannot split the difference — it is the whole subtree or
nothing. The escape hatch is to split the coordinator into two tasks. That is
preferable to a per-call-edge flag, which would have to be added to both `Cmd`
and `Dep`, written out on every edge, and re-read at each one to answer a
question about the tree as a whole. If the mixed case ever turns up with real
evidence behind it, a per-edge override can be added then.

### 2.7 `after` needs the exit status in scope

**Decided 2026-08-27: `{{.EXIT_CODE}}`, and only that.** It renders `"0"` on
success and the task's own status otherwise, inside `after` and `after_all`:

```yaml
after:
  - '[ "$EXIT_CODE" = 0 ] && echo green || echo red'
  - echo 'task ended {{.EXIT_CODE}}'
```

`{{.TASK_STATUS}}` ("success"/"failure") was the alternative and is **not**
shipping. It loses the code, so a hook cannot tell `exit 1` from `exit 7`, and
shipping both would be two spellings of one fact — the same defect §2.2 refuses
an alias for.

Without a status variable at all, `after` cannot do anything outcome-dependent,
and people write the same command into both `on_success` and `on_failure` just
to get "always" — the duplication `after` exists to remove. This is the one part
of the design with **no current workaround**.

`{{.TASK}}` already renders the invoked task's name inside a hook today.

---

## 3. Rejected, with reasons — do not re-propose

| proposal | why rejected |
|---|---|
| `hooks: false` on the call site | Reversed on 2026-08-27, after the doc was written — see 2.6 and Appendix A items 4 and 12. Three defects. It cannot reach `deps:` at all, because `Dep` is a separate struct and a dep is a child too. It has to be repeated on every edge to answer one question about the tree. And its name is silent about *whose* hooks die, which reads as "this callee runs no hooks" — and if that were the intent, the callee's hooks would just be deleted. Replaced by `child_hooks: false` on the task. |
| shallow suppression (one level) | Cannot express the coordinator case (2.6); inconsistent with `--no-lifecycle`, which is run-wide. |
| `child_hooks: false` also suppressing the declaring task's own hooks | Then it is `--no-lifecycle` for one subtree including its root, and the coordinator cannot do the very thing it suppressed the children for. A task that wants no hooks of its own deletes them. |
| moving `defer:` into the task definition as a field | It would lose its position, so it could only always run or never run — which is `after`. Two fields, one behaviour, and the pairing guarantee gone. A field could hold a *list*, but every entry would register at the same instant, so reverse order becomes meaningless. `defer` is an **instruction executed at a point**; `before`/`after`/`on_success`/`on_failure` are **declarations about the task**. Same reason Go's `defer` is a statement, not a function attribute. |
| keeping `on_error` as an alias of `on_failure_all` | Zero users; an alias ships two spellings forever, which is the defect being fixed. |
| adding no global `on_success_all` (leaving the globals at three) | Considered — `after_all` plus a status check covers it, and the global level exists to be small — but rejected in favour of full symmetry, so every global name is a per-task name plus `_all`. |
| a `before` that duplicates `deps:` | Not a duplicate: see 2.5. `deps:` are requirements, hooks are advice. |

One thing worth keeping from the "declare defer up front" instinct: it is
already expressible. Put the `defer` as the **first** step — it registers before
anything can fail, so it always runs, and the position still says so.

---

## 4. Implementation path

Line numbers are `main` at `d8a38e2`.

**The abstraction already exists.** Both helpers are generic over
`(hookName, cmds, trigger)` and nothing in either is global-specific:

```go
// internal/run/run.go:168
func (r *Runner) runHook(ctx context.Context, hook string, cmds chorefile.Cmds, trigger string) error
// internal/run/run.go:198
func (r *Runner) runHookBestEffort(ctx context.Context, hook string, cmds chorefile.Cmds, trigger string)
```

The gating distinction the design needs is already encoded in the split:

```
runHook            returns error   -> can gate.  before / before_all use this.
runHookBestEffort  returns nothing -> cannot change exit status, and runs on
                                      cleanupContext, so it survives Ctrl-C
```

So `after` / `on_success` / `on_failure` inherit interrupt survival for free by
calling `runHookBestEffort` — the property that looked like it would need care.

Work items:

1. **`Task` gains four fields** — `Before`, `After`, `OnSuccess`, `OnFailure`,
   all `chorefile.Cmds` (`internal/chorefile/schema.go:147`).
2. **`Lifecycle` renames `OnError` → `OnFailureAll` and gains `OnSuccessAll`**
   (`internal/chorefile/schema.go:112`); six internal references to update.
3. **`Task` gains `ChildHooks *bool`** (`internal/chorefile/schema.go:147`). A
   pointer, so "not declared" is distinguishable from an explicit `true`. The
   Runner carries the suppression state down the call stack — through `deps`
   and `command` alike — rather than re-reading it per task, because it is the
   ancestor's declaration that decides, and a descendant cannot lift it.
   Nothing is added to `Cmd` or `Dep`.
4. **Second call site in the task path.** `execute`
   (`internal/run/run.go:251`) already collects and unwinds deferred steps at
   `run.go:275-311`. `before` goes before the `cmds` loop; the outcome hook and
   `after` go after the defer unwind, using the error from the body.
5. **Hook label becomes scoped.** Today the synthetic task is labelled
   `"lifecycle:" + hook`; it becomes `scope + ":" + hook` —
   `lifecycle:after_all`, `task:after` — so output says which level fired.
6. **`--no-lifecycle` must reach the task path** (`internal/run/run.go:44`,
   `internal/cli/cli.go:313`), where today it only guards the global block.
7. **`chore_min_version`** bumped in the docs' examples, and the CHANGELOG entry
   written as a breaking change.

### SPEC.md changes

- The Hooks section from `f659804` becomes the home for all of it; the hook
  table at `SPEC.md:118` gains the four per-task rows and `child_hooks:`.
- Each of the ten decisions in 2.4 gets a sentence. The four most likely to be
  assumed wrongly: `after` runs **as well as** the outcome hook;
  `child_hooks: false` covers the **whole subtree, `deps:` included**; it does
  **not** cover the declaring task; and it never touches `defer:`.
- `SPEC.md:192` (`--no-lifecycle` turns off hooks, not dependencies) must say it
  now covers per-task hooks too, and still not `defer:`.

### Tests

`internal/run/lifecycle_test.go` is the model — it already asserts run-once,
ordering, the gate, and that `on_error` fires on failure but not on success.
Mirror every case at task level, plus:

- a task's hooks fire when it is invoked **as a subtask**;
- `child_hooks: false` suppresses hooks in the subtree, through both `deps:`
  and `- task:` steps, at every depth;
- `child_hooks: false` does **not** suppress the declaring task's own hooks;
- `child_hooks: false` does **not** suppress `defer:` at any depth;
- a child inside a suppressed subtree **cannot** re-enable its own hooks;
- `before` failing skips `cmds:` and still fires `on_failure`;
- `on_failure` cannot change the exit status;
- defers unwind **before** `on_success` / `on_failure` / `after`;
- `--no-lifecycle` suppresses per-task hooks and leaves `deps:` and `defer:`.

---

## 5. Closed since writing

- **The exit-status variable** in `after` / `after_all` — settled as
  `{{.EXIT_CODE}}` alone, 2026-08-27. See §2.7.
- **A failing `defer:` in an otherwise-green task** — already settled in shipped
  behaviour, and the doc missed it: `SPEC.md:210` says the run FAILS with the
  defer's own status. `execute` implements exactly that
  (`internal/run/run.go:308-312`, `if runErr == nil { runErr = err }`). No
  change, no decision needed.
- **Whether `before` can mark a task up-to-date / skip it**, as distinct from
  failing it — no. It gates or it passes; it cannot skip.
- **Where subtree suppression lives** — `child_hooks: false` on the task, not
  `hooks: false` on the call site. See §2.6 and Appendix A item 15.

Nothing is open.

## 6. Related work found while designing this

Two chore 0.6.0 defects surfaced during the same session and are recorded here
only so they are not re-found:

- `run.go:313` passed a nil renderer to `fingerprint.Save`, so a templated
  `sources:` hashed the raw pattern and stored the empty-set hash — the task
  never went up to date. `SaveWith` already takes a Renderer.
- Call vars on `- task:` were not case-folded the way command-line args are, so
  `vars: {out: …}` against `args: [out]` left `{{.OUT}}` empty.

Both were reported against 0.6.0; check `main` before acting on either.

---

## Appendix A — the conversation this came from, in order

Recorded verbatim so the debate does not have to happen twice. It took over an
hour. Every position below was argued to a conclusion; **treat each as settled
and build to it.** Where an argument was made and then defeated, the defeat is
recorded too, because otherwise the losing argument gets re-made.

The quotes are the repository owner's own words, from the transcript cited at
the top of this file. The responses are compressed to the decision they
produced.

### 1 — 09:18:56Z · the opening ask

> "I would like to extend the lifecycle hooks, we have a
> `before_all` `after_all` `on_error` but I think each task also needs lifecycle
> hooks. defer is already a lifecycle hook right? maybe we don't treat it like
> one, but maybe it is. extras could be: `before` `after` `on_error` — then we
> have these lifecycle hooks on every task as well, which would be useful for
> giving specific things to do after tasks at each level"

Answer: yes, `defer:` *is* already a per-task lifecycle hook, filed under step
forms rather than named as one. So the useful question is not "add three hooks"
but what each new one does that `defer:` does not — otherwise there are two
spellings of one behaviour and people pick by coin flip.

### 2 — 09:20:38Z · is `after` just `defer`?

> "does 'defer' actually mean 'after' in this case then? seems like ti, but
> perhaps there is a use case for both?"

Tested, not reasoned about. They are genuinely different and the difference is
**positional** — the measurements are in §1 of this document. `defer` = "undo
the thing I just did, if I did it". `after` = "when this task ends, do this,
regardless". Both are worth having. The trap is that `after` *looks* like the
general case, when `defer` is the one with the stronger guarantee.

### 3 — 09:22:50Z · the field set, first cut

> "ok, so perhaps 'after' is the same as 'defer' but then perhaps we'd like
> 'on_success' or 'on_failure'. so ok, 'before' overlaps with 'deps' which seems
> like we could just add a dep when we needed that. so we could have:
> on_success, on_failure, after (any status). what about that?"

Accepted as coherent. Two consequences raised at this point and both later
settled: whether `after` runs *as well as* the outcome hook (yes — §2.4), and
where the defers sit in the order (before the outcome branch — §2.3).

**`before` was dropped here and later re-instated.** See item 5.

### 4 — 09:24:08Z · child hooks, and the flag that was rejected and then restored

> "oh, so the problem you're mentioned is, do child hooks execute always?
> perhap we could add a `child_hooks: true/false` to control that behaviour?"

**This was the right answer, and the doc originally recorded it as rejected.
Reinstated 2026-08-27 — see the addendum at item 15.** The reasoning below is
what was argued at the time and is kept only so the reversal is legible.

Pushed back and the push-back held: the flag makes behaviour non-local, is
ambiguous about whether it lives on the parent or the child, and duplicates a
distinction the two levels already encode. Full reasons in §3. What replaced it
is `hooks: false` at the **call site** (§2.6).

### 5 — 09:25:52Z · the argument that re-instated `before`

> "child hooks might be a reason we want to separate deps from hooks, if we want
> a hook, putting it into deps would mean it executes all the time, but
> `child_hooks:false` would disable the before hook without affecting the deps"

**This won, and it overturned the answer given one message earlier.** Measured
immediately:

```
normal            HOOK-RAN  DEP-RAN  MAIN-RAN
--no-lifecycle              DEP-RAN  MAIN-RAN
```

chore has already committed to *deps are requirements, hooks are advice*. So
`before` is not redundant with `deps:` — they differ in **suppressibility**.
`before` is in the design because of this exchange. Do not remove it on the
grounds that `deps:` covers it; that argument was made and lost.

### 6 — 09:29:31Z and 09:29:55Z · the rename

> "true, on_error and on_failure is basically the same, lets drop on_error in
> favor of on_failure because of symmetry with on_success?"

> "I'd also change the global on_error to on_success on_failure as well"

Measured: zero `chores.yml` on the machine use `on_error`, six internal Go
references. Clean rename, no alias (§2.2). The `_all` suffix goes on all four
globals so every global name is a per-task name plus `_all` (§2.1).

### 7 — 09:31:08Z · scope as a parameter, not two implementations

> "so actualy, if we have before, on_success, and on_failure, it seems we have
> them at global level and task level, this might make it easier because it
> means we could perhaps abstract the hooks from the code and we just use the
> global or task as a way to scope things, so the hooks code can be refactored
> into this pattern that accepts its scope"

Checked against the code: **the abstraction already exists.** `runHook` and
`runHookBestEffort` are already generic over `(hookName, cmds, trigger)`, and
the gate-vs-best-effort split already matches the design. The work is a rename
of the label plus a second call site (§4). This is why the feature is small.

### 8 — 09:34:58Z · the driving use for `before_all`

> "ah and chore can actually use a global before hook to enable the git hooks
> automatically, therefore sidestepping the problem of not having them
> installed. The second chore is called, they're installed"

Confirmed — it is SPEC.md's documented canonical use, and `deionizer` already
does it: `core.hooksPath = .githooks`, `internal: true` + `silent: true`, and
`before_all` rather than `deps:` precisely because it fires even when the task
is up to date. Not part of this feature, but it is the pattern the feature is
being built to extend.

### 9 — 09:39:38Z · the hole that produced `hooks: false`

> "but tasks that are executed as subtasks, aren't given command lines, so we
> need a way to surpress hooks if we need to do it finally because we need to
> add it to a task definition?"

Correct, and it defeated the earlier "just use `--no-lifecycle`" — a subtask has
no command line. `Cmd` already carries per-call-site modifiers (`silent`,
`ignore_error`), so `hooks: false` sits beside them with no new concept.

### 10 — 09:48:30Z · what "positional" means

> "when you say defer is positional, you mean it's position in the task
> definition?"

Yes — its position in the `cmds:` list, and specifically whether execution ever
reached that line. Registration happens when the step executes, not when the
file is parsed.

### 11 — 09:53:19Z · scope of responsibility

> "chore only works when you use it, if you choose to write the command lines
> yourself, chore can't help you and I don't care about that person anyway. They
> can do what they want. Then they have to manage everything themselves. It's
> their choice"

Taken as a standing constraint: **hooks do not need a backstop for someone who
bypasses chore.** Design for the chore path only.

### 12 — 09:54:54Z · deep suppression, which overturned "shallow"

> "but what if A wants to disable hooks to do something at it's level instead.
> hooks:false only affects it's own hooks, so tasks can't control downstream
> functionality, despite being the coordinator of those downstream tasks"

**This won.** The prior recommendation of shallow (one-level) suppression was
wrong: the hook usually lives one level below where the coordinator can see it,
so shallow suppresses nothing real. Deep is also consistent with
`--no-lifecycle`. It is safe **only** because `defer:` is never suppressed —
that is what stops deep suppression leaking a resource. See §2.6. Do not
re-propose shallow.

### 13 — 10:08:10Z and 10:38:45Z · `defer` stays a step

> "oh, but the lifecycle hooks are written on the task, but this
> `- defer: echo FIRST` that you wrote, is that really a good idea? or is it a
> better idea to move defer into the task definition instead of writing it as a
> command?"

> "regarding what you wrote about defer, ok, it sseems better the way it is now
> instead of changing it, it really is a positional command"

**Closed: `defer:` stays exactly as it is.** The asymmetry with the four new
fields is not an inconsistency — the four are *declarations about the task*,
true before it starts; `defer` is an *instruction executed at a point*. What is
genuinely inconsistent is the documentation, and that half already shipped as
`f659804`.

### 14 — 10:09:11Z · defers accumulate

> "I guess defer has value in that it can be specified multiple times in the
> command sequence and execute different things, do they accumulate to a list of
> defer callers, or do they overwrite the previous one?"

Measured: they accumulate as a LIFO stack, nothing overwrites anything, and a
failing defer does not stop the others (§1). A cleanup stack that stopped at the
first failure would leak everything registered beneath it — which is precisely
when it most needs to keep going.

### 15 — addendum, 2026-08-27 · `hooks: false` reverted to `child_hooks: false`

Raised on reading this document back:

> "I think it should only apply to the child tree, not including the task it's
> starting from. My reason is, that if I don't want hooks on this task, I should
> just remove them from the task def. Why add hooks to the current task, then add
> `hooks:false`, it's inconsistent logically. thats why originally i wrote
> `child_hooks:false`, to make it obvious it doesn't apply to the current task …
> because I guess the current task is planning to do something that encompasses
> the entire tree itself"

> "child_hooks:false applies to every task, including deps … they run as a child
> of this task too"

**This won, and it reverses item 4 and §2.6 as first written.** Three reasons,
in ascending order of force:

1. *Logical consistency.* The only thing a call-site `hooks: false` does beyond
   deleting the callee's hooks is silence the tree below it — so the name has to
   say "below", and `child_hooks` does.
2. *One read, one answer.* The question "do hooks fire under here?" is about the
   tree, so it belongs at the top of the coordinating task, not repeated on every
   edge into it.
3. *`deps:` were unreachable.* `Dep` (`internal/chorefile/schema.go:219`) carries
   only `task`/`vars`/`silent`, and item 3 of §4 originally added `Hooks *bool` to
   `Cmd` alone. A dep is a child; a per-edge flag would have had to be added to
   both structs and written on both. A task field covers the subtree however it
   was entered.

What survives from the original design unchanged: deep propagation, no opting
back in from below, and `defer:` never suppressed. What changed: where the field
lives, its name, and that it excludes the task declaring it.

---

## Appendix B — what to build, if you read nothing else

```yaml
# per task
build:
  before:     [ ... ]      # gates: failure skips cmds, fires on_failure
  cmds:       [ ... ]
  on_success: [ ... ]      # best-effort
  on_failure: [ ... ]      # best-effort, cannot swallow the failure
  after:      [ ... ]      # best-effort, any status, IN ADDITION to the above
                           # {{.EXIT_CODE}} in scope: "0", or the task's status

# per invocation, root file only
lifecycle:
  before_all:     [ ... ]
  on_success_all: [ ... ]
  on_failure_all: [ ... ]   # was on_error — clean rename, no alias
  after_all:      [ ... ]

# subtree suppression, on the coordinating task
build:all:
  child_hooks: false       # deps AND `- task:` steps, at every depth
                           # NOT this task's own hooks; never suppresses defer
  deps:  [ prep ]
  cmds:  [ { task: driver } ]
  after: ./sweep.sh        # mine still runs, once

# unchanged
cmds:
  - docker compose up -d
  - defer: docker compose down
```

Order per task run:

```
before -> deps (concurrent) -> cmds -> defers (LIFO, reached only)
                  -> on_success | on_failure -> after
```
