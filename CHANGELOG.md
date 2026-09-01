# Changelog

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
