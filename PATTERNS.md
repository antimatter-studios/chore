# Reuse in a chores.yml

A `chores.yml` grows by copy-paste, because a task is a wall of shell and there
is no obvious place to put the half of it that two tasks share. Measured across
five real files — rest-mail, lunaria-web, period-tracker, rogue-trader and
deionizer — **12–28% of the non-comment lines sit inside a run of three or more
lines that appears somewhere else in the same project**, and the ratio barely
moves with the size of the file. rest-mail's 2,284 code lines carry 416 of them.

Everything in this document works today and none of it needed a new feature. It
is written because the pieces were already there and nobody could see them: the
question "how do I stop repeating myself" had no answer in the README, so the
answer people reached for was the clipboard.

## The one decision

**Does the repeated thing need the caller's shell?**

A task runs its `cmds:` in a fresh process. `internal/shell` says so outright —
"each Run or Capture starts a fresh process, so nothing — variables, options,
`cd` — leaks from one script into the next". That single fact decides which tool
you want:

| what repeats | reach for |
|---|---|
| a sequence of commands, self-contained | a task, called with `- task:` |
| a **value** — a flag string, a container status, a path | a task called through `{{.CHORE_EXE}}` |
| shell **state** — a variable, a `trap`, a `cd` | a var holding a shell fragment |

Getting this wrong is the usual failure. Extracting a block that sets
`trap 'rm -f "$SECRETS"' EXIT` into a called task moves the trap into a process
that exits a millisecond later, and the file it was guarding is never removed —
the extraction looks right, the tests pass, and the secret stays on disk.

## A private helper

`internal: true` keeps a task out of `--list`, which is what you want for a
helper that exists to be called rather than typed:

```yaml
  _dart_defines:
    internal: true
    args:
      - {name: seed, type: bool}
      - {name: tab}
    vars: {tab: ""}
    cmds:
      - ...
```

Naming it with a leading underscore is convention, not syntax, and it is worth
following: `--list` hides the task but `chore --help` and an error message will
still print the name, and a reader wants to know at a glance that they were not
meant to type it.

`internal:` is enforced, not merely advisory: `chore _dart_defines` typed at a
prompt is refused, and so is any alias of it. `deps:` and `- task:` are
untouched, and so is a nested `{{.CHORE_EXE}} _dart_defines` from inside a task —
see the next section, which depends on it. The rule is **callable by chore, not
by a person**, and chore tells the difference by the `CHORE=1` it exports to
every task script.

## Calling a helper as a subroutine

`- task:` with `vars:` is a call with arguments. The callee's `args:` block is
honoured on this path exactly as it is from the command line — types are
checked, declared defaults apply, and a required parameter with no value is an
error rather than a blank:

```yaml
  ci:
    cmds:
      - task: test
        vars: {lint: "1"}
```

```
chore: needy: needs argument(s) required_thing — pass positionally
       (chore needy <required_thing>), as REQUIRED_THING=value, or give it
       a default in vars
```

This is the mechanism that stops two entry points drifting. period-tracker's
`chore test` and `chore ci` began as separate renderers of the same summary and
disagreed within the hour; `ci` is now three lines that call `test`.

`deps:` takes `vars:` the same way, and `run: once` keys its deduplication on
the *rendered* variables — so the same helper called with different arguments
runs once per distinct call, not once in total.

## Calling a helper for its value

A called task's output goes to the terminal, not into a variable. To capture
one, invoke chore recursively — that is what `.CHORE_EXE` is for:

```yaml
  _dart_defines:
    internal: true
    silent: true
    args:
      - {name: seed, type: bool}
      - {name: tab}
    vars: {tab: ""}
    cmds:
      - |
        d=""
        [ -n "{{.SEED}}" ] && d="$d --dart-define=seed=true"
        [ -n "{{.TAB}}" ]  && d="$d --dart-define=tab={{.TAB}}"
        printf '%s' "$d"

  app:run:
    args:
      - {name: seed, type: bool}
      - {name: tab}
    vars:
      tab: ""
      DEFINES: {sh: '{{.CHORE_EXE}} _dart_defines {{if .SEED}}--seed{{end}} --tab "{{.TAB}}"'}
    cmds:
      - flutter run {{.DEFINES}}
```

```
$ chore app:run --seed --tab calendar
flutter run --dart-define=seed=true --dart-define=tab=calendar
```

Four things make this work, and each is worth knowing:

