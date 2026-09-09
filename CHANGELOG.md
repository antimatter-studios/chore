# Changelog

## Unreleased

- **Commands are no longer printed unless you ask.** `--verbose` prints each one
  before it runs; nothing does otherwise.

  A task's script is written for the shell, not for a reader. Measured on a real
  `chore test collisions`, the echo put a six-line `case` dispatcher and a
  compound one-liner on screen before the test runner said anything:

      case "collisions" in
        "")                 pnpm vitest run ;;
        collisions)         /opt/homebrew/bin/chore collisions ;;
        wire|protocol)      /opt/homebrew/bin/chore wire ;;
        *)                  pnpm vitest run "collisions" ;;
      esac

      pnpm vitest run src/world/collisions.test.ts; kept=$?; cat collision-report.txt || true; ...

  Noise ahead of the work, so it cannot be skipped past, and it buries the output
  that was wanted. The verdict was already in the repository: **ten of the eleven
  curated examples set `silent: true`**, and switching off this echo was the only
  thing that field did for them. A default every example disables is the wrong
  default. Those ten no longer set it.

  What the echo was worth is kept. When a step FAILS, the step is printed then —
  after its own output, on stderr, and only when the failure is not being
  ignored — so a five-step task still says which line produced the status:

      chore: build: failing step:
          case "release" in
            release) echo "picking release"; exit 4 ;;
            *)       echo "debug" ;;
          esac
      chore: build: exit status 4

- **`verbose: true` on a task or a file** prints that task's commands with nobody
  passing a flag — the inverse of what `silent:` used to buy, now that the
  default is quiet. For the task whose commands are part of what the operator is
  meant to see: a deploy, a destructive migration, anything where "what exactly
  did it run" is the question afterwards.

  Four settings now decide what reaches the terminal, and they settle in one
  order:

      a command's own silent:      never printed, --verbose included
      --verbose                    prints everything else
      the task's verbose:/silent:  a loud task in a quiet file, or the reverse
      the file's verbose:/silent:

  A command's own `silent:` is the only setting attached to THAT text, so it is
  the only one that can promise it never reaches a screen — a token on a command
  line. It outranks `--verbose`, and the failing-step report skips it too,
  because a secret does not become printable by failing.

  A task setting both `silent:` and `verbose:` is refused when the file loads:
  they are opposite answers to one question, and picking a winner silently is how
  a file ends up meaning something nobody wrote. Across LEVELS there is no
  conflict — a task outranks its file either way, which is what lets one loud
  task live in a quiet file.

  Two smaller consequences. `--dry` is untouched: it prints the commands INSTEAD
  of running them, which is the whole flag. And `silent:` now only suppresses
  chore's own progress notices — in practice the "is up to date" line — which
  also cleans up the documented value-capture pattern: a `sh:` var reading
  `{{.CHORE_EXE}} _helper` used to capture the nested chore's echoed command
  along with the value, and needed `silent: true` on the helper to avoid it. It
  has never suppressed a command's own output, which is the task's business and
  passes through untouched.

- **`chore help timeouts` says to write the teardown idempotent, and why the
  redundancy is free.** A timeout means more than one thing may tear down the
  same resource, with the handler usually first: `on_timeout:`, then the task's
  `defer:` steps, then anything outside the process. All of them can run against
  a state that is already clean, and a failing `defer:` fails an otherwise-green
  task — so a teardown that errors because the work was already done turns a
  clean run red.

  With the line drawn where it belongs, which is not "never fail": exit 0 for
  every state that is not the resource still being there, non-zero only when it
  is. A blanket `|| true` swallows the one case that has to be loud, and a
  teardown reporting success while the thing is still up is worse than one that
  fails, because nobody looks again at a green run.

  Observed with three layers composed on a hung VM fixture: the handler reclaimed
  it, and the `defer:` and an out-of-process reaper then ran harmlessly on an
  already-clean state. That is a better argument for keeping all three than "each
  covers a case the others do not" — the overlap is free, so there is nothing to
  trade off when deciding whether to keep the outer nets.

## v0.10.1

Two corrections, both found while verifying the release that preceded them.
Nothing about `timeout:` behaves differently.

- **`chore verify-release` forces UTC when deriving the build date.** It rebuilds
  a published release and compares hashes, which is the whole basis for trusting
  the pipeline being optional — and it reported v0.10.0 as NOT reproducible.

  The release was fine. goreleaser stamps `.CommitDate` in UTC, and
  `git log --date=format-local:…%SZ` means *render in the local zone*, so an
  11:45 UTC commit was stamped `13:45` and labelled Z: two hours of difference in
  the ldflags, a different binary, and a false alarm from the one tool that
  exists to let somebody check the claim by hand. Wrong outside UTC since it was
  written, and green in CI because a runner's clock is already UTC — the worst
  available hiding place. With TZ forced, v0.10.0 and v0.9.0 both reproduce
  byte-for-byte.

