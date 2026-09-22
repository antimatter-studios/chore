# Global shell drop and machine capabilities — design, as agreed

**Status: PROPOSED.** Baseline is the `feat/global-namespaces` branch at `2cb01c0`
plus its uncommitted working tree (`internal/global/expand.go`, `resolve.go`, and
modifications to `load.go`, `run.go`, `schema.go`, `ssh.go`). Nothing below is
built. This file records the decisions taken in conversation on 2026-09-22 so the
work can be built from them rather than re-argued.

The feature that started it was one command:

```
chore global:ssh:homelab
```

"Drop me into a shell on the homelab, without my having to work out how." It is
not expressible today, and working out why turned into a larger question about
what `global:` is for.

---

## 1. What exists today (the baseline this changes)

The branch already carries the hard parts. Verified against the working tree, not
assumed:

- **Routes and hops.** `Namespace.Routes`, each a list of `Hop`, every hop after
  the first dialled *from the previous hop*. Host keys checked per hop against
  `~/.ssh/known_hosts` (`ssh.go:149`). Authentication is the ssh-agent over
  `$SSH_AUTH_SOCK`; chore never handles a key.
- **Conditional routes.** `when:`/`then:`/`else:`, the predicate a local shell
  line, so the LAN-versus-tunnel choice costs no network probe.
- **Three task shapes.** `cmd:` with a `route:` runs at the far end; `cmd:`
  without one runs here; `forward:` holds a tunnel open.
- **`deps:`**, ordered and in-process, with a `done` map so a diamond runs a
  dependency once (`run.go:48`).
- **`exports: true`**, which captures a local command's stdout, parses
  `KEY=value` lines and applies them with `os.Setenv` (`run.go:141`).
- **`with_route:`**, which resolves a route and hands its connection details to a
  local command as `CHORE_ROUTE_*` environment, for a tool that does its own ssh.

Two facts about the ordering matter, because the whole design leans on them:

1. `run.go:48` runs `Deps` **before** `Resolve` and `Dial`.
2. `ssh.go:127` `agentAuth` reads `os.Getenv("SSH_AUTH_SOCK")` **at dial time**,
   and nothing in `internal/cli` ever sets `Dialer.AgentSock`.

So an `exports:` dependency that unlocks a vault and prints a new agent socket
does reach the dialler. That chain works today.

What does not exist:

- **No interactive shell.** `Task.validate` (`schema.go:444`) refuses a task with
  neither `cmd:` nor `forward:`. And `Exec` (`ssh.go:303`) never calls
  `RequestPty`, so even `route:` plus `cmd: bash` yields a pipe with no job
  control, no line editing and no working full-screen program.
- **No way to hand `exports:` to a caller.** `runLocal` captures stdout into a
  buffer and applies it in-process. A task run from a shell script therefore
  prints nothing and changes nothing the caller can see.
- **No cross-namespace `deps:`.** `Task.validate` requires each dependency to be
  a task in the *same* namespace.

---

## 2. Why `global:` and not the alternatives

Two alternatives were weighed seriously, and the honest scoring is recorded here
because the answer is not uniform.

### 2.1 `~/.ssh/config`

For the shell drop alone, ssh_config is the smaller answer: `ProxyJump` gives the
multi-hop chain and `Match host … exec …` gives the LAN-versus-tunnel choice.
Against a *standard* agent it can even run the unlock — `Match exec` runs a
command and uses its exit status, so `keepassxc-cli` feeding `ssh-add` would
work.

It loses on two counts:

- **The file is contended.** It exists already, it belongs to the machine, and a
  dotfiles repository cannot own it. `Include ~/.ssh/config.d/*` is the standard
  mitigation, but the include must be at the top of the file — ssh_config is
  first-match-wins — so every new machine still means editing the contended file
  by hand, carefully. `~/.config/chore/global.d/` is contended with nothing:
  clone and done.
