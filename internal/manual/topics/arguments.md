<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/cli/cli.go:558 -->
---
title: Task arguments
summary: declaring parameters, and the three ways to supply one
aliases: args parameters params
---

# Task arguments

This is the feature chore exists for. go-task has no equivalent, so a value
could only reach a task as an environment variable set before the command.

A task declares its parameters in positional order:

```yaml
up:
  args:
    - config                     # shorthand for {name: config}
    - name: follow
      short: f
      type: bool
      desc: keep streaming
  cmds:
    - docker compose -p {{.CONFIG}} up {{if .FOLLOW}}--follow{{end}}
```

A parameter is readable as `{{.CONFIG}}` and as `$CONFIG` in the script.

## The three call forms

```
chore up mail4.test              # positional
chore up CONFIG=mail4.test       # named
chore up --config mail4.test     # flag, or -c with `short: c`
```

All three bind BEFORE `dotenv:` is resolved, so a `dotenv:` path may be keyed
on a parameter: `dotenv: ['config/{{.CONFIG}}/config.env']`. go-task resolves
dotenv while parsing, before CLI variables exist, which is why the same line
there silently loads the default config and acts on the wrong stack.

The same `NAME=value` pairs are also global to the run, so they reach the
tasks this one calls. `chore down CONFIG=mail1` has to arrive at the
`- task: postgres:down` inside `down`, or the child renders a container name
with a hole in it and matches nothing.

## Types

```
type: string   (default)
type: bool     a flag: present or absent, no value
type: int
```

The type is declared, never inferred. Inferring "boolean" from a `false`
default was tried and is subtly wrong: a string parameter whose default
happens to be "false" would become a flag and stop consuming its value.

A bool takes `true`/`false`/`1`/`0`/`yes`/`no` and REFUSES anything else,
rather than treating every value as true.

## Short flags

Opt-in per parameter, with `short: f`. Not derived automatically, because a
single-dash word is otherwise the task's DATA — `chore logs -f api` passes
`-f` to the task — and inventing shorts would change what existing files do.
Shorts bundle: `-abc`. `-h` cannot be claimed; it is answered before the task
runs, so the parameter would be unreachable.
