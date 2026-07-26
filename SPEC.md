# go-tsk — spec

A task runner that reads `Taskfile.yml`, supports the features one real project
uses, and fixes the semantics that make Task unusable as a control plane.

Binary: `tsk`. Platforms: macOS and Linux. No Windows, which is why this can stay
small — the only reason Task embeds a shell interpreter is Windows support.

## Why

Measured against rest-mail's `Taskfile.yml` + `tasks/*.yml` (3,131 lines, 153
tasks), three Task behaviours are not quirks but defects:

1. **A task cannot take an argument.** Bare words after a task name are more task
   names, so `tsk up mail4.test` is impossible; the value must arrive as an
   environment variable set *before* the command.
2. **`dotenv:` resolves at parse time, before CLI variables are merged.** So
   `task up CONFIG=mail4.test` — valid Task syntax, shown in its own docs —
   loads the *default* config's env and silently acts on the wrong stack. A
   guard task had to be written to reject the form.
3. **A missing `dotenv:` file is skipped silently.** Every variable falls back to
   a default, container names resolve to `-suffix`, and commands quietly match
   nothing.

Plus one structural problem: **included files' variables are flattened into the
parent's namespace**, so two includes can overwrite each other. rest-mail cannot
include `reference-mailserver` at all because of it, and has to shell out with
`task -d` instead.

## Feature set

Exactly what rest-mail uses, plus what is nearly free. Measured, not guessed:

| feature | uses in rest-mail | notes |
|---|---|---|
| `desc` | 128 | `--list` |
| `cmds` (string, `- task:` ref, `- \|` block) | 124 | |
| `vars` | 32 | |
| `deps` | 24 | run concurrently |
| `silent` | 10 | file- and task-level |
| `sources` | 9 | checksum up-to-date check |
| `generates` | 8 | |
| `sh:` dynamic vars | 4 | |
| `env:` | 4 | |
| `status` | 3 | shell exit code check |
| `dir:` | 2 | |
| `dotenv` | 1 | |
| `includes` (`taskfile`, `dir`, `vars`, `optional`, `flatten`) | 1 | |
| `internal` | 1 | hidden from `--list` |
| `run: once` | 1 | |
| `defer` | 1 | runs on task exit, in reverse order |

Templating: Go `text/template`, with `if`/`else`/`range` (native) and exactly one
function — `default`, used 216 times. No sprig.

Special variables: `.ROOT_DIR` (27 uses), `.CLI_ARGS` (2). Also provided:
`.TASK`, `.TASKFILE_DIR`, `.USER_WORKING_DIR`.

Cheap additions, included because they are a few lines each: `aliases`,
`ignore_error`, `requires`, `platforms`, `summary`.

**Explicitly not supported**: remote/git includes, `watch`, `for:`/matrix
expansion, `prompt`, `interactive`, output styles (group/prefixed),
`set`/`shopt`, v2 schema, shell completions, Windows.

## Fixed semantics

1. **Positional arguments.** `args: [config]` on a task; `tsk up mail4.test`
   binds them in order. Too many arguments is an error. Bare words are never
   additional task names — multi-target invocation does not exist. Running
   several tasks is `tsk a && tsk b`, which is what everyone types anyway.
2. **One resolution order, applied once, at invocation:**
   `positional args` → `call vars` (from a `- task:`/`deps` reference) →
   `task vars` → `include vars` → `file vars` → `dotenv` → process environment.
   Later layers never shadow earlier ones.
3. **`dotenv:` is resolved after arguments are bound**, per invocation, so
   `dotenv: ['config/{{.CONFIG}}/config.env']` works from the command line.
4. **A config with no environment at all is an error.** The property worth
   guaranteeing is that a task never runs with every variable silently defaulted —
   that is how container names resolve to `-suffix` and commands match nothing. So
   if a task declares dotenv files and *none* of them exist, it fails. A partial
   miss is reported on stderr and continues, because an absent `secrets.env` is
   normal. Prefix a path with `?` to silence the report:
   `dotenv: ['config/{{.CONFIG}}/config.env', '?config/{{.CONFIG}}/secrets.env']`.
5. **Includes get only what is mapped to them.** No flattening of variables into
   the parent namespace, so two includes cannot collide.
6. **`deps` run concurrently**; `run: once` deduplicates on the task's *rendered*
   variables, so the same task with different arguments runs twice.
7. **Failures propagate.** A non-zero command fails the task and the run unless
   `ignore_error`, and the exit code is the command's.

## Package layout and contracts

Each package owns its files and depends only on those listed.

```
cmd/tsk/main.go            → internal/cli
internal/taskfile/         (no deps)          schema + strict YAML decoding
internal/shell/            (mvdan.cc/sh)      run and capture shell
internal/tmpl/             (taskfile, shell)  scope, precedence, rendering
internal/loader/           (taskfile, tmpl)   read files, resolve includes
internal/fingerprint/      (taskfile, tmpl, shell)  status/sources/generates
internal/run/              (all of the above) graph, scheduling, execution
internal/cli/              (all of the above) flags, arg binding, --list
```

### internal/taskfile

Types are already written (`schema.go`) and are the contract — do not change
them. Needs: `UnmarshalYAML` for `Var` (scalar or `{sh: …}`), `Cmd` (scalar
string, or a mapping with `cmd:`/`task:`), `Dep` (scalar string or mapping), and
a strict `Decode([]byte) (*File, error)` that errors on unknown fields so a typo
is loud.

### internal/shell

```go
type Shell struct{ Dir string; Env []string; Out, Err io.Writer; Bin string }
func (s Shell) Run(ctx context.Context, script string) error          // stream
func (s Shell) Capture(ctx context.Context, script string) (string, error) // stdout
```