- **It cannot carry a new agent socket.** `trove unlock` spawns its own agent and
  prints a new `SSH_AUTH_SOCK`. `ssh` read that variable at startup, so a
  `Match exec` unlock has no way to inform the process that triggered it, and
  `LocalCommand` runs after the connection is up. This is not a preference; it is
  the case ssh_config structurally cannot serve.

### 2.2 `~/.local/bin` shell scripts, kept in the same dotfiles

This is the closer comparison, and it ties or wins on more axes than is
comfortable:

- Uncontended drop-in directory, exactly like `global.d`.
- A script name on `PATH` *is* an indirection point. `unlock-ssh` can be trove on
  one machine and 1Password on another, and `eval "$(unlock-ssh)"` works the
  same. The agnostic-capability argument does not favour chore.
- No prerequisite. Works on a machine that has never heard of chore.
- Shell completion for free.

Where chore is actually ahead:

- **A listable surface with descriptions.** `chore global:` and
  `chore global:ssh:` enumerate what exists with each task's `desc`, and
  `Set.Lookup` (`load.go:157`) names the missing namespace and lists what is
  installed. `ls ~/.local/bin` gives names. Matching that in bash means a `help`
  script kept in sync by hand. This matters most for the case that motivated it:
  handing an agent a machine's capabilities without describing each one.
- **Routes as shared data.** One `routes:` block, with its conditional, used by
  the shell task, by `k3s:pods`, and by a `with_route:` task feeding Pulumi. In
  bash this is a sourced `lib.sh` — which is the moment "just scripts" stops
  being just scripts.
- **A flat namespace versus a structured one.** `PATH` has one namespace shared
  with every binary on the machine; `global:ssh:unlock` cannot collide with
  anything.

The decision is that these three, together with §2.1's ownership argument, are
worth the dependency. It is a judgement, not a proof, and it is recorded as one.

### 2.3 What the answer is not

`~/.local/bin` scripts that *shim* chore were rejected on a narrower ground:
chore has no `-f`/`--chorefile` flag and discovers `chores.yml` by walking up
from the working directory, so such a shim must `cd` into a config directory and
loses the directory it was invoked from. That objection applies only to shims.
It does not apply to §2.2, and an earlier version of this argument wrongly
treated the two as one.

A shim over the finished feature (`exec chore global:ssh:homelab "$@"`) remains
available at any time, costs two lines, and is generatable from the namespace
file. It is deliberately out of scope here.

---

## 3. The three changes

### 3.1 `shell: true` — an interactive session at the end of a route

A third mode, alongside `cmd:` and `forward:`:

```yaml
tasks:
  homelab:
    desc: a shell on the homelab
    route: homelab
    deps: [global:ssh:unlock]
    shell: true
```

**Explicit, not inferred from the absence of `cmd:`.** The house rule elsewhere
is that the mode falls out of which key is present rather than a `mode:` field,
and inference would follow it. It is rejected here for one reason: project
chorefiles spell the key `cmds:` and global files spell it `cmd:`. Under
inference, that one-letter slip stops being a parse error and silently opens an
interactive shell instead of running the command. The existing error — a task
needs `cmd:`, `forward:` or now `shell:` — is worth more than the symmetry.

Mutual exclusion is extended to three: `cmd:`, `forward:` and `shell:`, at most
one, and `shell: true` requires a `route:` (a shell "here" is the shell you are
already sitting in).

Implementation, a new `Shell()` beside `Exec` in `ssh.go`:

- `session.RequestPty` with `$TERM` (falling back to `xterm-256color` when unset)
  and the size from `term.GetSize` on stdin.
- `term.MakeRaw` on local stdin, restored by `defer`, on every exit path
  including a dialling failure after the terminal was already raw.
- `SIGWINCH` forwarded as `session.WindowChange`.
- `session.Shell()` and `session.Wait()`, exit status through the existing
  `translateExit`.
- A clear refusal when stdin is not a terminal, naming the task: a shell task in
  a pipeline or in CI must fail loudly rather than hang.

`golang.org/x/term v0.45.0` is already a direct dependency. No new module.

Two interactions to get right:

