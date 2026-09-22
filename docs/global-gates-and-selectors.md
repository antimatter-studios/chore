# Predicates and the selector — design, as agreed

**Status: PROPOSED.** Baseline is the `feat/global-namespaces` branch at
`d4be060`, where conditional routes already work. Nothing below is built. This
file records the decisions taken in conversation on 2026-09-22.

**Revised the same day, before implementation: there is no `gates:` block.** A
predicate is an ordinary task, and `if:` names one. §3.1 as first written argued
for a block of its own; it is kept below, marked, because one of its three
objections was wrong and the other two turned out to be a validation rule and a
runner rule rather than a reason for a new construct. Superseded reasoning is
left where it was so a reversal is not re-reversed.

It is independent of `docs/global-shell-and-capabilities.md`. That design adds
`shell:`, an `exports:` re-emit and cross-namespace `deps:`; this one reshapes
how a choice is written. They touch different fields and can be built in either
order, though this one renames a key and so is cheaper first.

---

## 1. What exists today

`Route` is a union type. A route is either a list of hops or a choice:

```go
type Route struct {
    Hops []Hop            // the list form
    When, Then, Else string // the conditional form
}
```

`Resolve` (`resolve.go:30`) walks it: while the named route is conditional, run
the predicate, follow `then:` or `else:` to another route name, loop until a
route has hops. A `seen` map catches a route that leads back to itself. The
predicate is `predicateHolds` (`resolve.go:61`) — a shell line run on this
machine, output and stderr dropped, true when it exits 0.

The shape it was built for: the same task has two correct routes and which one
is correct depends on which network the laptop is on.

---

## 2. What is wrong with it

**The conditional is fused into `Route`.** A choice between two values is a
general shape, and nothing about it is specific to hop lists. As written, only
a route can be chosen, and only by a route.

**`when:` is the wrong word.** The `Route` doc comment already calls the
construct "if/else and not if" while the key says `when:`. The name arrived with
the conditional living inside `Route` rather than being argued for. GitLab keeps
`if:` for the condition and `when:` for *scheduling* — `on_success`, `manual`,
`always` — and that is a distinction chore may want later. Ansible's `when:` is
the main precedent the other way, and it is a gate that skips a task, not a
selector that picks a value.

**A predicate cannot be reused or named.** Written inline inside the route, the
same test cannot be shared by a second choice, and the call site shows a shell
line where a reader wants a fact about the machine.

---

## 3. The design

One new construct, not two. A **selector** — `if:`/`then:`/`else:` — names a
**predicate** and yields a value. The predicate is an ordinary task, so the only
thing being added is the selector itself.

```yaml
name: homelab

routes:
  lan:    [ { host: 192.168.0.47, user: chris } ]
  tunnel:
    - { host: s1.example.com, port: 10022, user: root }
    - { host: 127.0.0.1, port: 2222, user: chris }
  homelab: { if: on-lan, then: lan, else: tunnel }

tasks:
  on-lan:
    desc: this machine is on the home network
    cmd: ifconfig | grep -q 'inet 192\.168\.0\.'
```

### 3.1 A predicate is a task

`if:` names an ordinary task in the same namespace. There is no new construct:
a task is already a named command with an exit status, which is the whole of
what a predicate is.

What it gets for free, and what a `gates:` block could not have given it:
`desc:`, so a listing says what the question means, and `deps:`, so a predicate
that must unlock something before it can answer can say so.

**Not `internal: true`, though**, and the reason is worth stating because the
first draft of this design assumed the opposite. `internal:` refuses a task
from the command line, and running the predicate is the only way to learn which
branch a route will take — `--dry` will not say, because it refuses to run
anything (§3.2). Marking a predicate internal closes the debugging route this
design argues for and then points at. A predicate is a question with an answer,
which is a reasonable thing to list; a helper step like an unlock is not.

Two rules make it safe, one checked and one in the runner:

- **A task named by `if:` may not have a `route:`.** Refused at load. The whole
  reason the interface check exists is that it costs no network round trip —
  measured on the route this was built for, a hop is 326–377 ms against 0.65 ms
  on the LAN, about 500x. A routed predicate would silently be the expensive
  thing this shape avoids, and the failure would look like slowness rather than
  a mistake.