- **`chore help timeouts` says what was observed of the out-of-process backstop,
  not what was assumed.** The page claimed both nets had been "observed working
  on one machine on one day". Half of that was measured: `timeout:` reclaimed a
  hung task's VM in 23 seconds. The guest-side deadline had been observed
  ARMING — four ways, including a `hold` that cancels it and a reboot that
  re-arms it — and confirmed from inside a guest as a poweroff scheduled for
  13:25, but nobody has watched it fire; that VM was taken down by hand as the
  target of the timeout test.

  Corrected because of where the sentence sits. It is the argument for keeping a
  net this feature makes look redundant, and an argument for trusting something
  is the worst place to claim more evidence than exists. The honest version is
  the stronger one: the inner net is proven to fire, the outer is so far only
  proven to be set.

## v0.10.0

- **`timeout:` and `on_timeout:` — the net for a task that hangs.**

      e2e:
        timeout: 20m
        on_timeout:
          - ./vm.sh destroy          # $TIMEOUT_PGID is the hung process GROUP
        cmds:
          - vagrant up
          - defer: vagrant destroy -f
          - ./run-tests.sh

  `defer:` covers a task that ENDS, normally or with an error. It runs when the
  task reaches the step that registered it — so it does not cover a task that
  HANGS, and a hang is the common case: a build stalled on a lock, an ssh that
  never returns, a test deadlocked. A hung task reaches nothing, so nothing it
  registered runs.

  Measured on 2026-09-07: a task booted a Vagrant VM, the task's process was
  killed, and nothing tore the VM down. It held a global lock for **thirteen
  hours**. Three safety nets existed and all three were PASSIVE — a `defer:`
  waiting for the task to reach its step, a reaper waiting for a later `chore`
  invocation, a lock staleness check waiting for another repository to try the
  lock. Each waited for somebody else to act, and that night nobody did.

  When the budget is spent, the handler runs FIRST — while the hung process group
  is still alive — then chore SIGTERMs that group, SIGKILLs after a 2s grace
  whatever ignored it, unwinds the `defer:` steps and outcome hooks exactly as
  after a Ctrl-C, and the task fails with exit **124**, the status `timeout(1)`
  uses for the same event. `{{.EXIT_CODE}}` in `after` reads it.

  Two details are the whole reason this is not `context.WithTimeout`:

  - **The handler is given a process GROUP, not a pid.** `vagrant up` forks qemu
    and virtiofsd; killing the pid orphans both, which is exactly how a qemu
    process ended up holding that lock with no parent left to clean up after it.
    `$TIMEOUT_PGID` is the group, handed over live, so
    `kill -TERM -"$TIMEOUT_PGID"` takes the tree. It is space-separated when
    concurrent `deps:` had several scripts in flight, and empty for an
    `interactive: true` task, which shares chore's own group by design.
  - **The clock is wall-clock from the start of the task**, not time since the
    last output. A hung process very often still logs — progress ticks,
    keepalives, retries — so an idle-output timer is silent on precisely the case
    worth catching.

  Nothing suppresses it: `--no-lifecycle` and `child_hooks: false` silence
  advice, and neither a safety net nor the teardown it triggers is advice — the
  same rule that keeps `defer:` alive inside a suppressed subtree. `on_timeout:`
  is therefore not one of the nine hooks. The budget covers the task's forward
  progress (its `before`, its `deps:`, its `cmds:`) and is switched off before
  the deferred steps unwind, because a deadline that killed the teardown it had
  just triggered would leave behind the very resource it fired to reclaim. The
  handler gets 60 seconds and so does the teardown behind it, since the handler
  runs before the kill and one that hung would defeat the timeout it serves. A
  handler that kills the group itself — the shape to write — does not cut that
  teardown short: the unwinding runs on a context chore's own cancellation cannot
  reach.

  Refused at load, not at runtime: `on_timeout` with no `timeout`, and a duration
  with no unit — `timeout: 30` is thirty of something, and a net that can be out
  by a factor of sixty is not one.

  A `defer:` that the hang would have swallowed runs after all, which is the part
  that surprises: a deferred step is registered positionally, so a task that
  never returns never unwinds — until the budget ends the hang. A hang was the
  one case where `defer:` was unreachable, and with a `timeout:` it no longer is.

  Verified against the case that motivated it rather than a stand-in: a task
  holding an already-booted btrfs oracle VM, hanging without booting anything —
  the failure that beat every existing net, because the process is alive so a
  liveness check correctly declines to reclaim, and `defer:` never fires because
  the body never returns.

      qemu before: 1
      chore: holds-a-vm: timed out after 20s — signalling process group 19781
      handler: pgid=[19781] spent=20s
      ==> default: Force killing QEMU process (pid=21655)
      ==> default: virtiofsd stopped
      after, exit 124
      elapsed: 23s
      qemu after: 0
      slot: the oracle slot is free

  Twenty-three seconds against a twenty-second budget. The group was live when
  the handler ran, so `vm.sh down` reached the guest and halted it properly
  rather than orphaning qemu — which is the entire reason the handler runs before
  the signal. The scenario that cost thirteen hours, closed in 23 seconds.

  **It does not make an out-of-process backstop redundant**, and it will look as
  though it does. A timer dies with the process that owns it: SIGKILL chore, or
  lose the machine, and nothing fires. The thirteen-hour lock above was held by a
  VM whose parent had already been killed. Keep the dumb net too — a guest-side
  deadline, the VM scheduling its own poweroff at boot, survives the host dying
  outright. `timeout:` is the fast, precise net; something outside the process is
  the unkillable one. Both, not either. `chore help timeouts`.

  What has been observed, stated exactly, because the two halves are not equally
  proven: `timeout:` reclaimed a hung task's VM in 23 seconds, measured. The
  guest-side deadline has been observed ARMING — four ways, including a `hold`
  that cancels it and a reboot that re-arms it — and confirmed from inside a
  guest booted at 11:24 as a poweroff scheduled for 13:25; its firing has not
  been watched, that VM having been taken down by hand at 12:58 as the target of
  the timeout test. So the inner net is proven to fire and the outer one is so
  far only proven to be set — worth knowing before leaning on it, and a reason
  to watch one fire on purpose rather than a reason to drop it.