- **Ctrl-C.** `globalMain` installs a `signalContext` that cancels on SIGINT.
  Raw mode disables `ISIG`, so the byte passes through to the remote shell and
  chore's handler does not fire — which is the behaviour wanted. The ordering
  must be that raw mode is entered before the session starts and restored before
  any error is printed.
- **`--dry`.** `Set.DryRun` must print the route hop by hop and then say that a
  shell would be opened, rather than printing an empty command.

### 3.2 `exports:` across a process boundary

The motivating use is a capability an *external* project's script can call
without knowing what implements it:

```bash
eval "$(chore global:ssh:unlock)"
ssh myhost
```

Today this yields nothing: `exports: true` captures stdout into a buffer
(`run.go:119`), applies it with `os.Setenv`, and the process exits. A child
cannot change its parent's environment — the law the code already cites at
`schema.go:268`, solved there for the in-process case only.

**A task with `exports: true` that is the task the user asked for re-emits its
absorbed `KEY=value` lines on stdout.** Run as a dependency, it behaves exactly
as it does now: captured, applied, silent. The distinction is
asked-for-versus-depended-on, which `Runner.Run` versus `Runner.run` already
draws for `internal:`.

This is the `ssh-agent`, `direnv` and `pyenv` idiom, and a task that declares
`exports:` has declared that its output configures the caller.

Two constraints on the implementation:

- The re-emitted lines must reach real stdout undecorated. `globalMain` passes
  `out.Writer()` from `internal/ui`; whatever prefixing or styling that applies
  must not apply here, or `eval` gets garbage.
- The lines are re-emitted **normalised** — the parsed `KEY=value`, requoted —
  not the captured text verbatim. What the caller evaluates should be what chore
  understood, so a line chore ignored cannot be a line the caller executes.

The portable contract for the capability itself is deliberately weaker than what
trove needs, and should be documented as such:

> After `chore global:ssh:unlock` exits 0, `ssh-add -l` lists a usable key.

`keepassxc-cli` feeding `ssh-add`, and 1Password's agent, satisfy that with
nothing to export. trove's moving socket is the outlier, and the only case that
needs the `eval`.

Worth stating once in the manual: the capability name hides how strong the unlock
is. `trove unlock --env` reads its password from a file and prompts for nothing;
a 1Password implementation would ask for biometrics. That is a property of the
machine's file, not of the calling script, which is the entire point — and it is
also the thing a reader of the calling script cannot see.

### 3.3 Cross-namespace `deps:`

If `global:ssh:unlock` is the machine's unlock capability, every namespace that
travels a route wants to depend on it. Today none can: `Task.validate` requires
`n.Tasks[dep]` to exist in the same namespace, so each file would need its own
copy, each hardcoding trove — which defeats the indirection precisely where it
pays.

```yaml
# global.d/homelab.yaml
name: homelab
tasks:
  k3s:pods:
    route: pi
    deps: [global:ssh:unlock]
    cmd: [kubectl, get, pods, -A]
```

**Spelled as the full address.** Not shortened to `ssh:unlock`, and not because
the short form is ambiguous to a parser — `global:ssh:unlock` and `ssh:unlock`
are different strings and cannot collide. It is the rule the manual already
states for the command line, applied in one more place:

> **`global:` is mandatory.** Not to resolve ambiguity — to make the call site
> readable.

Dropping the prefix would lose where the dependency comes from, which is the
thing worth reading in a file that a person opens on a machine they are visiting.
So `deps: [ssh:unlock]` continues to mean a task named literally `ssh:unlock` in
this namespace — legal, since task names already contain colons (`k3s:pods`).

Three consequences:

- **A task name beginning with `global:` is refused at load.** Otherwise a file
  could define a task called `global:ssh:unlock` and undermine the rule.
- **The `done` map is keyed by address**, not bare name. Two namespaces each with
  an `unlock` must not be conflated. Each entry is resolved to its full address
  *before* the map is consulted, because a task may legally name its own
  namespace's task either way — `deps: [unlock, global:ssh:unlock]` is one
  dependency written twice, and keying the two spellings separately would unlock
  twice. The redundancy itself is harmless and allowed, like an absolute path to
  a file in the current directory; only the dedupe has to see through it.
  `global:unlock` is not a third spelling: an address is always
  `global:<namespace>:<task>`, and `Set.Lookup` (`load.go:152`) already refuses a
  namespace given where a task belongs.
