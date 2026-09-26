<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/global/schema.go:17 -->
---
title: Global tasks
summary: declaring and running tasks available from any directory
---

# Global tasks

A global task is an ordinary chore task declared in a user-wide taskfile. It
is available from any working directory; global declarations do not change
what a task can do or how it runs.

```yaml
name: homelab
version: '3'
tasks:
  status:
    desc: show cluster status
    cmds: [kubectl get nodes]
```

Files live in `${XDG_CONFIG_HOME:-$HOME/.config}/chore/global.d/*.yaml`, one
namespace per file. The `name:` is the namespace segment in the address:

```
chore global:                       list installed namespaces
chore global:homelab:               list a namespace's tasks
chore global:homelab:status         run an ordinary chore task
```

`--dry`, task arguments, dependencies, includes and lifecycle hooks keep their
ordinary chore meanings. A taskfile's namespace is its name, while the task
name and task schema remain unchanged.