## v0.9.0

- **`interactive: true` gives a task the terminal.**

      claude:login:
        interactive: true
        cmds:
          - 'claude setup-token; read -rs token; ...'

  Until now a task could not prompt. `exec.Cmd` with a nil `Stdin` wires the
  child to /dev/null, and chore never set one — so `read -rs token` returned EOF
  at once and the script carried on with an empty answer it never received.

  The second half was worse to diagnose. Every task runs in its OWN process
  group, which is what lets Ctrl-C kill what the script started rather than only
  the shell. But a child in its own group is a BACKGROUND group as far as the
  terminal is concerned: it cannot take the foreground, so a full-screen program
  draws nothing and reading /dev/tty raises SIGTTIN. Measured on a real
  Taskfile, `chore claude:login` printed its banner, sat silent while `claude
  setup-token` ran invisibly, and only flushed the TUI when the user pressed
  Ctrl-C.

  So an interactive task gets a real stdin AND shares chore's process group, and
  cancelling it signals the process rather than the group. That is a genuine
  loss — what that task starts is no longer swept up by Ctrl-C — which is why it
  is opt-in per task rather than detected: a task that silently changed its
  cleanup guarantee depending on whether output was piped would be worse than
  one that declares itself.

  A `sh:` capture ignores the flag. A captured value is chore reading a command,
  not a human answering one, and an up-to-date check that swallowed the
  keystrokes meant for the prompt would be a very quiet bug.

## v0.8.0

- **A built-in manual, extracted from the source.**

      chore help              a contents page of every topic
      chore help hooks        read one

  Topics are written as standalone `chore:manual` comment blocks sitting beside
  the code that implements them:

      // chore:manual hooks
      // title: Hooks
      // summary: before/on_success/on_failure/after, on a task or the whole run
      // aliases: lifecycle-hooks, lifecycle
      // order: 10
      //
      // # Hooks
      // ...markdown...

  `chore manual` regenerates `internal/manual/topics/*.md`, which are embedded
  into the binary; CI regenerates and fails on a diff. That diff is the only
  thing that actually keeps a document in sync with behaviour — beside the code
  is not enough on its own, it just makes the drift a one-line fix instead of a
  rewrite.

  Eleven topics ship, covering the whole command surface: `invocation`, `flags`,
  `arguments`, `variables`, `hooks`, `up-to-date`, `includes`, `dotenv`,
  `versions`, `interrupts`, `manual`. Names accept hyphens or underscores
  interchangeably, and carry aliases — `chore help sources`, `chore help args`,
  `chore help ctrl-c` all land somewhere useful — because the reader types the
  phrase they remember, not the one that was filed.

  Two rules the format enforces rather than documents. A topic with no
  `summary:` is refused, since it would be a blank line on the only page anyone
  browses. And a `chore:manual` marker that is not the FIRST line of its comment
  block is an error: written directly under an existing doc comment the two
  merge, the marker stops being first, and the topic vanishes from the manual
  without a word. That happened once, to `includes`, during this change.

- **Curated hook examples with golden output**, under `examples/hooks/`. Ten
  taskfiles, one rule each, and a recorded transcript of exactly what each
  prints. `go test ./examples` verifies them; `-update` re-records. They are the
  test suite for the documentation: a comment claiming `after` runs on both
  paths is a claim, and `02-failure.golden` is evidence.

