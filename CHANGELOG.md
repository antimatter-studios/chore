# Changelog

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
