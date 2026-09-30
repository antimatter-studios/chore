<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/chorefile/routes.go:13 -->
---
title: Routes
summary: running a task's steps over ssh, forwarding a port, and choosing a route
aliases: ssh ssh-tasks remote hops forward tunnel with_route exports pty
---

# Routes

Any task, in any file, can run its steps on another machine. A file declares
`routes:` — named paths of ssh hops — and a task names one with `route:`.
Nothing else about the task changes: `args:`, `vars:`, `deps:`, hooks and
`--dry` mean what they mean everywhere else.

```yaml
routes:
  pi:
    - { host: s1.example.com, port: 10022, user: root }
    - { host: 127.0.0.1, port: 2222, user: chris }

tasks:
  pods:
    desc: pods in one namespace, or all of them
    args: [namespace]
    vars: { namespace: "" }
    route: pi
    cmd: kubectl get pods {{if .NAMESPACE}}-n {{.NAMESPACE}}{{else}}-A{{end}}

  proxy:
    desc: the cluster's API on this machine's 6443
    route: pi
    forward: { remote: 127.0.0.1:6443, local: 127.0.0.1:6443 }

  shell:
    desc: an interactive login shell on the homelab
    route: pi
    pty: true
    cmd: [bash, -l]
```

`cmd:` is a one-step `cmds:`. A step is a shell line, or an argv list, which
chore quotes, so an argument containing a space or a quote arrives whole. The
list form cannot pipe, and that is the only reason both exist.

## What runs where

With `route:`, the task's `cmd:`/`cmds:` steps — and its `defer:` steps — run at
the far end, one ssh session each over one connection. Everything else runs
here: `deps:`, hooks, `status:`, `sources:`, and any `- task:` step, which runs
wherever the task it names runs.

Templates are rendered HERE, before the step travels, so `{{.NAMESPACE}}`
is the argument you passed. A shell line's `$VAR` is expanded by the shell at
the far end, so `$HOME` there is the far end's. The task's declared arguments,
and `CLI_ARGS` when you passed `-- words`, are exported at the far end too, so
`$NAMESPACE` works in a routed step the same as in a local one.

In an argv step, `$VAR` is expanded here, from the task's variables and chore's
environment, and an unset one is an error rather than an empty string. `$$` is
a literal `$`. The same rule applies to hop hosts and users and to `forward:`
addresses.

## Each hop is resolved FROM THE PREVIOUS HOP

`127.0.0.1:2222` in the route above is not this machine's loopback — it is
**s1's**, where a reverse tunnel lands on the homelab. One file can therefore
hold several `127.0.0.1`s that mean different machines:

```
routes.pi[1].host   127.0.0.1  ->  s1's loopback
forward.remote      127.0.0.1  ->  the homelab's loopback
forward.local       127.0.0.1  ->  the machine you are sitting at
```

Which is why a failure names the route and the hop by number:

```
chore: route pi, hop 2 (chris@127.0.0.1:2222): connection refused
```

A hop with no `port:` dials 22, and one with no `user:` is your username, as
ssh does.

## Rules

- **A key is never handled by chore.** Authentication is your ssh-agent, over
  `$SSH_AUTH_SOCK`. Host keys are checked against `~/.ssh/known_hosts`, with
  the same refusal to continue when one has changed.
- **`forward:` is the task's body.** It binds, prints the address, and holds
  the terminal until Ctrl-C. A task with `forward:` has no steps.
- **`pty: true` allocates a remote terminal**, for an interactive shell. The
  local terminal goes into raw mode for the session and is restored on exit;
  resizes are forwarded.
- **`with_route:` travels nothing.** It resolves a route and hands it to the
  task's LOCAL steps as `CHORE_ROUTE`, `CHORE_ROUTE_HOST`, `_PORT`, `_USER` and
  `_JUMP` (the earlier hops, in ssh's `-J` form) — for a tool that does its own
  ssh, such as Pulumi.
- **`exports: true`** reads `KEY=value` lines (with or without `export`) from
  the task's stdout and sets them for everything after it in this run — how an
  unlock step hands the next one its `SSH_AUTH_SOCK`. stdout is captured;
  stderr still streams. Refused with `route:`, where the variables would be set
  at the far end.

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
```

**`if:` names a TASK**, and it is true when that task succeeds. The predicate
has a `desc:`, can be run on its own to debug a route taking the wrong branch,
and keeps shell out of the routing table. `else:` is required: the case this
is for is two real answers, not an optional extra.

A predicate runs on this machine — one with a `route:` is refused at load,
since it would pay a connection to answer a question about here. It exiting
non-zero is an ANSWER, not a failure: nothing is reported and `else:` is taken.
Its answer is remembered for the rest of the run. A predicate that must not
run twice in one invocation, like an unlock it depends on, wants `run: once`.

**`--dry` resolves nothing.** A predicate is an ordinary task and may change
something, so `--dry` prints both branches instead.

## Naming things in another file

`route:`, `with_route:`, a selector's `then:`/`else:` and `if:`, `deps:` and
`- task:` all read a name the same way: a bare name is in THIS file, `:name`
is in the root file of this one's tree (an include reaching its project's
route), and `global:<ns>:<name>` is in `global.d/<ns>.yaml`. So a project task
can travel a route declared once for the machine:

```yaml
tasks:
  deploy:
    route: global:homelab:pi
    deps: [global:ssh:unlock]
    cmd: ./deploy.sh {{.ENV}}
```