- **BREAKING: `lifecycle.on_error` is now `lifecycle.on_failure_all`.** No alias,
  no deprecation window. Every global hook is now its per-task name plus `_all`,
  and `on_error` was the one that broke the pattern. Nothing on disk used it —
  measured: zero occurrences across every `chores.yml` on the machine, six
  internal references — so the rename is free now and never will be again. A file
  written against the new names states `chore_min_version: 0.8.0`, which turns an
  older chore's confusing `unknown field "on_failure_all"` into a message that
  says what to do.

- **Per-task lifecycle hooks: `before`, `on_success`, `on_failure`, `after`.**
  The same four names the `lifecycle:` block uses, minus the `_all` that marks a
  hook as per-invocation, on any task:

      build:
        before:     [ ./check-toolchain.sh ]
        cmds:       [ make ]
        on_success: [ ./publish.sh ]
        on_failure: [ ./collect-logs.sh ]
        after:      [ 'echo "ended {{.EXIT_CODE}}"' ]

  `before` gates: if it fails, `cmds:` do not run, the task fails with the gate's
  status, and `on_failure` fires for it — the rule `on_failure_all` already
  followed for a failed `before_all`. The other three are best-effort and cannot
  change the exit status; `on_failure` in particular cannot swallow a failure.

  `after` runs **in addition to** the outcome hook, not instead of it. Without
  that, "always" would have to be written into both `on_success` and
  `on_failure`, which is the duplication `after` exists to remove.

  Hooks run in the TASK's scope — its variables, parameters and `dir:` — so
  `after: echo done {{.TARGET}}` reads the argument the task was called with.
  They fire wherever the task runs, as a dependency or a `- task:` step included,
  and they run **even when the task is up to date**, because a hook is not the
  task's prerequisite. That last point is the whole reason `before` is not a
  slower spelling of `deps:`.

  Order, with the defers unwinding before the outcome branch so a finishing hook
  runs once the thing it is finishing is already down:

      before -> deps -> cmds -> defers (reverse) -> on_success|on_failure -> after

- **`{{.EXIT_CODE}}` in `after` and `after_all`** — `"0"`, or the task's own
  status. Exported to the script as `$EXIT_CODE` too, from the same value. Only
  those two hooks get it: `on_success`/`on_failure` already know the outcome by
  having been chosen, and `before_all` runs when there is no outcome yet.

- **`lifecycle.on_success_all`**, so the run level has the same four hooks the
  task level does.

- **`child_hooks: false` silences a whole subtree**, for a coordinator that does
  something once for a tree instead of letting every task in it do its own
  version:

      build:all:
        child_hooks: false
        deps:  [ prep ]
        cmds:  [ {task: driver}, {task: driver} ]
        after: ./sweep.sh          # once, not once per driver

  It leaves the declaring task's own hooks alone — a task that did not want those
  would delete them; what cannot be deleted is the tree below, because the same
  library task is right to run its hooks when it is the top of a run and wrong
  when it is nested inside one, and only the caller knows which. It reaches every
  depth through `deps:` and `- task:` alike (a dep is just a task invocation), it
  cannot be lifted from inside the subtree, and it **never** suppresses `defer:` —
  which is what makes deep suppression safe, since all it can silence is advice,
  never a teardown paired with something already brought up.

- **`--no-lifecycle` now covers per-task hooks too.** It still leaves `deps:` and
  `defer:` alone: a dependency is a requirement and a deferred step is a paired
  teardown, while a hook is advice.

- **A `- defer:` inside any hook is now refused at parse time.** A hook runs to
  completion at one point in the task's life, so there is nothing for it to defer
  to; accepting it would leave the reader choosing between two wrong answers.

      taskfile: task "x": after step 1 is a `defer:` — a hook runs to completion,
      so there is nothing for it to defer to; put it in `cmds:`, or call a task
      that does


## v0.7.0

