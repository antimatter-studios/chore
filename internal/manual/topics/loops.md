<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/run/loop.go:15 -->
---
title: Loops
summary: for: — one step per item of a list, a map or a string
aliases: for loop list lists map maps iterate
---

# Loops

`for:` repeats one step per item, so a loop over hosts, drivers or services is
something chore walks rather than a bash `for` it cannot see into. The step is
a shell line or a call to another task, exactly as it is without `for:`.

```yaml
vars:
  DRIVERS: [ext4, ntfs, xfs]
  PORTS:   {map: {api: 8080, worker: 9090}}

tasks:
  build:all:
    cmds:
      - for: {var: DRIVERS}
        task: build
        vars: {DRIVER: '{{.ITEM}}'}

      - for: {var: PORTS}
        cmd: echo "{{.KEY}} listens on {{.ITEM}}"

      - for: [tick, analyst, auditor]
        cmd: launchctl unload "$HOME/Library/LaunchAgents/com.rt.{{.ITEM}}.plist"
```

## What to loop over

```
for: [a, b, c]                   a literal list; each item is a template
for: {var: NAME}                 a variable — a list, a map, or a string
for: {var: NAME, split: ','}     a string split on ',' instead of whitespace
for: {var: NAME, as: HOST}       the item is {{.HOST}} rather than {{.ITEM}}
```

The item is a variable like any other, so `{{.ITEM}}` and `$ITEM` are the same
value; a map loop sets `{{.KEY}}` and `$KEY` too. With `as: HOST` the item is
in HOST only, and ITEM is not set. `as:` cannot be KEY.

A list gives its items in order. A map gives each entry in the order it is
written, the key in `{{.KEY}}` and the value in `{{.ITEM}}`. A string is split
on whitespace unless `split:` says otherwise, so `{sh: ls *.sh}` output and a
value typed on the command line both loop without further work. Items split
out of a string are trimmed, and empty ones are skipped.

A `for:` naming a variable that is not set is an ERROR, not a loop of zero
items: a misspelt name would otherwise succeed by doing nothing. A variable
that is set and empty loops zero times.

## Where it works

On any step: in `cmds:`, in a hook (`before:`, `after:` and the rest), and in
a `defer:`, where it goes INSIDE the deferred command — the whole loop is
deferred, and its items run in order when the task finishes:

```yaml
cmds:
  - defer: {for: [web, db], cmd: 'docker rm -f {{.ITEM}}'}
```

Written beside `defer:` instead (`- for: […]` with `defer:` on the same step)
it is refused, because it could mean either thing.

## List and map variables

```yaml
vars:
  HOSTS: [web1, web2]                 # a list
  PORTS: {map: {web: 80, db: 5432}}   # a map — `map:` because {sh: …} owns the bare mapping
```

Each list item and map value is a template, rendered like any variable; map
keys are taken as written. Items are plain values — a list of lists, or a map
holding one, is refused when the file loads.

Templates and the environment see a STRING form: a list's items joined by
spaces (`web1 web2`), a map's `key=value` pairs joined by spaces. So
`for h in {{.HOSTS}}` in an existing script keeps working when the variable
becomes a list. A list passed to another task through `vars:` arrives as that
string, and `for:` splits it back into the same items — exactly, as long as no
item contains a space.

A value from the command line replaces a list like any other value:
`chore build:all DRIVERS="ext4 xfs"` loops over two drivers.

A template cannot walk a list itself. `{{range .HOSTS}}` fails with "range
can't iterate over web1 web2", and `{{index .HOSTS 0}}` prints 119, the first
byte of the string, because a template only ever sees the string form. Loop
with `for:`.

## Order and failure

Items run one at a time, in order, and the first failure stops the loop and
fails the step — the same rule as `cmds:` itself — with the error naming the
item:

```
chore: build:all: for ITEM="broken": build: exit status 1
```

With `ignore_error: true` on the step or on the task, a failing item is
reported and the loop carries on to the next.

Each `- task:` iteration is an ordinary call: it sees only the `vars:` passed
to it, `requires:` and `args:` check each call, and `run: once` keys on the
rendered vars, so distinct items each run.

The spelling is go-task's. Not supported: `for: sources`, `for: {matrix: …}`,
and `for:` on a `deps:` entry.