- **A non-zero exit from a predicate is an answer, not a failure.** The runner
  does not report it, does not propagate its status, and does not stop the run.
  This is the one piece of real behaviour the change needs.

Output is left exactly as a task's output. The canonical predicate prints
nothing by construction — `ifconfig | grep -q '…'` is silent whichever way it
answers — so suppression would be machinery for a badly written predicate
rather than for the shape.

Side effects are not policed, for the same reason `deps:` does not police them.
`if: deploy` is possible and is the author's business, as it is everywhere else
in chore.

#### Superseded: why this was first written as a `gates:` block

*Kept, marked, so it is not re-proposed.* The original argument was that a
predicate should be structurally incapable of being a task, on three counts.
The first was wrong: "a task streams stdout and stderr" describes a predicate
written badly, not the shape, since the canonical one is silent either way. The
second — that a global task may carry `route:` and pay for it — is real, and is
now a load-time refusal instead of a reason for a new block. The third, side
effects, is real and unpoliceable, and chore does not police it anywhere else
either. What tipped it: a `gates:` entry cannot have `deps:`, and a predicate
that needs a vault unlocked before it can answer is an ordinary thing to want.

### 3.2 `if:`/`then:`/`else:` — the selector

**`if:` takes a task name and nothing else.** No inline shell line, one form
only:

```yaml
homelab: { if: on-lan, then: lan, else: tunnel }
```

Deliberately narrower than `cmd:`, which has two forms because an argv cannot
express a pipe. A predicate has no such need, and the restriction buys four
things:

- **The question gets a name and a `desc:`**, so a listing, `--dry` and an error
  can say what was asked rather than only which branch won.
- **It is independently runnable.** `chore global:homelab:on-lan` answers the
  question by itself, which is how a route that keeps choosing the wrong branch
  gets debugged.
- **No form detection**, so a task name containing a space is never mistaken for
  a shell line, and the parser has nothing to guess.
- **It keeps shell out of the routing table.** An `if:` that accepted a shell
  line would collect them, and a route file is read on machines its author is
  not sitting at.

The cost is real and worth stating: a one-off predicate now costs a named
named task instead of an inline string. Naming it is usually the part worth
keeping.

A task name here follows the same rule as `deps:` — unprefixed means this
namespace, and `global:other:on-lan` names another's.

**`else:` is required.** A selection with no alternative is not a selection, and
the case this exists for is not "do something extra" — it is that the same task
has two correct answers. A missing branch would mean a task that silently does
nothing somewhere.

**The selector is defined independently of routes.** Routes are its first
consumer and, today, its only one: global namespace files have no `vars:` block
for it to pick a value in. That is the point of separating them — when a second
consumer arrives, it consumes the construct that already exists rather than
growing a second conditional of its own.

**Chaining survives.** `then:` may name another conditional route; resolution
loops and `seen` already refuses a cycle.

### 3.3 `when:` is freed

Removed from the schema entirely, not kept as an alias. It is reserved for a
scheduling sense — when a task runs, not whether a branch is taken — if chore
ever wants one.

The rename is free right now and never again. `when:` exists only in code
committed on this branch, its tests, and `internal/manual/topics/global.md`.
`global:` has never been released: the latest tag, `v0.11.0`, is `670d7c1`, two
commits before the feature lands. No file in anyone's dotfiles uses it.

### 3.4 Lazy, and memoised per run

A predicate must not run at load, and must not run when `chore global:` or
`chore global:homelab:` merely lists what is installed. `Resolve` is already lazy and
stays so.

Memoisation is new: a predicate consumed by two selectors in one run must fire
once. It joins the `done` map that already stops a dependency running twice.
A predicate is a shell process, and the reason this whole shape prefers an
interface check over probing the far end is that predicates are paid for.
Running one twice for one answer is the cheap version of the expensive mistake.

Memoisation is per run, never cached across runs. The answer to "am I on the
LAN" is true until it is not.

### 3.5 Exit 0 is success

Stated once, in chore's own terms, and then never mentioned again at a call
site:

> A command **succeeds** when it exits 0. An `if:` is true when the task it
> names succeeds.