- **A templated `sources:` or `generates:` never went up to date.** The check
  side rendered its patterns; the SAVE side did not. `sources: ['src/*.{{.EXT}}']`
  was hashed with the braces still in it, so it matched no file and recorded the
  checksum of the EMPTY SET — which can never equal the checksum of the rendered
  set the next check computes. The task rebuilt on every invocation, for ever,
  and nothing anywhere said so.

      $ chore ta ; chore ta                    # 0.6.0
      echo built-a > out-a.txt
      echo built-a > out-a.txt                 # "is up to date" never appears

      $ chore ta ; chore ta                    # fixed
      echo built-a > out-a.txt
      task: ta is up to date

  Two tasks with DIFFERENT templated patterns stored the same hash, because both
  hashed nothing. That empty-set digest is the tell:

      0.6.0   ta  "hash": "4a45a2b2be26286502e3244aabe5757706383407ca0b46b957aac97e1acd4b9a"
              tb  "hash": "4a45a2b2be26286502e3244aabe5757706383407ca0b46b957aac97e1acd4b9a"
      fixed   ta  "hash": "33f764b7138f17cd62ed63b275b336fc8a98c7adee71f04d989d5d83d1049473"
              tb  "hash": "b232ae5ae8eb81cd354ff8b76bb5c867b4d955e389544096df0d0b1ceebf888d"

  `generates:` failed the same way and one step worse. A raw `out/{{.NAME}}.a`
  has no glob metacharacters, so it was treated as a NAMED file, stat failed, and
  the recorded output list came out empty — which made the "every file the last
  run produced must still exist" check, the one that catches a deleted binary
  under `generates: [bin/*]`, pass vacuously over nothing.

  `SaveWith` already took a `Renderer`, and the package doc on `Save` already
  warned about exactly this; `internal/run` was calling `Save`. It now passes the
  same scope the up-to-date check is given.

- **A value passed by `- task:` or `deps:` now binds under both spellings, as one
  typed at a prompt always has.** `internal/cli` writes a supplied parameter
  under the declared name AND its uppercase form. A call var did not, and the
  mirror that fills the other spelling only fills one that is EMPTY — so with an
  `OUT` defined anywhere lower (a file var, dotenv, the process environment) the
  caller's value reached `{{.out}}` and `{{.OUT}}` kept the OLD one. Not blank:
  wrong. The build lands in another directory, exit 0, nothing on stderr.

      vars: {OUT: /wrong/dir}          # anywhere below the call
      - task: staticlib
        vars: {out: /right/path}

      0.6.0   chore build                        {{.OUT}} = /wrong/dir
              chore staticlib out=/right/path    {{.OUT}} = /right/path
      fixed   both                               {{.OUT}} = /right/path

  Matching is case-insensitive, as `Args.Find` already was: `vars: {Out: ...}`
  against `args: [out]` was refused as a missing argument while the identical
  command line bound it. Only DECLARED parameters fold — an undeclared name is an
  ordinary variable and stays exactly as written, which is what the command line
  does with one too.

  One parameter given two different values under two spellings is now refused
  rather than silently ranked, which is the same call `checkArgConflicts` already
  made for the positional case:

      chore: staticlib: out given twice: "/b" as OUT and "/a" as out

  The fold happens in `Run`, the one point every `- task:` step, every `deps:`
  entry and the command line all arrive at, rather than by teaching the mirror
  another case — two call paths for one declaration is what allowed them to
  disagree in the first place.

## v0.6.0

- **Ctrl-C now stops the task, not just chore.** A task's script runs in its own
  process group (so that cancelling can kill what the script started, rather than
  only the shell). The terminal delivers SIGINT to the FOREGROUND process group
  only — which is chore, never the script — and chore installed no handler, so it
  died instantly from the default action while everything it started carried on.
  `chore app:run` exited and left `flutter run` holding the terminal; the same
  went for an emulator, a `docker logs -f`, a `go run` server.

  The machinery to stop them was already there and correct: `cmd.Cancel` kills the
  whole process group. Nothing ever triggered it, because nothing cancelled the
  context. Now SIGINT and SIGTERM do.

      chore: interrupted: stopped app:run and anything it started

  The exit code is 128+signal — 130 for Ctrl-C — which is what every shell reports
  for a signalled command. A SECOND Ctrl-C is deliberately not caught: someone
  pressing it twice has stopped waiting for a tidy shutdown.

- **Teardown survives an interrupt.** `defer:` steps, and the `after_all` and
  `on_error` lifecycle hooks, now run on a fresh context with a bounded budget
  when the run's own has been cancelled. `exec.CommandContext` refuses to START a
  process on a cancelled context, so passing it straight through would have
  skipped every teardown step at the one moment they matter most — Ctrl-C on a
  task that brought a topology up.

- **`internal: true` now refuses to run from the command line.** It hid a task
  from `--list` and stopped there, so the promise was documentation: `chore
  _prepare` ran the helper anyway, skipping whatever set its arguments up. This
  is parity with the format chore reads — go-task refuses an internal task too —
  and it is what makes a helper safe to factor out of two tasks.

      chore: _prepare is internal: another task can call it with deps: or
             `- task:`, but it cannot be run from the command line

  The ban is on the command line, not on the task: `deps:` entries and `- task:`
  steps are untouched, which is why the check lives in `Invoke` — the one entry
  point the CLI uses — rather than in `Run`, which every internal call goes
  through. An alias of an internal task is refused too; the rule cannot depend on
  how the name was typed.

  **One exception, and it is the reason internal helpers are useful for anything
  but side effects.** A `- task:` step returns nothing, so a helper that produces
  a *value* is invoked as `{{.CHORE_EXE}} _helper` from inside a task and its
  stdout captured by an `sh:` var. That is a command line, and refusing it broke
  the pattern silently — as a variable that would not resolve. `CHORE=1` is
  already exported to every task script, so its presence distinguishes chore
  calling itself from a person at a prompt. The rule is **callable by chore, not
  by a person**.

