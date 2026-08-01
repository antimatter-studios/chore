# Changelog

## Unreleased

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
