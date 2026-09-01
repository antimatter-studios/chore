<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/run/run.go:598 -->
---
title: Variables
summary: where a value can come from, and which one wins
aliases: vars templates env
---

# Variables

Templating is Go `text/template` with `if`/`else`/`range` and exactly one added
function, `default`. No sprig.

## Precedence, lowest first

```
process environment
dotenv files
file vars:          (with the include's vars: merged in)
task vars:
vars: passed by a caller  (- task: x, or a deps: entry)
NAME=value typed on the command line
positional arguments
```

Nothing later shadows something earlier, and it is all resolved at ONE point.
That single evaluation is the fix for go-task's `dotenv:`, which resolves while
parsing — before command-line variables exist — so `task up CONFIG=x` loads the
default config's environment and acts on the wrong stack.

The one thing above a command-line variable is a value a parent passes
EXPLICITLY to a child, because a file naming a value for one step is describing
that step rather than guessing: a task that brings up two servers by passing
each its own name must not have both collapsed into one by a global.

## Two forms

```yaml
vars:
  NAME: literal
  SHA:  {sh: git rev-parse HEAD}      # captured from a shell command
```

## Provided by chore

```
{{.ROOT_DIR}}          directory of the root taskfile
{{.TASKFILE_DIR}}      directory of the file this task came from
{{.USER_WORKING_DIR}}  where the person actually was
{{.TASK}}              the task's own name
{{.CLI_ARGS}}          everything after --
{{.CHORE_EXE}}         path of the binary running, NOT what PATH answers
{{.CHORE_VERSION}}     its version
{{.EXIT_CODE}}         in `after` and `after_all` only
```

`CHORE_EXE` matters when a task must invoke chore — a launchd plist needs an
absolute path. Resolving the word `chore` through PATH answers "the one I would
get if I typed it", not "the one running me", and those differ exactly when it
matters: a file whose `env:` pins PATH, or a dev binary run from a checkout.

Every variable whose name can be one is also exported to the script as an
environment variable, so `$CONFIG` and `{{.CONFIG}}` are the same value.
