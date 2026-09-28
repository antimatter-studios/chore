<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/global/schema.go:30 internal/global/expand.go:9 -->
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

  shell:
    desc: an interactive login shell on the homelab
    route: pi
    pty: true
    cmd: [bash, -l]
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
- **A task says where it goes.** There is no default route: a task with a
  `route:` runs at the far end of it, and one without runs here.
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
- **`pty: true` allocates an interactive terminal** for a remote command. Chore
  puts a local terminal into raw mode for the session and restores it on exit;
  terminal resize events are forwarded to the remote PTY.
## A route can be a choice

```yaml
routes:
  lan:  [ { host: 192.168.0.47, user: chris } ]
  pi:   [ { host: s1.example.com, port: 10022, user: root } ]
  home: { if: on-lan, then: lan, else: pi }

tasks:
  on-lan:
    desc: this machine is on the home network
    cmd: ifconfig | grep -q 'inet 192\.168\.0\.'

  pods: { route: home, cmd: [kubectl, get, pods, -A] }
```

The same task often has two correct routes, and which one is correct depends
on which network the laptop is on. `if:` picks between them.

**`if:` names a TASK.** Not a shell line — there is no inline form. A command
**succeeds** when it exits 0, and an `if:` is true when its task succeeds;
that is the same word `ignore_error:`, `on_failure` and `status:` already
mean. Naming the task buys four things: the question has a `desc:`, so a
listing says what is being asked; it can be run on its own, which is how a
route choosing the wrong branch gets debugged; nothing has to guess whether a
string is a name or a script; and shell stays out of the routing table, which
is read on machines its author is not sitting at. The cost is that a one-off
predicate needs a named task, and naming it is usually the part worth keeping.

**Do not mark a predicate `internal: true`.** It would still work, but
`internal:` refuses a task from the command line, and running the predicate is
the only way to find out which branch a route will take — `--dry` will not say,
because it refuses to run anything. A predicate is a question with an answer,
which is a reasonable thing to offer; a helper step like an unlock is not.

**`else:` is required.** The case this exists for is not "do something extra"
— it is that both answers are real. A missing branch would be a task that
silently does nothing somewhere. It is also why no `not:` is needed: negation
is swapping the two names.

**A predicate may not have a `route:`.** Refused at load. See above on what a
hop costs.

**A predicate exiting non-zero has ANSWERED.** It is not a failed run,
nothing is reported, and `else:` is taken. The answer is remembered for the
rest of the run, so two routes sharing a predicate ask it once.

**`--dry` resolves nothing.** A predicate is an ordinary task and may modify
data, so "nothing happens except the parts chore judged safe" is not a
promise `--dry` can make. It prints both branches instead, each reachable
route indented under the branch that leads to it. To learn which branch you
are on, run the predicate.

## Naming another namespace's task

`deps:` and `if:` follow the rule the command line follows: a bare name is
this file's task, and `global:<namespace>:<task>` is another's.

```yaml
tasks:
  pods:
    route: pi
    deps: [global:ssh:unlock]
    cmd: [kubectl, get, pods, -A]
```

That is what makes an unlock step a machine-wide capability rather than a
copy in every file: what `global:ssh:unlock` does is the machine's business —
trove here, 1Password or `keepassxc-cli` elsewhere — and the task that needs
a key just says so.

Checked once every namespace has loaded, since one file cannot see another.
One dependency written both ways is one dependency. A task may not be NAMED
with the `global:` prefix: it could never be reached, and it would make a
`deps:` entry ambiguous.

- **`forward:` holds the terminal.** It binds, prints the address, and stays up
  until Ctrl-C. Daemonising would mean a stop verb, a registry, pid files, and a
  story for a tunnel that died — a lot of machinery to save one terminal tab.

## `$VAR` in a namespace file

```yaml
routes:
  pi: [ { host: $HOMELAB_HOST, user: $USER } ]
tasks:
  unlock:
    cmd: [trove, unlock, $HOME/vault.kdbx, --export]
```

Which machine's `$HOME`? The rule is one sentence:

> Every value in the file is expanded HERE, when the file is read — except the
> string form of `cmd:`, which is handed to a shell that expands it at
> whichever end it runs.

So `cmd: [ls, $HOME]` is this machine's home, and `cmd: 'ls $HOME'` on a task
with a `route:` is the far end's. Both are useful and neither can be reached by
accident.

The escape hatch is the shell form; `$$` is a literal `$` for the rest.

**An unset variable is an error, not an empty string.** A path that quietly
became `/vault.kdbx` because `$HOME` was not set is the silent-wrong-target
failure this whole program exists to remove — and these files are read on
machines their author is not sitting at, which is exactly where a blank would
go unnoticed.
