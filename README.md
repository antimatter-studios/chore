# chore

The commands you keep forgetting, in a file, with real arguments.

A task runner that reads `chores.yml` — go-task's format under a name of its own —
and lets a task declare parameters:

```bash
chore --list
chore build
chore instance:up mail4.test
chore instance:up --config mail4.test
```

## Install

```bash
brew install antimatter-studios/tap/chore
# or
go install github.com/antimatter-studios/chore@latest
```

```bash
chore --list                        # every task, grouped, from its own desc
chore build                         # run one
chore up mail4.test                 # positional argument
chore config:check CONFIG=mail4.test # or a variable, bound before dotenv resolves
```

Binary: `chore`. macOS and Linux.

### Running a local build

To try a change without installing it, source the dev script. It builds
`./bin/chore` and puts that directory first on PATH **for the current shell**.
The binary reports itself as `dev+<sha>` (`-dirty` with uncommitted changes) and
prints that above every command it runs, so a local build can never be mistaken
for the installed one:

```bash
source scripts/install-chore-dev              # build, then shadow the installed chore
source scripts/install-chore-dev --no-build   # shadow what is already built
source scripts/uninstall-chore-dev            # back to the installed one
```

Source them, don't run them — a child process cannot change its parent's PATH,
so executing them would export into a shell that immediately exits (they say so
and refuse). Nothing is written outside the repo, no symlink lands in a shared
prefix, and the installed build is left alone: undo is the uninstall script, or
just a new shell.

## Why it exists

**go-task is a good tool.** Its file format is good enough that `chore` reads it
rather than inventing another one, and if a project's tasks are "build, test,
lint" it is the right answer. This is not a criticism of the tool; it is a
statement about what the tool is *for*.

The problem is lineage. go-task is a make descendant, and make's grammar is
built around **targets**, not commands: a bare word after the program name is
another target to build. That single inherited decision means a task can never
take an argument — so the moment your tasks stop being "build the project" and
start being "operate this thing", the model fights you. Everything below follows
from it, measured while driving a 153-task, 3,131-line Taskfile that runs a mail
server:

- **A task cannot take an argument.** `task up mail4.test` asks for a task named
  `mail4.test`. The value has to arrive as an environment variable set *before*
  the command: `CONFIG=mail4.test task up`. Every wrapper, alias and README line
  inherits that shape.
- **Command-line variables do not reach `dotenv:`.** go-task resolves `dotenv:`
  while parsing, before CLI variables exist, so `task up CONFIG=mail4.test` —
  valid syntax, the form its own docs show — loads the *default* config's
  environment and acts on the wrong stack, silently, exit 0. The only defence was
  a guard task that rejected the syntax outright.
- **A missing `dotenv:` file is skipped in silence.** Every variable falls back to
  a default, container names resolve to `-suffix`, filters match nothing, and
  commands report success.
- **Included files' variables are flattened into the parent namespace**, so two
  includes overwrite each other. One peer repository could not be included at
  all; it had to be shelled out to with `task -d`.
- **The shell is an embedded reimplementation** (mvdan.cc/sh, there for Windows
  support), which does not implement `set -o pipefail` — a failing pipeline
  reports success — and whose `printf` pads by runes rather than bytes, so
  carefully aligned output drifts. A Taskfile can become quietly dependent on
  that interpreter's quirks.

None of that makes go-task bad at what it is. It makes it a poor foundation for a
**command-line tool that other people run**, which is what a project's task
surface becomes once it has more than a handful of verbs.

## Why not just

