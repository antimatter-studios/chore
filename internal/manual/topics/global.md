<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/global/schema.go:28 -->
---
title: Global tasks
summary: machine-wide tasks that run over ssh, from any directory
aliases: globals remote ssh routes homelab hops
---

# Global tasks

Tasks that belong to a MACHINE rather than to a project, kept in
`~/.config/chore/global.d/*.yaml` — one namespace per file — and reachable
from any directory, with or without a `chores.yml` in sight.

```
chore global:                       the namespaces installed here
chore global:homelab:               the tasks in one
chore global:homelab:k3s:pods       run one
```

`$XDG_CONFIG_HOME` is honoured when set, and `~/.config` is the fallback —
which matters because the variable is unset on macOS by default, and these
files are meant to arrive on both by the same dotfiles repository.

## The file

```yaml
name: homelab

routes:
  pi:
    - { host: s1.example.com, port: 10022, user: root }
    - { host: 127.0.0.1, port: 2222, user: chris }

tasks:
  k3s:pods:
    desc: pods across every namespace
    route: pi
    exec: [kubectl, get, pods, -A]

  k3s:proxy:
    desc: the cluster's API on this machine's 6443
    route: pi
    forward: { remote: 127.0.0.1:6443, local: 127.0.0.1:6443 }
```

## Each hop is resolved FROM THE PREVIOUS HOP

This is the part worth reading twice. `127.0.0.1:2222` in the route above is
not this machine's loopback — it is **s1's**, where a reverse tunnel already
lands on the homelab. One file can therefore hold several `127.0.0.1`s that
mean different machines:

```
routes.pi[1].host   127.0.0.1  ->  s1's loopback
forward.remote      127.0.0.1  ->  the homelab's loopback
forward.local       127.0.0.1  ->  the machine you are sitting at
```

Which is why a failure names the route and the hop by number rather than only
the address it could not reach:

```
chore: route pi, hop 2 (127.0.0.1:2222): connection refused
```

It is also why the forwarding keys are `remote:` and `local:` rather than
anything shorter.

## Rules

- **`global:` is mandatory.** Not to resolve ambiguity — to make the call site
  readable. `chore global:homelab:k3s:pods` says where it came from without the
  reader knowing what is installed on that machine, and a project that defines
  a `homelab:` namespace cannot silently shadow it. A README documenting the
  command stays true on every machine.
- **Every task names its route.** There is no default route, for the same
  reason: the task says where it goes, or it does not say it anywhere.
- **`exec:` and `forward:` are mutually exclusive**, by which key is present
  rather than by a `mode:` field — so a task that is neither, or both, cannot be
  written rather than merely being rejected.
- **`exec:` is an argv list**, so chore does the shell quoting once, correctly,
  instead of every task doing it. Note what that does NOT mean: the SSH protocol
  carries a command as a single STRING which the far end hands to a login shell,
  so quoting exists either way — chore just owns it.
- **A key is never handled by chore.** Authentication is your ssh-agent, over
  `$SSH_AUTH_SOCK`, so a secret manager keeps working without chore knowing it
  exists. Host keys are checked against `~/.ssh/known_hosts`, the same file
  `ssh` uses and with the same refusal to continue when one has changed.
- **`forward:` holds the terminal.** It binds, prints the address, and stays up
  until Ctrl-C. Daemonising would mean a stop verb, a registry, pid files, and a
  story for a tunnel that died — a lot of machinery to save one terminal tab.