## v0.5.0

- **`chore_min_version`: a file can state the oldest chore that may run it.**

      version: '3'
      chore_min_version: 0.4.0

  Optional — absent means no restriction. It exists because a file's safety can
  rest on the RUNNER and not only on what the file says. A Taskfile driving a
  trading platform declared every dangerous flag as a string compared to
  `"true"`, and ordered its parameters so a stray value landed on a harmless
  one, for exactly one reason: chore < 0.4.0 bound an unknown `--flag`
  positionally and let a bool take any value, so `chore backtest --robot-name x`
  set `holdout=true` and spent a one-shot resource. Stating the floor is what
  lets that file drop the workarounds instead of carrying them forever.

      chore 0.3.0 is too old: /w/chores.yml requires chore_min_version 0.4.0.
        Upgrade with `brew upgrade chore`, or run an older copy of the file.

  Details: versions compare NUMERICALLY, so 0.10.0 satisfies a 0.4.0 floor —
  a string comparison gets that backwards. The strictest floor among the loaded
  files wins, includes included, and the message names which file asked. A dev
  build is exempt: it has no version to judge and already banners itself on every
  run. `--list` and `--help` still work, because someone staring at a refusal has
  to be able to read the file that caused it. A floor that is not a version is
  refused at decode time, where it is written.

  A chore too old to know the field refuses the file anyway — an unknown
  top-level key is an error — so this fails closed even against versions that
  predate it. It replaces a confusing message with an actionable one.

- **`{{.CHORE_EXE}}` and `{{.CHORE_VERSION}}`**: the binary actually running, and
  its version. A task that has to invoke chore — a launchd plist needs an
  absolute `ProgramArguments` path — otherwise resolves the word `chore` through
  PATH, which answers "the one I would get if I typed it", not "the one running
  me". Those are different files exactly when it matters: a file whose `env:`
  pins PATH, or a binary run from a checkout, silently writes the installed copy
  into the plist instead of itself. (Not `{{.CHORE}}` — that name is already the
  environment variable set to `1` so a Taskfile can tell this runner from
  go-task.)

## v0.4.0

- **Short flags: `-f` can mean `--force`.** A parameter opts in with `short:`:

      args:
        - {name: service, short: s}
        - {name: follow, short: f, type: bool}
        - {name: all, short: a, type: bool}

      chore logs -f              # a bool short is its own value
      chore logs -s api          # or -s=api
      chore logs -fa -s api      # bools bundle
      chore logs --follow        # unchanged

  Opt-in rather than derived from the name. A single-dash word is otherwise
  DATA — that is the whole reason `chore logs -f api` renders `docker logs -f
  api` — so deriving `-f` from `follow` would silently change what every
  existing file does, and `args: [force, follow]` would have no answer for `-f`
  at all. A file that declares no `short:` behaves exactly as before.

  Refused at decode time, where the mistake is written rather than at some later
  call: a short of more than one letter, a digit (`-5` is a negative number
  reaching an `int`), two parameters claiming the same letter, and `h` — chore
  answers `-h` as help before a task is invoked, so the parameter would be
  unreachable.

  Only bools bundle. `-sfa`, where `-s` takes a value, is refused rather than
  read as either `-s -f -a` or an `-s` whose value is "fa" — guessing is how a
  flag ends up set to a filename. And a task that declares any short has opted
  into short parsing, so a single-dash letter it does not know is an error
  naming the ones it has, instead of silently becoming a positional value.

- `chore <task> --help` no longer offers `--flag <value>` for a bool, which is
  now an error, and spells the flag the way a caller types it. It shows the
  short alias in both the parameter list and the call forms:

      arguments:
      dry_run (-d)  bool, optional — decide and journal, but place no order

      called as:
      flag         chore tick --dry-run
      short flag   chore tick -d