- **The root task is marked done before recursing.** Today only dependencies are
  marked, so a cycle `a → b → a` does not hang — it terminates with `a` having
  run twice, which is exactly what `run.go:47` says the map exists to prevent.
  Marking the root turns it into a cycle that can be detected and named.

An error naming an unresolvable cross-namespace dependency should say which
namespace was looked in and what is installed, as `Set.Lookup` already does.

---

## 4. Not in scope

Deliberately excluded, each with its reason:

- **`args:`, `vars:`, `env:`, `dotenv:` in global files.** The question of
  whether a global namespace file should converge on `chorefile` — becoming a
  chorefile with an extra `routes:` block — is real and unresolved. It is
  deferred because nothing in this design needs it: a shell takes no parameters,
  and the unlock task is a fixed argv. It should be reopened by a use case, not
  by symmetry.
- **Per-task `cmds:` lists and lifecycle hooks in global files.** `deps:` plus
  one `cmd:` composes far enough for the cases named. These are where the global
  schema would begin duplicating `chorefile` in earnest.
- **A project task depending on a global one.** `deps: [global:ssh:unlock]`
  inside a project's `chores.yml` is the consistent extension of §3.3's rule —
  prefixed means an address, unprefixed means this file — and it is the form
  that makes a machine capability usable from a project with no wrapper at all.
  It is deferred rather than rejected. It couples `internal/chorefile`'s
  dependency resolution to `internal/global`'s loader and runner, which the
  global package's own documentation argues should stay separate, and it needs a
  decided answer for a namespace that is not installed on this machine — a hard
  failure, or an `optional:` form. Nothing is lost by waiting: with §3.2 in
  place, a project task reaches the same capability as
  `eval "$(chore global:ssh:unlock)"` in its `cmds:`. The native form is the
  ergonomic version, not the enabling one.

- **A `~/.local/bin` shim generator.** Two lines of shell, addable later over
  whatever this becomes, and nothing in the design forecloses it.
- **A machine-readable (`--json`) listing.** Plausibly wanted if agents become a
  first-class consumer of the capability surface. Not needed to build this.

---

## 5. Appendix: decisions taken

1. `shell:` is an explicit key, not inferred from the absence of `cmd:` —
   because `cmds:`/`cmd:` is one letter apart and inference makes the typo
   silent (§3.1).
2. `shell: true` requires `route:` and excludes `cmd:` and `forward:` (§3.1).
3. A shell task with a non-terminal stdin fails loudly rather than hanging
   (§3.1).
4. A top-level `exports:` task re-emits normalised `KEY=value` on stdout; as a
   dependency it stays silent (§3.2).
5. The cross-machine contract for an unlock capability is `ssh-add -l`, not an
   environment variable. The `eval` form exists for implementations like trove
   that move the agent socket (§3.2).
6. Cross-namespace dependencies are written as full addresses, `global:` prefix
   mandatory, for readability rather than for parsing (§3.3).
7. A task name beginning with `global:` is refused at load (§3.3).
8. The `done` map is keyed by address, and the root task is marked before
   recursing (§3.3).
9. `~/.ssh/config` is rejected as the home for this because the file is
   contended and because it cannot receive a new agent socket (§2.1).
10. `~/.local/bin` scripts are rejected on the strength of three advantages —
    a listable described surface, routes shared as data, a structured namespace
    — and the judgement is recorded as a judgement (§2.2).
11. The prefix rule is one rule everywhere a task is named: `global:`-prefixed
    is an address, unprefixed is the current file. A project task naming a
    global one follows from it and is deferred, not rejected (§3.3, §4).
12. A task may name its own namespace's task by full address. Dependency
    entries are resolved to addresses before the `done` map is consulted, so one
    dependency written two ways runs once (§3.3).
