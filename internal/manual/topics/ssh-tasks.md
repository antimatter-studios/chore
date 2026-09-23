<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/global/remote.go:15 -->
---
title: SSH tasks and port forwarding
summary: optional remote execution and local TCP forwarding for global tasks
aliases: remote-tasks port-forwarding
---

# SSH tasks and port forwarding

SSH is an optional execution form for a global task. Ordinary global tasks
remain ordinary chore tasks. A task using `route:` names a route declared in
the same global taskfile and uses exactly one of `exec:` or `forward:`:

```yaml
name: homelab
routes:
  pi:
    - { host: s1.example.com, port: 10022, user: root }
    - { host: 127.0.0.1, port: 2222, user: chris }
tasks:
  k3s:pods:
    route: pi
    exec: [kubectl, get, pods, -A]
  k3s:proxy:
    route: pi
    forward: { remote: 127.0.0.1:6443, local: 127.0.0.1:6443 }
```

Every hop after the first is dialled from the previous hop. SSH authentication
uses `$SSH_AUTH_SOCK`; host keys are checked against `~/.ssh/known_hosts`.
`exec:` is quoted as one POSIX shell command because SSH carries a command
string, not an argv array. `forward:` binds locally and stays in the foreground
until interrupted. `--dry` prints the route and remote action without dialing.

Remote tasks accept `desc:`, `aliases:`, `internal:`, `route:`, and one of
`exec:`/`forward:`. Other task behavior belongs to ordinary tasks, and is
refused for this execution form.