Runs the **system shell** (`bash -c`), not an embedded interpreter. Task embeds
`mvdan.cc/sh` because it supports Windows; measured against that library, two of
its gaps disqualify it here:

- **`set -o pipefail` is not implemented** — a failing pipeline returns success,
  silently. That is the exact class of bug this program exists to remove.
- **builtins differ subtly** — its `printf` pads by runes where a real shell pads
  by bytes, so a Taskfile can be accidentally coupled to Task's own shell (see
  Deviations).

Dropping Windows makes the real shell available, with the semantics a developer
gets at their own prompt, and removes four dependencies.

`bash` is resolved from **PATH first**: macOS still ships bash 3.2 (2007) at
/bin/bash, whose parser mishandles a `case` pattern inside `$( … )` and reports a
syntax error at the `;;`. Real Taskfiles contain that construct.

### internal/tmpl

```go
type Scope struct{ /* ordered layers, lowest priority first */ }
func New(env []string) *Scope                       // process env is the base layer
func (s *Scope) Push(vars map[string]string) *Scope // returns a child, never mutates
func (s *Scope) Set(k, v string)
func (s *Scope) Get(k string) (string, bool)
func (s *Scope) All() map[string]string
func (s *Scope) Render(text string) (string, error) // text/template + `default`
func (s *Scope) Resolve(ctx context.Context, vars map[string]taskfile.Var, sh Shell) (map[string]string, error)
```

`Resolve` renders each var's `Value` against the current scope, or runs `Sh` and
captures stdout. Rendering a reference to an undefined variable yields empty
string (as Task does), but `requires:` can make it an error.

### internal/loader

```go
func Load(path string) (*taskfile.Project, error)
```

Reads the root Taskfile, resolves `includes` recursively (relative to the
including file's directory), namespaces tasks as `ns:task`, honours `optional`
(missing file → skip) and `flatten`, detects cycles, sets `Name`, `File`, `Dir`,
`Path`, `RootDir`. Include `vars` are attached to the included file's tasks as
their include layer — not merged into the parent.

### internal/fingerprint

```go
func UpToDate(ctx context.Context, t *taskfile.Task, sc *tmpl.Scope, sh shell.Shell, cacheDir string) (bool, error)
```

`status:` — every command exits zero → up to date. `sources:`/`generates:` —
SHA-256 over matched files (globs relative to the task's directory), compared
with the previous fingerprint stored under `cacheDir` (default `.tsk/`). Any
missing generated file means not up to date.

### internal/run

```go
type Runner struct {
    Project *taskfile.Project
    Out, Err io.Writer
    DryRun, Force, Verbose bool
}
func (r *Runner) Run(ctx context.Context, name string, args []string, callVars map[string]string) error
```

Resolves the task, binds `args`, builds the variable scope in the documented
order, loads `dotenv` (after args), evaluates up-to-date checks unless `Force`,
runs `deps` concurrently (errgroup, first error cancels), then `cmds` in order.
A `- task:` cmd recurses with its own call vars. `run: once` dedupes on
`name + rendered vars`. `DryRun` prints rendered commands without executing.

### internal/cli

```go
func Main(args []string, stdout, stderr io.Writer) int
```

```
tsk [flags] <task> [args...] [-- extra]
  -C, --dir DIR     change to DIR before reading Taskfile.yml
  -f, --file FILE   Taskfile to read (default Taskfile.yml, searched upward)
  -l, --list        list tasks with descriptions, grouped by namespace
      --dry         print commands without running them
      --force       ignore up-to-date checks
  -v, --verbose     echo commands before running
```

Everything after `--` becomes `.CLI_ARGS`. Unknown task → error listing near
matches. No task → `--list`.

## Acceptance

The binary must run rest-mail's Taskfile **unmodified**:

1. `tsk --list` lists all 153 tasks, matching `task --list` modulo ordering.
2. `tsk status`, `tsk ps`, `tsk config:check` behave as their Task equivalents.
3. `tsk config:check CONFIG=mail4.test` — the invocation Task silently got wrong —
   either acts on mail4.test or fails; it never silently uses the default.
4. `tsk build` and `tsk test:unit` run.
5. Deleting `_guard:selector` from rest-mail's Taskfile changes nothing.

Diffing both binaries over the same file is the test harness, and it is the
reason for keeping the format.

## Deviations found while running rest-mail's Taskfile

Both are consequences of using the real shell, and both are improvements that
happen to be visible:

1. **`printf` pads by bytes, not runes.** rest-mail's `status` table aligns
   columns containing ●/○/· with `%-9s`, which lines up under Task's rune-aware
   builtin and drifts by 1–2 columns under any real shell. A Taskfile that
   formats multi-byte output is coupled to Task's shell; the fix is to pad by
   display width in a program rather than in `printf` (rest-mail has already
   moved that view into its own CLI).
2. **Parse errors surface later.** bash reads a `-c` string incrementally, so
   commands before a malformed construct do run; an interpreter that parses the
   whole script first would have run nothing.

## Verified against rest-mail (2026-07-27)

| check | result |
|---|---|
| `tsk --list` vs `task --list` | **153 tasks each, zero difference** |
| `tsk status` (193-line shell body, nested `{{\`{{.Names}}\`}}` escaping) | runs to completion |
| `tsk ps`, `tsk config:check` | correct output, exit 0 |
| `tsk config:check CONFIG=mail4.test` | acts on **mail4.test** — the invocation Task silently got wrong |
| `tsk build` (sources/generates) | builds, then skips as up to date on re-run |
| `_guard:selector` | no longer needed |