- **A `type: bool` parameter now only takes a boolean.** It was the one declared
  type nothing validated: `checkArgType` rejects a non-numeric `int`, but
  returned nil for a bool, and `NormalizeBool` reads everything outside
  `{"", "0", "false", "no", "off"}` as true. So any word bound to a bool set it —
  `chore deploy typo` switched on `live`, and so did `chore deploy -x`,
  `--live=maybe` and `LIVE=maybe`.

  This is what made single-dash flags appear to work. chore has no short-flag
  syntax, so `-f` is data, and data binds by POSITION: with `args: [f, a, b, c]`
  all bool, `chore t -f -a -b -c` set all four — but so did `-c -b -a -f`, and
  `-c` alone set `f`. The letters were never read; coercion hid it by answering
  true either way.

  Non-boolean values are now refused, naming the flag spelling to use instead,
  and saying why a single-dash word did not do what it looked like:

      task deploy: live must be true or false, got "-x"; a flag is supplied as
        --live (a single-dash word is data, and binds by position, not by letter)

  Checked where each value still exists as text: positionals in `checkArgType`
  (exit 1, beside the int check), and `--live=X` / `LIVE=X` in `splitArgs` before
  `NormalizeBool` collapses them (exit 2, with the other usage errors). The
  accepted vocabulary is `1 true yes on` / `0 false no off`, empty, any casing.
  Untouched: `--live` on its own, a bool's default from `vars:`, and single-dash
  words reaching an untyped parameter — `chore logs -f api` still renders
  `docker logs -f api`.

- The refusal message now suggests parameters in the spelling a caller would
  type — `--dry-run` for a `dry_run`, since v0.3.0 made both reach it — rather
  than echoing the underscored declaration.
- Corrected the v0.3.0 note below: it cited a `--robot-name` typo as setting
  `holdout` and `force`. Re-measured against that file, it bound the literal
  string `--robot-name` to `robot`. The mechanism and the danger are unchanged —
  a typo silently satisfies whichever parameter comes first — but the example
  was not reproducible as written.

## v0.3.0

- **A mistyped `--flag` no longer switches on a different one.** A `--word` that
  named no declared parameter fell through the lookup in `splitArgs` and was
  appended as a POSITIONAL, so it bound to whatever the task declares first — and
  for a `type: bool` parameter `NormalizeBool` reads anything outside
  `{"", "0", "false", "no", "off"}` as true, the flag's own text included.
  Measured on a Taskfile driving a trading platform, whose `tick` declares
  `dry_run` then `force`: `chore tick --total-nonsense` rendered `main.py tick
  --dry-run`, a flag nobody asked for, and the same file's `backtest` bound the
  literal string `--robot-name` to its `robot` parameter. Which parameter gets
  hit is just declaration order, so the same typo lands on a `live` or a
  `holdout` in any file that declares one first. It is the same failure that once
  made `chore instance:up --help` START a stack, fixed then for `--help` alone;
  this generalises it. Such a word is now refused when it is bound, naming the
  parameters the task does declare:

      chore: backtest: task backtest: --robot-name is not one of its parameters
        (--holdout, --force); to pass it along as data instead:
        chore backtest -- --robot-name

  Deliberately narrow, so the two things that relied on the old rule still work:
  single-dash words are untouched (`chore logs -f api` passes `-f` to the task,
  and its unit test pins that), and `--` still hands everything after it over as
  `CLI_ARGS`. Only a leftover long flag — the shape that is a typo essentially
  every time — is refused. Exit 1, a task-level error like any other bad
  argument; 2 remains chore's own flag parsing.

- **`--train-bars` now reaches a parameter declared `train_bars`.** A declared
  name cannot contain a hyphen — it has to be usable as `{{.train_bars}}`, and the
  loader rejects one that is not — so a two-word parameter is always underscored
  in the file, while the command line convention is the opposite. The lookup was
  `strings.ToLower(name)` with no folding, so `--train-bars` matched nothing and
  (before the fix above) became a positional: with `type: int` that surfaced as
  `train_bars must be a whole number, got "--train-bars"`, and for a string or
  bool it bound silently. Hyphens now fold onto underscores, in any casing, so
  `--train-bars`, `--train_bars` and `--TRAIN-BARS` all reach the parameter.

## v0.2.2

- **Release pipeline fix (follow-up to v0.2.1).** The reproduce step downloaded the
  goreleaser binary, tarball and checksums into the repo root; goreleaser reads that
  as a dirty git state and refuses to build ("git is in a dirty state"). They are
  now fetched and unpacked in a temp dir outside the working tree — `dist/` and
  `binaries.txt` are already gitignored, these were not. (v0.2.1 fixed the PATH
  lookup but introduced this; the release still published — only the self-check
  failed. Verified locally: the reproduction now rebuilds all four published
  binaries byte-for-byte from a clean tree.)

## v0.2.1