[just](https://github.com/casey/just) is the obvious alternative and it fixes the
biggest problem outright: recipes take positional parameters. It was evaluated
seriously — a partial conversion of the same project was written and run. Four
things ruled it out here:

- **No composition across repositories.** `import` and `mod` read *justfiles*
  only. 27 of those 153 tasks belong to peer repositories that ship Taskfiles, so
  adopting just meant hand-writing 27 wrappers around `task -d …` — a facade over
  the tool being replaced.
- **No parallel dependencies.** Dependencies run sequentially. Bringing up a
  stack leans on concurrent `deps:`.
- **No up-to-date checks.** No equivalent of `sources:`/`generates:`, which gate
  17 tasks here.
- **Every recipe line runs in its own shell.** Multi-line logic needs a shebang
  recipe, or the `[script]` attribute — which still required `set unstable` in
  1.57. `set -e`, a variable assignment, or a sourced env file otherwise does not
  survive to the next line.

Two smaller cuts: a stray comment above a recipe silently becomes its
description, and `just` is not YAML, so the migration was a 3,131-line rewrite
rather than a rename.

## Why not mise

[mise](https://github.com/jdx/mise) is excellent, and the most-installed of the
three by a wide margin — but read what it is: *"dev tools, env vars, task
runner"*, in that order. Its install base is overwhelmingly people replacing asdf
for toolchain versions, which is its strongest surface and a genuinely different
job from this one. Adopting it as a task runner means betting on its least-used
feature, and it is not Taskfile-compatible either, so the migration cost matches
just's.

Its actual strength is worth stealing separately: pinning toolchain versions per
project, which is a class of drift this project hit twice.

## What this is instead

Deliberately smaller than all three. `chore` reads go-task's format, so adopting
it costs a rename; it supports the features one real project measurably uses and
nothing else; and it targets macOS and Linux only, which is what makes using the
real shell — with real `pipefail` — possible.

## What it does differently

- **`args:` on a task** declares its parameters — `- config` for a plain one, or
  `{name: follow, short: f, type: bool, desc: …}` when it is a flag, takes an
  int, wants a short alias, or wants a description. One declaration, five call
  forms: `chore up`, `chore up mail4.test`, `chore up --config mail4.test`,
  `chore up -c mail4.test`, `chore up CONFIG=mail4.test`. Bools bundle: `-fab`.
  A two-word parameter is `train_bars` in the file and `--train-bars` or
  `--train_bars` on the command line. A `--flag` the task does not declare is a
  usage error naming the ones it does, so a typo cannot quietly become the value
  of another parameter; `chore up -- --raw` still passes words through verbatim.
  Required-ness follows from the declaration — no default means required, a default
  in `vars:` means optional, and `vars: {x: ""}` means optional with empty being
  meaningful. No marker syntax: the default says it.
- **`chore_min_version:` on the file** states the oldest chore that may run it.
  Optional; absent means no restriction. For a file whose safety depends on the
  runner — chore < 0.4.0 rebound a mistyped flag onto another one — it is the
  difference between carrying a workaround forever and saying what you need.
- **One resolution order, evaluated once at invocation** — arguments, call vars,
  task vars, file vars, dotenv, environment — with arguments bound *before* dotenv
  paths are rendered, so `dotenv: ['config/{{.CONFIG}}/config.env']` works from
  the command line.
- **A config with no environment is an error.** A partial miss (no `secrets.env`)
  is reported and continues; `?` on a path silences it.
- **Includes see only the variables mapped to them.** Nothing bleeds.
- **`lifecycle:` hooks run once around a whole invocation**, not per task —
  `before_all`, `after_all`, `on_error`. One block covers every task instead of
  wiring a `deps:` entry into each, and it fires even when the task it wraps is up
  to date (a dependency would be skipped along with the task). `before_all` is a
  gate — if it fails, the task never starts. The use it was built for: a repo that
  installs its own git hooks the first time anyone runs any task,
  `before_all: [{task: hooks:ensure}]`, with no per-task boilerplate. Off for a run
  with `--no-lifecycle`; never runs for `--list`/`--help`.
- **The system shell runs scripts**, so `set -o pipefail` works — Task's embedded
  interpreter does not implement it, and a failing pipeline there reports success.
- **`chore <task> --help` describes the task and runs nothing.** Built from the
  task's own declarations: its `desc`, each parameter's type and whether it is
  optional, and the call forms. Works before or after the task name.
- **The running binary identifies itself**, including how old it is:
  `dated 2026-07-28 11:21 UTC (9 minutes ago)` — the date the SOURCE was committed,
  not when a machine compiled it, because a wall-clock stamp would break the
  byte-for-byte rebuild the release pipeline verifies. `chore --version` prints the bare
  version on stdout — unchanged, so anything parsing it still works — and the
  build's commit, toolchain and the `chores.yml` it found on stderr. The version
  is read out of the build itself (`runtime/debug`), not hardcoded or passed in by
  whoever compiled it: a plain `go build` in a checkout reports `dev+<sha>`, a
  release reports its tag. A dev build also prints a one-line banner to stderr
  above each run; a release stays silent.
- **Output that suits where it is going.** The listing and diagnostics are
  coloured and column-aligned on a terminal, and fall back to exactly the plain
  text they always were on a pipe, in a CI log, under `NO_COLOR`, or with
  `--no-color`. Widths are counted in display cells, so a task name outside ASCII
  does not push the descriptions out of column. A task's OWN output is never
  touched — child processes write straight to the same stdout, so their colours,
  ordering and progress bars arrive verbatim.
- **A name resolves where it is written.** A `- task:` step or `deps:` entry is
  relative to its own file, so `- task: deps` means that file's `deps`, never a root
  task of the same name; a leading colon (`:build`) escapes to the root. An
  include's `vars:` are rendered in the file that WROTE them, so
  `IP: '{{.POSTGRES_IP}}'` means the parent's value. And an included task runs at
  the project root unless its include says `dir:` — `{{.TASKFILE_DIR}}` is how you
  ask for the file's own directory.
- **Variables you type outrank the file, everywhere in the run.** `chore down
  CONFIG=mail1` reaches the tasks that `down` calls, not just `down`. Values a
  parent passes explicitly to a child still win, so a task that brings up two
  servers by naming each is not collapsed into one.
- **`inherit: true` on an include** brings the including file's variables in, as a
  layer below the file's own. Off by default: an include sees the outside world and
  what was mapped to it, and nothing else.
- **`dotenv:` on a task** replaces its file's, and `dotenv: []` declines it — for a
  task whose job is to hand off to another project that owns its own config.
- **No multi-target invocation.** `chore a b` does not mean "run a then b"; that is
  `chore a && chore b`, which is what people type anyway. Giving up the make grammar is
  what buys arguments.

## Verifying a release

Every release is **reproducible**: the same source, compiler and flags produce
byte-identical binaries, so you do not have to trust the pipeline that built them
— you can rebuild and compare.

```bash
chore verify-release 0.1.0 darwin arm64
```

```
  published: 9bc094c15e4286137a146e9e21501d9a2b2d28c4f241e785d53622a640dcd005
  rebuilt:   9bc094c15e4286137a146e9e21501d9a2b2d28c4f241e785d53622a640dcd005
  ✓ reproduced — the release matches its source
```

Or by hand, if you would rather not run the thing you are checking:

```bash
git checkout v0.1.4
# The binary is stamped with the COMMIT's date in UTC, so the rebuild has to
# derive the same string — a wall-clock date would never match.
date=$(git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd)
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 GOTOOLCHAIN=go1.25.0 \
  go build -buildvcs=false -trimpath \
    -ldflags "-s -w -X main.version=0.1.4 -X main.buildDate=$date" -o chore .
shasum -a 256 chore
# compare with binaries.txt from the release
```

Releases up to and including v0.1.3 were built by a hand-written pipeline that
derived that date with `git log -1 --format=%cI`, which keeps the committer's
local UTC offset instead. Same instant, different bytes, different hash — so
verifying one of those releases needs `%cI` above. `chore verify-release` tries
both recipes and reports which one reproduced.

Four details make this work, and all four are load-bearing:

- **`GOTOOLCHAIN` is pinned to a patch release.** go1.25.0 and go1.25.6 emit
  different binaries; pinning only the minor version is not enough.
- **`-buildvcs=false`.** Go otherwise embeds `vcs.revision` and `vcs.time`, which
  differ for every build and are the sole reason an otherwise identical rebuild
  produces a different hash. The commit is recorded in the release itself, where
  it can be verified rather than merely read out of a binary.
- **The build date comes from the commit, never the clock.** Otherwise every
  rebuild differs by construction, and none of the above matters.
- **`binaries.txt` alongside `checksums.txt`.** `tar` records mtimes and
  ownership. goreleaser is configured to take archive mtimes from the commit too,
  so the tarballs are stable as well — but the binary hash is the one that does
  not depend on how the release was packaged, so that is what to compare.

The release workflow proves this on every release rather than claiming it: after
publishing, it rebuilds all four targets from the same commit and fails if any
byte differs.

## Supported subset

The feature set was measured against a real 3,131-line Taskfile rather than
guessed: `desc`, `cmds` (strings, `- task:` references, multi-line blocks),
`vars` (including `sh:`), `deps` (concurrent), `env`, `dotenv` (per file OR per
task), `includes` (`taskfile`/`dir`/`vars`/`optional`/`flatten`/`inherit`),
`silent`, `internal`, `dir`,
`run: once`, `defer`, `status`, `sources`/`generates` (content-hash up-to-date
checks), `aliases`, `ignore_error`, `requires`, `platforms`. Templating is Go
`text/template` plus one function, `default`.

Not supported, on purpose: remote includes, `watch`, `for:`/matrix, `prompt`,
`interactive`, output styles (go-task's group/prefixed task output), v2 schema, Windows. See [SPEC.md](SPEC.md).

## Build

```bash
go build -o bin/chore .
go test ./...
```

## Status

Early. It runs a real 153-task Taskfile unmodified — `chore --list` matches
`task --list` exactly — including a 193-line shell body with nested template
escaping, dependency fan-out, and `sources:`-gated builds. Two behaviour
differences are documented in [SPEC.md](SPEC.md#deviations-found-while-running-rest-mails-taskfile);
both come from using the real shell.
