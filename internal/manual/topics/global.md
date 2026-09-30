<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/global/load.go:27 -->
---
title: Global tasks
summary: machine-wide tasks, reachable from any directory as global:<file>:<task>
aliases: globals global-d
---

# Global tasks

Tasks that belong to a MACHINE rather than to a project, kept in
`~/.config/chore/global.d/*.yaml` and reachable from any directory, with or
without a `chores.yml` in sight.

```
chore global:                       the files installed here
chore global:ssh:                   the tasks in global.d/ssh.yaml
chore global:ssh:unlock             run one
```

**A global file is an ordinary taskfile.** Same schema, same arguments, same
everything: `args:`, `vars:`, `cmds:`, hooks, `--help`, `--dry`, `--` all mean
what they mean in a `chores.yml`. The only difference is where it lives, and
that its tasks are addressed through the fixed `global:<file>:` prefix — the
filename, without `.yaml`, is the namespace:

```yaml
# ~/.config/chore/global.d/agents.yaml
tasks:
  limit:
    desc: the rate limit left, as a table or as JSON
    args:
      - account
      - { name: json, type: bool }
    vars: { account: all }
    cmd: agent-limits {{.ACCOUNT}} {{if .JSON}}--json{{end}}
```

```
chore global:agents:limit                 # account=all
chore global:agents:limit work --json
chore global:agents:limit --help
```

`$XDG_CONFIG_HOME` is honoured when set, and `~/.config` is the fallback —
which matters because the variable is unset on macOS by default, and these
files are meant to arrive on both by the same dotfiles repository.

## Loaded on every run

Every file in global.d is loaded every time chore runs, so anything in one —
a task, a route, a predicate — can be named from anywhere as
`global:<file>:<name>`: from another global file, or from a project.

```yaml
# a project's chores.yml
tasks:
  deploy:
    deps: [global:ssh:unlock]
    route: global:homelab:pi
    cmd: ./deploy.sh
```

A bare name is always in the file it is written in. A global file is loaded
with the same strictness as any other, so one that is present and wrong is an
error naming it rather than a namespace that silently stopped existing.

## What differs, and why

- **`global:` is mandatory.** `chore ssh:unlock` is a task in the current
  project; `chore global:ssh:unlock` is the machine's. The prefix makes the
  call site readable without knowing what is installed, and a project cannot
  shadow a machine's task — a project task may not be named `global:…`.
- **A global task runs in the directory you ran chore from**, not beside its
  file: it belongs to the machine, and `chore global:tools:fmt` should format
  what you are standing in. `{{.TASKFILE_DIR}}` is still its own directory.
- **Its environment is its own.** A global file is the root of its own tree:
  its `dotenv:`, `env:` and `vars:` apply to its tasks, and a project's do not
  leak into it when a project task depends on one.
- **`chore --list` does not include them.** They are listed by `chore global:`.
- **`name:` is not needed.** A file written when it was still accepted loads as
  long as it matches the filename.