- **An `internal:` helper is still callable here.** This is a command line, so it
  looks like the thing `internal:` forbids. It is allowed because `CHORE=1` is in
  the environment, which means chore is the caller. Enforcing `internal:` without
  that exception breaks this pattern *silently* — as an `sh:` var that will not
  resolve.
- **`.CHORE_EXE`, never a bare `chore`.** It is the binary actually running, not
  whatever PATH answers to — which is a different file exactly when it matters,
  under a `env:` block that pins PATH, or when running a build from a checkout.
- **`silent: true` on the helper**, because chore echoes each command before
  running it and that echo goes to *stdout*. Without it the captured value is
  the command text followed by the value. The helper's `deps:` must be silent
  too, for the same reason.
- **`printf '%s'`, not `echo`.** `sh:` trims trailing newlines, so `echo` works
  by luck; `printf` says what you meant.
- **The space goes in the caller, not the value.** A captured value is
  `TrimSpace`d, so a helper that returns `" --flag"` hands back `"--flag"` and
  `flutter run{{.DEFINES}}` renders as `flutter run--flag`. Write the separator
  into the command — `flutter run {{.DEFINES}}` — and let an empty value collapse
  to nothing.

The cost is a subprocess per call, and arguments re-serialized onto a command
line — which is the quoting hazard `args:` exists to remove, so quote what you
interpolate.

## Sharing a fragment of shell

When the repeated thing needs the caller's shell — a variable the next line
reads, a `trap`, a `cd` — a task cannot hold it. A **var can**, because it is
interpolated as text before the script runs:

```yaml
vars:
  SECRETS_PREAMBLE: |
    SECRETS_ENV="{{.ROOT_DIR}}/.cache/{{.CONTAINER}}.secrets.env"
    trap 'rm -f "$SECRETS_ENV"' EXIT
    rm -f "$SECRETS_ENV"
    : > "$SECRETS_ENV"
    chmod 600 "$SECRETS_ENV"

tasks:
  up:
    cmds:
      - |
        {{.SECRETS_PREAMBLE}}
        printf 'DB_PASS=%s\n' "{{.DB_PASS}}" >> "$SECRETS_ENV"
        docker run --env-file "$SECRETS_ENV" ...
```

The trap installs in the shell that later runs `docker run`, so it fires when
that script exits — which is the whole point, and the thing a called task cannot
do. This is the shape that collapses rest-mail's most-repeated block: seven
copies across six files of a five-line preamble that creates a `0600` env-file
and arranges its removal. Seven copies of a security-critical sequence is seven
chances for one of them to lose the `chmod`.

Two rules for fragments:

- **Give it a name that says it is not a value.** A var that is a program reads
  very differently from a var that is a string, and `{{.SECRETS_PREAMBLE}}` on
  its own line is the only clue a reader gets.
- **Interpolate at column zero of the block.** The fragment carries its own
  newlines; indenting the placeholder indents only its first line.

## Cleanup: `defer:` or `trap`

Both exist and they are not interchangeable.

```yaml
    cmds:
      - defer: 'rm -f {{.TMPFILE}}'
      - |
        touch {{.TMPFILE}}
        ...
```

`defer:` steps run when the task finishes, in reverse order, whether or not it
succeeded. They run in a **fresh shell**, so they see chore's variables and none
of the script's:

```
cmd ran, SCRIPTVAR=hello
defer A - script var is []
```

Deferred steps also run when the run is interrupted — Ctrl-C on a task that
brought a topology up still tears it down, on a bounded budget.

So: `defer:` for anything nameable from a chore var — a file at a known path, a
container by name, a network. A shell `trap` for anything the script itself
computed. Reach for `defer:` first; it is visible in the file as a step rather
than buried in the middle of a heredoc.

(YAML footnote: `- defer: echo "gone: yes"` fails to parse — the `: ` inside the
scalar. Quote the whole value.)

## What none of this fixes

Two kinds of repetition survive every technique above, and it is worth saying so
rather than letting someone hunt for the trick:

- **Duplicated `args:` declarations.** A public task that accepts `--seed` must
  declare `--seed` itself, even when it does nothing with it but hand it to a
  helper. In period-tracker's two launch tasks that is fourteen duplicated lines
  against four of shared logic — and the copies have already drifted apart in
  their `desc:` text, which is exactly how this ends.
- **Include-mapping boilerplate.** Nine services each mapping
  `X_NETWORK: '{{.PARENT_NETWORK}}'` is nine copies of one idea. `inherit: true`
  on the include is the lever — it brings the parent's variables in as a layer
  below the file's own — but it is off by default and easy to miss.
