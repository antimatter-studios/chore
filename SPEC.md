# chore — spec

A task runner that reads `chores.yml` — go-task's format under a name of its own
— supports the features one real project uses, and fixes the semantics that make
Task unusable as a control plane.

Binary: `chore`. Platforms: macOS and Linux. No Windows, which is why this can stay
small — the only reason Task embeds a shell interpreter is Windows support.

## Why

Measured against rest-mail's `Taskfile.yml` + `tasks/*.yml` (3,131 lines, 153
tasks, as it stood on 2026-07-27), three Task behaviours are not quirks but
defects:

1. **A task cannot take an argument.** Bare words after a task name are more task
   names, so `chore up mail4.test` is impossible; the value must arrive as an
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

Exactly what rest-mail uses, plus what is nearly free. Measured, not guessed —
**and a measurement of somebody else's repository is a dated reading, not a
property of this one.** The first column is the 2026-07-27 survey the feature set
was chosen from; the second is the same corpus re-counted on 2026-08-27, at
rest-mail `373cf2a`, which has since migrated to `chores.yml` and grown to
**3,351 lines and 159 tasks**. Both columns are kept because the design decision
was made against the first and only the second is current.

| feature | 2026-07-27 | 2026-08-27 | notes |
|---|---|---|---|
| `desc` | 128 | 136 | `--list` |
| `cmds` (string, `- task:` ref, `- \|` block) | 124 | 131 | |
| `vars` | 32 | 39 | |
| `deps` | 24 | 18 | run concurrently |
| `silent` | 10 | 10 | file- and task-level |
| `sources` | 9 | 9 | checksum up-to-date check |
| `generates` | 8 | 8 | |
| `sh:` dynamic vars | 4 | 6 | |
| `env:` | 4 | 3 | |
| `status` | 3 | 3 | shell exit code check |
| `dir:` | 2 | 2 | |
| `dotenv` | 1 | 8 | |
| `includes` (`taskfile`, `dir`, `vars`, `optional`, `flatten`) | 1 | 1 | |
| `internal` | 1 | 2 | hidden from `--list`; refused from the command line, but callable by chore |
| `run: once` | 1 | **0** | no longer used there; kept because the semantics are load-bearing |
| `defer` | 1 | 1 | positional; runs on task exit, in reverse order — see [Hooks](#hooks) |

The re-count is `grep -cE '^[[:space:]]*<key>:'` over `chores.yml tasks/*.yml`,
which is **not provably the method that produced the first column** — nine of the
sixteen rows reproduce it exactly, which is corroboration rather than proof. Read
a row that moved as "the corpus moved, or the method differs", and re-run the
grep rather than believing either column.

Templating: Go `text/template`, with `if`/`else`/`range` (native) and exactly one
function — `default`, used 216 times in the first survey and 220 in the second.
No sprig.

Special variables: `.ROOT_DIR` (27 uses), `.CLI_ARGS` (2) — both unchanged
between the two surveys. Also provided: `.TASK`, `.TASKFILE_DIR`,
`.USER_WORKING_DIR`, `.CHORE_EXE`, `.CHORE_VERSION`.

`.CHORE_EXE` is the path of the binary actually running — not what PATH answers
to `chore`. A task that must invoke chore (a launchd plist needs an absolute
`ProgramArguments` path) otherwise bakes in whichever copy PATH finds, which is
a different file exactly when it matters: a file whose `env:` pins PATH, or a
binary run from a checkout. (`CHORE=1` remains an environment variable, so a
Taskfile can tell this runner from go-task; that is a different thing.)

A file may also state the oldest chore that may run it:

    chore_min_version: 0.4.0

Optional — absent means no restriction, which is what nearly every file wants.
It exists because a file's safety can depend on the RUNNER, not only on what the
file says: a Taskfile driving money declared its dangerous flags as strings
compared to `"true"` for one reason, that chore < 0.4.0 bound an unknown
`--flag` positionally and let a bool take any value. Stating the floor is what
lets such a file drop the workaround rather than carry it forever. The strictest
floor among the loaded files wins, includes included; a dev build is exempt,
having no version to judge and a banner that already says so; and a chore too
old to know the field refuses the file anyway, because an unknown top-level key
is an error — so the floor fails closed even against versions that predate it.

Cheap additions, included because they are a few lines each: `aliases`,
`ignore_error`, `requires`, `platforms`, `summary`.

**chore-only extension — hooks**: `before`, `on_success`, `on_failure` and
`after` on any task, the same four names plus `_all` in a top-level `lifecycle:`
block for the run as a whole, `child_hooks:` to switch off a subtree's, and
`defer:` as a positional step. Task has no equivalent for any of it. The family
is specified together under [Hooks](#hooks), because looking for a hook and
finding only `defer:` — filed under `cmds` as a step form — is how the rest go
unfound.

**Explicitly not supported**: remote/git includes, `watch`, `for:`/matrix
expansion, `prompt`, `interactive`, output styles (group/prefixed),
`set`/`shopt`, v2 schema, shell completions, Windows.

## Hooks

Nine hooks in three families. **The distinction that decides which one you want
is not what they do but WHEN they are scoped:**

| hook | written as | scope | fires |
|---|---|---|---|
| `before` | a task field | once per **task run** | before `cmds:`, as a gate |
| `on_success` | a task field | once per **task run** | when the task succeeded |
| `on_failure` | a task field | once per **task run** | when the task or its `before` failed |
| `after` | a task field | once per **task run** | either way, and *in addition* to the outcome hook |
| `lifecycle.before_all` | a top-level block | once per **run** | before the invoked task, as a gate |
| `lifecycle.on_success_all` | a top-level block | once per **run** | when the run succeeded |
| `lifecycle.on_failure_all` | a top-level block | once per **run** | on any non-zero |
| `lifecycle.after_all` | a top-level block | once per **run** | on the way out, success or failure |
| `defer:` | a **step**, inside `cmds:` | once per **task run** | when that task finishes, in reverse order |

Every `lifecycle:` name is its per-task name plus `_all`, and `_all` is the whole
mnemonic: it marks the hook that fires once for the `chore` invocation rather
than once for each task in it.

`defer:` is the odd one, and it is the reason this section exists: it is the only
hook that is **positional**, so it is written where a step goes rather than where
a setting goes, and anyone searching a `chores.yml` reference for "hooks" finds
the fields and not it.

### Order, for one task run

```
before
  -> deps (concurrent)
  -> cmds
  -> deferred steps, reverse order, only those reached
  -> on_success | on_failure
  -> after
```

The body **unwinds before the outcome branch**, so `after` is a finishing step
that runs once the thing it is finishing is already down — a task that brought a
topology up has taken it down again by the time `after` sweeps.

**`before` precedes `deps:`.** A gate is cheaper the earlier it fails, and there
is no dependency work to undo when the check it guards has already said no. The
decisive reason is the up-to-date rule: hooks fire for a skipped task and `deps:`
do not, so "deps then before" could only hold for a task that was not skipped —
the gate's position would depend on the up-to-date result.

At the run level, matching:

```
before_all -> <task> -> on_success_all | on_failure_all -> after_all
```

### The four task fields

```yaml
build:
  before:     [ ./check-toolchain.sh ]
  cmds:       [ make ]
  on_success: [ ./publish.sh ]
  on_failure: [ ./collect-logs.sh ]
  after:      [ 'echo "build ended {{.EXIT_CODE}}"' ]
```

- **`before` gates.** If it fails, `cmds:` do not run and the task fails with the
  gate's own status. `on_failure` fires for that failure, exactly as
  `on_failure_all` fires for a failed `before_all`.
- **`after` runs *as well as* the outcome hook, never instead of it.** On failure
  the order is `on_failure`, then `after`.
- **`after` reads `{{.EXIT_CODE}}`** — `"0"`, or the task's own status. Available
  as `$EXIT_CODE` in the script too, and only in `after` and `after_all`:
  `on_success`/`on_failure` already know the outcome by having been chosen, and
  `before` runs when there is no outcome yet.
- **`on_success`, `on_failure` and `after` cannot change the exit status.** They
  are best-effort, like `after_all`: a failure in one is reported on stderr and
  the task's own status stands. `on_failure` is where someone will reach to
  swallow a failure, and it cannot.
- **They run in the TASK's scope** — its variables, its parameters, its `dir:`.
  `after: echo done {{.TARGET}}` reads the argument the task was called with.
  This is the difference from a `lifecycle:` hook, which runs in the root file's.
- **They fire wherever the task runs**, including as another task's dependency or
  `- task:` step. There is no "only when invoked directly" mode; a hook that must
  fire once per invocation belongs in `lifecycle:`. `run: once` bounds the
  repetition for a task depended on many times — its hooks fire once with it.
- **They run even when the task is up to date**, for the same reason the
  `lifecycle:` block does: a hook is not the task's prerequisite. This is the
  whole reason `before` is not a slower spelling of `deps:`.
- **A `- defer:` inside a hook is refused.** A hook runs to completion at one
  point in the task's life, so there is nothing for it to defer to.

### `child_hooks:` — one task speaking for its whole subtree

```yaml
build:all:
  child_hooks: false                # everything BELOW me runs no hooks
  deps:  [ prep ]
  cmds:
    - { task: driver, vars: {NAME: ext4}  }
    - { task: driver, vars: {NAME: ntfs}  }
  after: ./scripts/sweep-target.sh   # mine still runs — once, not three times
```

- **It does not touch the declaring task's own hooks.** A task that did not want
  those would delete them. What it silences is the tree below, which cannot be
  deleted: the same library task is right to run its hooks when it is the top of
  a run and wrong when it is nested inside a bigger one, and only the caller
  knows which.
- **It reaches every depth**, through `deps:` and `- task:` steps alike. A dep is
  a task invocation, so there is no second rule for it — and a one-level
  suppression would be useless, because the hook that matters usually lives a
  level below where the coordinator can see it.
- **A child cannot opt back in.** `child_hooks: true` inside a suppressed subtree
  does nothing. The guarantee is written at the coordinator and has to be
  readable there.
- **It never suppresses `defer:`**, at any depth. That is what makes deep
  suppression safe: all it can silence is advice, never a teardown that pairs
  with something already brought up.
- **It says nothing about the `lifecycle:` block**, which is per invocation and
  not part of anybody's subtree.
- Known cost: it is the whole subtree or nothing. A coordinator that wants to
  suppress three children and not a fourth splits into two tasks.

### Why `defer:` cannot be a task field

Because a hook that registers itself *at a point in the script* can say things an
unconditional field cannot:

```yaml
cmds:
  - docker compose up -d
  - defer: docker compose down     # only registers if `up` was reached
  - ./run-tests.sh
```

If `docker compose up` fails, the `defer:` is never reached, so `down` never
registers and never runs — which is correct, because nothing came up. An
`on_failure:` field on the task would have to run `down` against a stack that
does not exist. Move the `defer:` above the `up` and `down` runs unconditionally;
that choice is what the position is for.

Measured, on 0.6.0:

```
defer registered BEFORE a failing step   -> it RUNS
defer registered AFTER  a failing step   -> it does NOT run, and never registered
```

### `lifecycle:` — once around the whole run

```yaml
lifecycle:
  before_all:
    - task: hooks:ensure
  on_success_all:
    - ./notify-green.sh
  on_failure_all:
    - ./notify-failure.sh
  after_all:
    - echo "done with {{.TASK}}, status {{.EXIT_CODE}}"
```

- **`before_all` is a gate.** If it fails, the invoked task never starts,
  `after_all` never runs, and the run exits with `before_all`'s status.
  `on_failure_all` still fires.
- **`after_all` runs once the task has been entered**, whether the task
  succeeded or not, so it can tear down what `before_all` set up. It runs *in
  addition to* the outcome hook, and reads `{{.EXIT_CODE}}`.
- **`on_failure_all` fires for a non-zero from either `before_all` or the task**;
  `on_success_all` fires when neither failed. Exactly one of the two runs.
- **They run even when the task they wrap is up to date**, which a `deps:` entry
  could not: a dependency is skipped along with the task it gates, and a
  lifecycle hook is not that task's prerequisite. This is the whole reason the
  block exists — a self-installing repo guard has to run on
  `chore build` when `build` has nothing to do.
- **`{{.TASK}}` is the invoked task's name.**
- **Only the ROOT file's block runs.** A `lifecycle:` in an included file is
  ignored, silently — no warning, no error. Running `chore sub:hello` runs the
  root's hooks and not the include's.
- **Skipped entirely for `--list`, `--help`, `chore <task> --help` and
  `--version`**, and for a whole run with `--no-lifecycle`.
- **A MISTYPED task name still runs them.** Against a file whose task is `ok`,
  `chore okk` runs `before_all`, fails with
  `no task "okk" (did you mean: ok?)`, then runs `on_failure_all` and `after_all`.
  The name is resolved inside the run, not before it, so a `before_all` that
  installs git hooks fires on a typo — harmless for a guard, and not harmless
  for anything expensive.
- **An `internal:` task typed at the prompt is refused BEFORE `before_all`**, so
  that one case runs no hook at all, `on_failure_all` included. A run that will not
  happen does not run a setup gate for it.

### `--no-lifecycle` turns off hooks, not dependencies

It suppresses every hook — the four `lifecycle:` ones and the four per-task
ones — and nothing else. `deps:` still run, and so do `defer:` steps: a
dependency is a requirement and a deferred step is a paired teardown, while a
hook is advice. That distinction is the reason `before` is not a duplicate of
`deps:`.

**As of 0.6.0 the flag works and `chore --help` does not list it**, so the only
way to find it is this document. Measured: `chore --no-lifecycle <task>` skips
the hooks and exits 0, while `chore --help` prints **nine** flags and this is not
one of them. The block above is the CLI's real surface, `--help`'s is nine tenths
of it.

### What a failure in a hook costs

This is the one place the two families genuinely disagree, and it is worth
knowing before you put a `docker rm` in either.

| the failing thing | the run's exit status |
|---|---|
| a `defer:`, in a task that otherwise succeeded | **the run FAILS**, with the defer's own status |
| a `defer:`, in a task that had already failed | unchanged — the task's status wins, the defer is reported on stderr |
| `after_all` | **unchanged** — reported on stderr, exit status untouched |

So `defer:` failure is fatal and `after_all` failure is not. Either way the
message names the task and unwinding **continues** — a defer that fails does not
stop the ones registered before it:

```
task: greenbody: deferred step failed: greenbody: exit status 9
```

### Ordering, and what runs after an interrupt

Registered `D1 D2 D3`, a task unwinds `D3 D2 D1`. Across the whole run the order
on a failure is: the task's own defers (innermost first), then its `on_failure`
and `after`, then `on_failure_all`, then `after_all`.

**All of it survives Ctrl-C.** SIGINT and SIGTERM cancel the run and kill the
script's process group, and teardown — `defer:` and every best-effort hook — then
runs on a **fresh** context with a bounded grace budget, because a cancelled
context cannot start a process. Exit is 128+signal. A second signal is not
caught, so there is always a way out.

### Two things `defer:` does not do

- **It does not run for a task that was up to date.** `sources:`/`generates:` and
  `status:` both short-circuit before the command list is entered, so nothing
  registers — measured separately for each. `lifecycle:` hooks *do* still run in that case — the asymmetry is
  deliberate and is the point of the block.
- **It does not see the script's shell.** A deferred step runs in a fresh
  process, so it reads chore's variables and none of the script's. Anything the
  script itself computed needs a shell `trap`; see
  [PATTERNS.md](PATTERNS.md#cleanup-defer-or-trap).

`lifecycle:` hooks and per-task hooks *do* still run for an up-to-date task —
the asymmetry with `defer:` is deliberate, and is the point of both.

## Fixed semantics

1. **Arguments.** `args:` declares a task's parameters — a bare name, or an
   object when it needs a type or a description:

       args:
         - config                    # shorthand for {name: config}
         - name: follow
           type: bool                # a flag: --follow takes no value
           desc: keep streaming
         - name: lines
           type: int                 # rejects a value it cannot mean

   Types are declared, not inferred. Inferring "boolean" from a true/false
   default was tried first and is subtly wrong: a string parameter whose default
   happens to be "false" silently becomes a flag, and then stops consuming its
   value. A flag is never required — absence is its value — and reads as EMPTY
   rather than "false", because a Go template treats any non-empty string as true
   and the shell idiom is `[ -n "$FLAG" ]`.

   One declaration, five call forms — the first takes the declared default, the
   other four supply a value and are equivalent to each other. All are bound
   before dotenv resolves:

       chore up                       # default from vars
       chore up mail4.test            # positional, in declared order
       chore up --config mail4.test   # named, for a declared parameter
       chore up -c mail4.test         # short, if the parameter declares one
       chore up CONFIG=mail4.test     # Task's spelling, still accepted

   A parameter opts into a single-letter alias with `short:`:

       args:
         - {name: config, short: c}
         - {name: follow, short: f, type: bool}

   Opt-in rather than derived from the name, because a single-dash word is
   otherwise DATA — that is what makes `chore logs -f api` work — so deriving
   `-f` from `follow` would silently change what every existing file does, and
   two parameters starting with the same letter would have no answer. A short
   that cannot work is refused at decode time: more than one letter, a digit
   (`-5` is a negative number reaching an int), two parameters claiming the same
   letter, or `h`, which chore answers as help before a task is invoked.

   Bools bundle, `-fab`, and only bools: if a letter in a bundle takes a value
   the bundle is refused rather than split one of the two ways it could be read.
   A task that declares any short has opted into short parsing, so a single-dash
   letter it does not know is an error naming the ones it has; a task that
   declares none keeps the data behaviour unchanged.

   Whether a parameter is required follows from the declaration, with no marker
   needed in the common cases:

   | declaration | meaning |
   |---|---|
   | `args: [config]` | required — nothing defines it |
   | `args: [config]` + `vars: {config: x}` | optional, defaults to `x` |
   | `args: [filter]` + `vars: {filter: ""}` | optional, and empty is meaningful |

   There is deliberately no required/optional marker: the presence of a default
   already says which a parameter is, and a marker was tried and dropped rather
   than carried on speculation. (A `!` would have to be a suffix in any case — a
   leading `!` is YAML tag syntax, so `args: [!config]` fails to parse and
   `- !config` decodes to a parameter named "". A parameter name that cannot be
   referenced as {{.Name}} is now rejected at decode time, which closes that trap
   whatever syntax anyone reaches for later.) A
   parameter's default reaches the dotenv path, since that path is usually keyed
   on the parameter itself; task vars as a whole cannot, because they are allowed
   to read dotenv values.

   Supplying the same parameter twice (positionally and by name) is refused
   rather than ranked — the caller named two values and would silently get one.

   A value binds under the declared name AND its uppercase form, because Taskfile
   convention is uppercase and a case mismatch would silently interpolate nothing.
   That applies to a value handed over by a `- task:` step or a `deps:` entry
   exactly as it does to one typed at a prompt, and a spelling that differs only
   in case names the same parameter — the two call paths fold identically, or a
   caller's value reaches one spelling while the other keeps whatever a file var
   or the environment said. One parameter given two different values under two
   spellings is refused rather than ranked.
   A flag is consumed only if it names a declared parameter, so `chore logs -f api`
   still passes `-f` to the task. A declared name cannot contain a hyphen (it has
   to be a usable variable name), so a two-word parameter is `train_bars` in the
   file and `--train-bars`, `--train_bars` or any casing of either on the command
   line — the hyphen folds onto the underscore, because nobody types the
   underscore. A leftover `--word` that named no parameter is refused rather than
   bound: it is a typo, and binding it would make it the value of an unrelated
   parameter — for a `type: bool` that value reads as true, so a mistyped flag
   would switch on a flag nobody named. `chore t -- --word` still hands it over as
   data. Single-dash words are untouched, which is what keeps `-f` above working.
   Too many arguments is an error; a parameter with
   neither argument nor default is an error, not a blank. Bare words are never
   additional task names — multi-target invocation does not exist. Running several
   tasks is `chore a && chore b`, which is what everyone types anyway.
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
8. **An interrupt stops the whole task.** SIGINT and SIGTERM cancel the run, which
   kills the script's process group — not just the shell — so Ctrl-C cannot leave
   a `flutter run` or a `docker logs -f` behind. Exit is 128+signal. Teardown
   (`defer:` and every best-effort hook) still runs, on a fresh bounded context,
   because a cancelled one cannot start a process. A second signal is not caught.

## Package layout and contracts

Each package owns its files and depends only on those listed.

```
main.go                    → internal/cli
internal/chorefile/        (no deps)          schema + strict YAML decoding
internal/shell/            (os/exec)          run and capture shell
internal/tmpl/             (chorefile, shell) scope, precedence, rendering
internal/loader/           (chorefile, tmpl)  read files, resolve includes
internal/fingerprint/      (chorefile, tmpl, shell)  status/sources/generates
internal/run/              (all of the above) graph, scheduling, execution
internal/cli/              (all of the above) flags, arg binding, --list
```

### internal/chorefile

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

`$SHELL` is ignored. On macOS it is zsh, which does not word-split unquoted
expansions — `x="a b c"; for i in $x` iterates once there and three times in bash
— so running a Taskfile's scripts in the user's interactive shell would silently
change their meaning. rest-mail's own `status` task loops over an unquoted list of
project prefixes and would stop detecting orphans.

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
func (s *Scope) Resolve(ctx context.Context, vars map[string]chorefile.Var, cap Capturer) (map[string]string, error)
```

`Resolve` renders each var's `Value` against the current scope, or runs `Sh` and
captures stdout. Rendering a reference to an undefined variable yields empty
string (as Task does), but `requires:` can make it an error.

### internal/loader

```go
func Load(path string) (*chorefile.Project, error)
```

Reads the root Taskfile, resolves `includes` recursively (relative to the
including file's directory), namespaces tasks as `ns:task`, honours `optional`
(missing file → skip) and `flatten`, detects cycles, sets `Name`, `File`, `Dir`,
`Path`, `RootDir`. Include `vars` are attached to the included file's tasks as
their include layer — not merged into the parent.

### internal/fingerprint

```go
func UpToDate(ctx context.Context, t *chorefile.Task, r Renderer, sh Runner, dir, cacheDir string) (bool, error)
```

`status:` — every command exits zero → up to date. `sources:`/`generates:` —
SHA-256 over matched files (globs relative to the task's directory), compared
with the previous fingerprint stored under `cacheDir` (default `.chore/`). Any
missing generated file means not up to date.

### internal/run

```go
type Runner struct {
    Project *chorefile.Project
    Out, Err io.Writer
    DryRun, Force, Verbose, NoLifecycle bool
    CLIArgs string                      // everything after `--`
    ChoreExe, ChoreVersion string        // {{.CHORE_EXE}}, {{.CHORE_VERSION}}
}
func (r *Runner) Invoke(ctx context.Context, name string, args []string, callVars map[string]string) error
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
chore [flags] <task> [args...] [-- extra]
  -C, --dir DIR       change to DIR before looking for the file
  -f, --file FILE     file to read (default chores.yml, searched upward)
  -l, --list          list tasks with descriptions, grouped by namespace
      --dry           print commands without running them
      --force         ignore up-to-date checks
  -v, --verbose       echo commands even for silent tasks
      --no-color      plain output (also: NO_COLOR, or a non-terminal)
      --no-lifecycle  skip the file's lifecycle: hooks for this run
  -h, --help          usage, or a task's own help when a task is named
      --version       print the version
```

Everything after `--` becomes `.CLI_ARGS`. Unknown task → error listing near
matches. No task → `--list`.

## Acceptance

The binary must run rest-mail's Taskfile **unmodified**. These are the criteria
as written against that repository's 2026-07-27 state, and are kept in that tense:
rest-mail has since adopted chore, so criterion 1 no longer has a matching pair
to be true of — `chore --list` is now a **superset** of `task --list` there, by
the two peer repositories go-task cannot reach. See README's Status section for
the re-measurement.

1. `chore --list` lists all 153 tasks, matching `task --list` modulo ordering.
2. `chore status`, `chore ps`, `chore config:check` behave as their Task equivalents.
3. `chore config:check CONFIG=mail4.test` — the invocation Task silently got wrong —
   either acts on mail4.test or fails; it never silently uses the default.
4. `chore build` and `chore test:unit` run.
5. Deleting `_guard:selector` from rest-mail's Taskfile changes nothing.

Diffing both binaries over the same file is the test harness, and it is the
reason for keeping the format.

## Filename

`chores.yml`, not `Taskfile.yml`. The format is go-task's, but the two runners
are not interchangeable: chore reads `args:` and go-task ignores it, and go-task
silently mishandles `task <task> VAR=value`, so a file both might claim invites
exactly the mistake this program exists to prevent. `Taskfile.yml` is still read
last, with a notice, so a repository can migrate without a flag day.

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

## Bugs the test suite found in code that already "worked"

All four were in code passing a 153-task acceptance run, which is why the run
alone was not evidence:

1. **`run: once` was not once.** Dedup was recorded after execution returned, so
   two concurrent `deps` both saw "not started" and both ran it — double-creating
   whatever the task creates. Now a `sync.Once` per key; the second caller blocks
   and takes the first's result.
2. **The dotenv path ignored the caller.** File vars outranked call vars when
   rendering `dotenv:`, so `vars: {CONFIG: a}` beat `chore show CONFIG=b`: the task
   ran with CONFIG=b and config **a**'s environment. rest-mail hid it by using the
   self-defaulting idiom everywhere.
3. **Strictness stopped at the first custom unmarshaler.** `KnownFields(true)` is
   not inherited by `yaml.Node.Decode`, so `{sh: date, shh: 1}` and
   `{cmd: x, slient: true}` decoded cleanly — a typo became silence, in the
   program written to abolish silence.
4. **A bare `-` in a list vanished.** yaml.v3 zero-fills a null element before any
   unmarshaler runs; the step simply disappeared. Named slice types now reject it.

## Verified against rest-mail (2026-07-27)

| check | result |
|---|---|
| `chore --list` vs `task --list` | **153 tasks each, zero difference** |
| `chore status` (193-line shell body, nested `{{\`{{.Names}}\`}}` escaping) | runs to completion |
| `chore ps`, `chore config:check` | correct output, exit 0 |
| `chore config:check CONFIG=mail4.test` | acts on **mail4.test** — the invocation Task silently got wrong |
| `chore build` (sources/generates) | builds, then skips as up to date on re-run |
| `_guard:selector` | no longer needed |
