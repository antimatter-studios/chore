# go-tsk

A task runner that reads `Taskfile.yml` and lets a task take arguments.

```bash
tsk --list                        # every task, grouped, from its own desc
tsk build                         # run one
tsk up mail4.test                 # positional argument
tsk config:check CONFIG=mail4.test # or a variable, bound before dotenv resolves
```

Binary: `tsk`. macOS and Linux.

## Why it exists

[go-task](https://taskfile.dev) has a good file format and three behaviours that
make it unusable as a project's control plane. Each was hit in practice, not
imagined:

**A task cannot take an argument.** Bare words after a task name are more task
names — make's grammar, inherited. So a value has to arrive as an environment
variable set *before* the command: `CONFIG=mail4.test task up`.

**Variables passed on the command line do not reach `dotenv:`.** Task resolves
`dotenv:` while parsing, before CLI variables are merged. So
`task up CONFIG=mail4.test` — valid Task syntax, the form its own documentation
shows — loads the **default** config's environment and acts on the wrong stack,
silently and with exit 0. The only defence is a guard task that rejects the
syntax.

**A missing `dotenv:` file is skipped in silence.** Every variable falls back to
a default, container names resolve to `-suffix`, filters match nothing, and
commands report success.

Plus: **included files' variables are flattened into the parent's namespace**, so
two includes can quietly overwrite each other's values.

## What it does differently

- **`args:` on a task** declares its parameters. One declaration, four call forms:
  `tsk up`, `tsk up mail4.test`, `tsk up --config mail4.test`, `tsk up CONFIG=mail4.test`.
  Required-ness follows from the declaration — no default means required, a default
  in `vars:` means optional, and `vars: {x: ""}` means optional with empty being
  meaningful. No marker syntax: the default says it.
- **One resolution order, evaluated once at invocation** — arguments, call vars,
  task vars, file vars, dotenv, environment — with arguments bound *before* dotenv
  paths are rendered, so `dotenv: ['config/{{.CONFIG}}/config.env']` works from
  the command line.
- **A config with no environment is an error.** A partial miss (no `secrets.env`)
  is reported and continues; `?` on a path silences it.
- **Includes see only the variables mapped to them.** Nothing bleeds.
- **The system shell runs scripts**, so `set -o pipefail` works — Task's embedded
  interpreter does not implement it, and a failing pipeline there reports success.
- **No multi-target invocation.** `tsk a b` does not mean "run a then b"; that is
  `tsk a && tsk b`, which is what people type anyway. Giving up the make grammar is
  what buys arguments.

## Supported subset

The feature set was measured against a real 3,131-line Taskfile rather than
guessed: `desc`, `cmds` (strings, `- task:` references, multi-line blocks),
`vars` (including `sh:`), `deps` (concurrent), `env`, `dotenv`, `includes`
(`taskfile`/`dir`/`vars`/`optional`/`flatten`), `silent`, `internal`, `dir`,
`run: once`, `defer`, `status`, `sources`/`generates` (content-hash up-to-date
checks), `aliases`, `ignore_error`, `requires`, `platforms`. Templating is Go
`text/template` plus one function, `default`.

Not supported, on purpose: remote includes, `watch`, `for:`/matrix, `prompt`,
`interactive`, output styles, v2 schema, Windows. See [SPEC.md](SPEC.md).

## Build

```bash
go build -o bin/tsk ./cmd/tsk
go test ./...
```

## Status

Early. It runs a real 153-task Taskfile unmodified — `tsk --list` matches
`task --list` exactly — including a 193-line shell body with nested template
escaping, dependency fan-out, and `sources:`-gated builds. Two behaviour
differences are documented in [SPEC.md](SPEC.md#deviations-found-while-running-rest-mails-taskfile);
both come from using the real shell.