That is the same word `ignore_error:`, `on_failure` and `status:` already mean —
`chorefile/schema.go:474` defines `status:` as shell commands where "all exiting
zero means 'already done, skip'". The polarity is already consistent across
chore; what was missing was one sentence defining it rather than leaving each
key to imply it.

No `success` field, no `Result` struct, no accessor. `predicateHolds`
(`resolve.go:61`) already returns `cmd.Run() == nil`, which is exactly this.
The change is a documented definition, not code.

No `{{.SUCCESS}}` in the hook template namespace either.
`docs/per-task-lifecycle-hooks.md` §2.7 settled that: `{{.EXIT_CODE}}` alone,
because a hook needs to tell `exit 1` from `exit 7`, and two spellings of one
fact in one place is the defect it refuses.

### 3.6 No `not:`

Never needed, because `else:` is mandatory. Negation is swapping two branch
names. A `not:` or `unless:` key would be a second way to write something
already writable, which is the alias defect again.

---

## 4. What this does not become

A general `if:` on a task, meaning "skip unless". chore already has three
purpose-built keys for that — `platforms:`, `requires:` and
`status:`/`sources:`/`generates:` (`chorefile/schema.go:340,341,474`) — and a
fourth general one would overlap all three, leaving a reader to work out which
wins.

The line this design holds: a selector **picks a value**; it does not **gate
execution**. Route selection earns it because the alternative is two tasks
differing only in one hop list, with no way for the file to say they are the
same task. Gating execution is already answered.

---

## 5. Known gap, deferred

Making a predicate a task merges two graphs into one. Before it, routes formed
one graph and tasks another; after it, `if:` is an edge from a route to a task,
`deps:` is an edge between tasks, and `route:` is an edge from a task back to a
route. A cycle can now cross the boundary.

Nothing in the codebase can see such a cycle. There are two detectors, in
unrelated places — `tmpl/render.go:174` for variable dependencies, which names
every variable involved, and `walkChoice`/`Resolve` for routes — and `deps:`
has none at all in either world. Global `deps:` catches only the direct
`dep == name`; an indirect `a -> b -> a` terminates by accident on the `done`
map, having run `a` twice, which is what `run.go:47` says that map exists to
prevent.

The eventual answer is one walk over one graph at load — nodes being routes and
tasks, edges being `if:`, `then:`, `else:`, `deps:` and `route:` — reported in
`cycleError`'s style, naming every node in the cycle rather than the one that
closed it. It would also subsume the acyclic memo in §3.4.

Deliberately not built here. The existing per-feature detectors keep working on
their own graphs, the new boundary-crossing case is possible but takes a
deliberately strange file to reach, and the unified walk is worth designing once
against every edge type rather than bolted on beside this change.

---

## 5. Appendix: decisions taken

1. There is no `gates:` block. A predicate is an ordinary task, so it gets
   `desc:` and `deps:` for nothing — but NOT `internal:`, which would refuse it
   from the command line and close the only way to ask which branch applies
   (§3.1).
2. A task named by `if:` may not have a `route:`, refused at load; a non-zero
   exit from it is an answer, not a failure (§3.1).
3. `if:` accepts a task name and nothing else — no inline shell line, one form
   only, so the question always has a name, a `desc:` and a way to be run on its
   own (§3.2).
4. `else:` is required (§3.2).
5. The selector is defined independently of routes; routes are its first and
   currently only consumer (§3.2).
6. `when:` is removed, not aliased, and reserved for a scheduling sense (§3.3).
7. Predicates are lazy, and memoised for the duration of one run only (§3.4).
8. "A command succeeds when it exits 0" is stated once and is the definition
   every other key already uses. No success field, no struct, no new hook
   variable (§3.5).
9. No `not:`/`unless:` — `else:` being mandatory makes negation a swap (§3.6).
10. A selector picks a value and does not gate execution; gating stays with
    `platforms:`, `requires:` and `status:` (§4).
11. `--dry` resolves nothing and runs nothing. It prints the selector as a tree,
    every reachable route indented under the branch that leads to it, because
    `--dry` promising "nothing happens except the parts I judged safe" is not a
    promise. Which branch you would get is answered by running the predicate
    task itself (§3.2, §5).
12. A unified cycle detector over the merged route-and-task graph is deferred,
    with the gap written down (§5).