- **Release pipeline fix.** The "reproduce the release" step failed with
  `goreleaser: command not found`. goreleaser-action installs goreleaser for its
  own step but does not leave it on PATH for later steps — and once GitHub
  force-migrated the pinned (Node 20) action onto Node 24, that PATH export stopped
  persisting. The step now fetches the same pinned goreleaser **release binary**
  (checksum-verified against goreleaser's own `checksums.txt`) and runs that, so it
  no longer depends on the action's PATH. A prebuilt binary rather than
  `go run …goreleaser@ver`, because compiling goreleaser needs a newer Go than the
  `GOTOOLCHAIN` this step pins to reproduce the chore binaries byte-for-byte. The
  goreleaser version is now held once in a job-level `GORELEASER_VERSION` so the
  publish and reproduce steps can never drift onto different versions. (The publish
  itself was unaffected; only the self-check step broke.)

## v0.2.0

- **New: `lifecycle:` hooks.** A top-level block with `before_all`, `after_all`
  and `on_error`, run once *around* the task named on the command line — chore's
  own extension, with no Task equivalent. It lets a project run setup/teardown for
  a run without wiring a dependency into every task, and — the reason it beats a
  `deps:` entry — it fires even when the task it wraps is up to date, because it is
  not that task's prerequisite (a dep would be skipped along with the task).
  `before_all` is a gate: if it fails, the task does not run and neither does
  `after_all`. Hooks are skipped for `--list`/`--help`/`version` (which run no
  task) and can be turned off for a run with `--no-lifecycle`. `{{.TASK}}` inside a
  hook is the invoked task's name. Built for self-installing repo guards:
  `before_all: [{task: hooks:ensure}]` activates a repo's git hooks the first time
  anyone runs any task.

- The release is built by goreleaser instead of a hand-written build matrix. Same
  four artifacts under the same names, the same pinned toolchain and flags, and the
  release still rebuilds every target afterwards and fails if a byte differs. Two
  things this buys that the old pipeline did not have: archive mtimes now come from
  the commit, so the tarballs are byte-stable and not just the binaries inside them,
  and one runner cross-builds all four targets instead of four runners each building
  one.

  **The build date is now stamped in UTC**, where the old pipeline kept the
  committer's local offset — the same instant, but a different string, so a
  different hash. Verifying a release built before this change needs the old
  derivation; `chore verify-release` tries both and says which one reproduced.

- `chore verify-release` never reproduced v0.1.3. It rebuilt without
  `-X main.buildDate`, which that release stamps, so it reported a mismatch for a
  release that was in fact reproducible.

## v0.1.3

- `--help` is global and answers about whatever the command line names: the program
  when no task is given, that task when one is, from either position. It used to be
  swallowed as a task argument, so `chore instance:up --help` STARTED a stack. A task's
  help is built from its own declarations — the `desc`, each parameter's type,
  whether it is optional, and the four ways it can be called. After `--` the words
  still belong to the task, so a command that must pass `--help` through can.

- `--version` reports when the source was committed, with its age:
  `dated 2026-07-28 11:21 UTC (9 minutes ago)`. The COMMIT's date, not the build
  machine's clock — a wall-clock stamp would make two builds of the same source
  differ, and the release proves on every tag that they do not. The age is computed
  when you run it; only the stamp is fixed.

## v0.1.2

Everything here was found by driving a real 197-task project, not by reading code.

**Fixed — names resolved to the wrong thing**

- An included task ran in the directory of its own file, so relative paths pointed
  one level down: `-v $(pwd):/app` mounted `tasks/` as the application. Tasks now run
  at the project root; an include's `dir:` is the one thing that moves them.
- `- task:` and `deps:` resolved globally. A reference is relative to its file, so
  `- task: deps` means that file's `deps`; `:name` escapes to the root.
- An include's `vars:` were resolved in the child, where an include sees only what
  was mapped to it, so `IP: '{{.POSTGRES_IP}}'` rendered empty and a container
  started with no address. They are now resolved in the file that wrote them.
- Variables typed on the command line were scoped to the first task, so
  `down CONFIG=mail1` never reached the tasks `down` calls — the children matched no
  container and reported success having stopped nothing. They are now global to the
  run and outrank the file; values a parent passes explicitly still win.
- `env:` parsed and was documented but never reached the shell, so `>$OUTPUT` was an
  ambiguous redirect and `sh:` under an `env:` key never ran.

**Added**

- `inherit: true` on an include: the including file's variables come with it, as a
  layer below the file's own. Off by default.
- `dotenv:` on a task, replacing its file's; `dotenv: []` declines it.
- `CHORE_BIN`, the running binary's path, so a task driving another project uses the
  runner that is executing.
- `--version` prints the bare version on stdout and the commit, toolchain and
  resolved `chores.yml` on stderr. The version is read from the build itself, so a
  local build reports `dev+<sha>` with nothing passed to it, and says so above each
  run.
- `--no-color`, plus colour and column alignment for the listing and diagnostics on a
  terminal. Piped output is byte-identical to before, and widths are counted in
  display cells so a name outside ASCII no longer skews the columns.

**Changed**

- A bad key names itself rather than a Go type: `unknown field "dotenv" in a task`.

## v0.1.1

First reproducible release: same source, compiler and flags produce byte-identical
binaries, and the pipeline rebuilds every artifact to prove the published hash.

## v0.1.0

Initial release. Reads go-task's file format, with real arguments for tasks.
