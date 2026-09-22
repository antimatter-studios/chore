# Selector Rework Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the route-bound `when:`/`then:`/`else:` conditional with a general selector whose predicate is an ordinary chore task.

**Architecture:** `Route`'s conditional form currently holds a shell line inline. It becomes `if:` naming a task in the namespace. The predicate is run by the `Runner` rather than by a bare `exec.Command`, memoised for the run, refused at load if it carries a `route:`. `--dry` stops resolving and prints the selector as a tree. A new `Set`-level validation pass runs after every namespace has loaded, so a predicate or dependency may name another namespace's task.

**Tech Stack:** Go 1.x, `gopkg.in/yaml.v3`, `golang.org/x/crypto/ssh`. No new dependencies.

**Spec:** `docs/global-gates-and-selectors.md`

## Global Constraints

- Baseline is `feat/global-namespaces` at `4f815eb`. `go build ./...` and `go test ./internal/global/... ./internal/cli/...` are green there and must stay green after every task.
- `when:` is **removed**, not aliased. No compatibility shim: `global:` has never been released — latest tag `v0.11.0` is `670d7c1`, before the feature.
- `if:` accepts a **task name only**. No inline shell line, no second form.
- `else:` is required on every selector.
- A task named by `if:` may **not** declare `route:`. Refused at load.
- A non-zero exit from a predicate is an answer, not a failure: not reported, not propagated, does not stop the run.
- Predicates are lazy (never run at load or during a listing) and memoised for the duration of one run.
- `--dry` resolves nothing and runs nothing.
- Unprefixed task names mean the current namespace; `global:<ns>:<task>` names another's. Task names beginning with `global:` are refused at load.
- The manual is generated: edit the `chore:manual` comment in the source, then run `chore manual`. Never edit `internal/manual/topics/*.md` by hand. `chore manual:check` fails the build if they disagree.
- Error messages name the file (`n.Path`) and the thing that is wrong, following the existing style in `schema.go`.

## Review Focus

Five things the spec implies that no task's own happy-path test would catch. Each has a test assigned to the task that owns the code.

1. **A predicate that fails is not a failed run.** `chore global:ns:task` whose selector takes `else:` must exit 0 and print no error. Covered in Task 4.
2. **A listing must not execute anything.** `chore global:` and `chore global:ns:` on a namespace whose routes have selectors must not run the predicate. Covered in Task 4.
3. **`--dry` must not execute anything.** Same file, same requirement, and it is the one `--dry` exists for. Covered in Task 5.
4. **A predicate naming a missing task, or one that carries `route:`, must fail at load** with a message naming both. Covered in Task 3.
5. **A selector whose `then:` and `else:` name the same route** is legal but pointless, and a selector naming a route that does not exist must be refused. Covered in Task 2.

---

## File Structure

- `internal/global/schema.go` — `Route.If` replaces `Route.When`; `Task.validate` gains the predicate rules; `Namespace.Validate` keeps per-file checks.
- `internal/global/resolve.go` — `Resolve` takes a predicate runner instead of calling `predicateHolds`; `predicateHolds` is deleted.
- `internal/global/run.go` — `Runner` gains predicate execution and the memo; `Set.DryRun` prints a tree.
- `internal/global/crossref.go` — **new.** The `Set`-level validation pass, run after every namespace loads.
- `internal/global/load.go` — calls the new pass at the end of `Load`.

---

### Task 1: Rename `when:` to `if:`

Mechanical and semantics-preserving, done first so every later diff is about behaviour rather than spelling.

**Files:**
- Modify: `internal/global/schema.go` (the `Route` struct, `UnmarshalYAML`, `Conditional`, `validateChoice`, the `chore:manual` comment block)
- Modify: `internal/global/resolve.go` (`Resolve`)
- Modify: `internal/global/load_test.go:310-420`
- Regenerate: `internal/manual/topics/global.md`

**Interfaces:**
- Consumes: nothing.
- Produces: `Route.If string` replacing `Route.When string`. `Route.Conditional() bool` unchanged in name and meaning.

- [ ] **Step 1: Change the failing tests first**

In `internal/global/load_test.go`, replace every `when:` in a YAML fixture with `if:`. There are five, at lines 318, 319, 395, 400 and 407. For example line 318-319 become:

```go
  athome: { if: "true",  then: lan, else: pi }
  away:   { if: "false", then: lan, else: pi }
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/global/ -run 'Conditional' -v`
Expected: FAIL. `yaml: unmarshal errors: field if not found in type struct { When string ...}` — `KnownFields(true)` rejects the unknown key.

- [ ] **Step 3: Rename the field**

In `internal/global/schema.go`, in the `Route` struct:

```go
type Route struct {
	// Hops is set for the list form.
	Hops []Hop
	// If, Then and Else are set for the selector form. If names a TASK in this
	// namespace whose exit status is the answer; Then and Else name other routes.
	If   string
	Then string
	Else string
}
```

`Conditional`:

```go
func (r Route) Conditional() bool { return r.If != "" }
```

`UnmarshalYAML`'s mapping branch:

```go
	case yaml.MappingNode:
		var raw struct {
			If   string `yaml:"if"`
			Then string `yaml:"then"`
			Else string `yaml:"else"`
		}
		if err := n.Decode(&raw); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		r.If, r.Then, r.Else = raw.If, raw.Then, raw.Else
		return nil
	default:
		return fmt.Errorf("line %d: a route is a list of hops, or `if:`/`then:`/`else:`", n.Line)
```

In `validateChoice`, the missing-branch message:

```go
			return fmt.Errorf("%s: route %q has `if:` but no `%s:` — a choice needs both branches,"+
				" because the other one is a different route rather than doing nothing", n.Path, name, branch.key)
```

In `resolve.go`, `Resolve` reads `route.If` where it read `route.When`.

- [ ] **Step 4: Run the whole package**

Run: `go test ./internal/global/ ./internal/cli/`
Expected: PASS.

- [ ] **Step 5: Update the manual comment and regenerate**

In `schema.go`'s `Route` doc comment and in the `chore:manual global` block, replace `when:` with `if:` in the YAML examples and prose.

Run: `go run ./internal/manual/gen && git diff --stat internal/manual/topics`
Expected: `internal/manual/topics/global.md` changed.

- [ ] **Step 6: Commit**

```bash
git add internal/global internal/manual/topics
git commit -m "Rename when: to if:, the word for a condition

GitLab keeps if: for the condition and when: for scheduling, and that is a
distinction chore may want later. The Route doc comment already called this
construct if/else while the key said when:. Free to change now and never
again: global: has never been released."
```

---

### Task 2: `if:` names a task, checked at load

**Files:**
- Modify: `internal/global/schema.go` (`validateChoice`)
- Test: `internal/global/load_test.go`

**Interfaces:**
- Consumes: `Route.If` from Task 1.
- Produces: load-time refusal of a selector whose `if:` names a missing task, a task with a `route:`, or whose `then:`/`else:` name a missing route.

- [ ] **Step 1: Write the failing tests**

Add to `internal/global/load_test.go`. The existing `TestConditionalRoutesAreCheckedAtLoad` is table-driven; add cases to its table:

```go
		{
			name: "if names a task that does not exist",
			body: "name: x\nroutes:\n  a: [ { host: h } ]\n  c: { if: nope, then: a, else: a }\ntasks:\n  t: { route: c, cmd: [true] }\n",
			want: `names "nope" as its ` + "`if:`",
		},
		{
			name: "a predicate may not travel a route",
			body: "name: x\nroutes:\n  a: [ { host: h } ]\n  c: { if: p, then: a, else: a }\ntasks:\n  p: { route: a, cmd: [true] }\n  t: { route: c, cmd: [true] }\n",
			want: "must run on this machine",
		},
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/global/ -run TestConditionalRoutesAreCheckedAtLoad -v`
Expected: FAIL — both cases load without error, because nothing checks `if:` yet.

- [ ] **Step 3: Check the predicate in validateChoice**

In `internal/global/schema.go`, at the top of `validateChoice`, before the branch loop:

```go
	// The predicate is a task in this namespace, so a typo is caught here rather
	// than as a route that silently always takes `else:`.
	if !strings.HasPrefix(route.If, "global:") {
		p, ok := n.Tasks[route.If]
		if !ok {
			return fmt.Errorf("%s: route %q names %q as its `if:`, which is not a task in this namespace",
				n.Path, name, route.If)
		}
		// A predicate answers a question about THIS machine. One that travelled a
		// route would pay a connection to answer it — measured at 326-377 ms
		// against 0.65 ms on the LAN — which is the cost this whole shape exists
		// to avoid, and it would look like slowness rather than a mistake.
		if p.Route != "" {
			return fmt.Errorf("%s: route %q names task %q as its `if:`, but that task has `route: %s` —"+
				" a predicate must run on this machine", n.Path, name, route.If, p.Route)
		}
	}
```

A `global:`-prefixed `if:` is left to Task 6's cross-reference pass, which is the only place another namespace's tasks are visible.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/global/ -run TestConditionalRoutesAreCheckedAtLoad -v`
Expected: PASS.

- [ ] **Step 5: Run the package**

Run: `go test ./internal/global/ ./internal/cli/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/global
git commit -m "Refuse a predicate that does not exist or travels a route

A typo in if: would otherwise be a route that silently always takes else:,
and a predicate with a route: would pay a 300ms connection to answer a
question that costs 0.65ms locally."
```

---

### Task 3: Run the predicate as a task

**Files:**
- Modify: `internal/global/resolve.go` (`Resolve` signature; delete `predicateHolds`)
- Modify: `internal/global/run.go` (`Runner.run`, `Runner` gains the memo)
- Test: `internal/global/load_test.go`

**Interfaces:**
- Consumes: `Route.If` (Task 1), the load-time guarantees (Task 2).
- Produces:
  - `func (n *Namespace) Resolve(ctx context.Context, name string, ask Predicate) (Resolved, error)`
  - `type Predicate func(ctx context.Context, task string) bool`
  - `func (r *Runner) predicate(ctx context.Context, set *Set, n *Namespace) Predicate`

- [ ] **Step 1: Write the failing test**

Add to `internal/global/load_test.go`:

```go
// The predicate is a task, so the branch taken is decided by that task's exit
// status — and the task is the ordinary kind, with a desc and a name a person
// can run on its own.
func TestAPredicateTaskDecidesTheBranch(t *testing.T) {
	dir := write(t, map[string]string{"x.yaml": `
name: x
routes:
  lan: [ { host: lan.example } ]
  pi:  [ { host: pi.example } ]
  home: { if: on-lan, then: lan, else: pi }
tasks:
  on-lan:
    internal: true
    cmd: 'exit 0'
  t: { route: home, cmd: [true] }
`})
	set, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n := set.Namespaces["x"]
	r := &Runner{Out: io.Discard, Err: io.Discard}

	got, err := n.Resolve(context.Background(), "home", r.predicate(set, n))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "lan" {
		t.Errorf("route = %q, want lan — the predicate exited 0", got.Name)
	}
	if !strings.Contains(got.Why, "then -> lan") {
		t.Errorf("Why = %q, want it to name the branch", got.Why)
	}
}
```

`write(t, map[string]string) string` is the existing helper at
`internal/global/load_test.go:12`; it writes the files into a temp directory and
returns it. `io` must be added to that file's imports.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/global/ -run TestAPredicateTaskDecidesTheBranch -v`
Expected: FAIL to compile — `too many arguments in call to n.Resolve` and `r.predicate undefined`.

- [ ] **Step 3: Change Resolve to take a predicate**

In `internal/global/resolve.go`, delete `predicateHolds` entirely along with the now-unused `os/exec` import, and change the signature:

```go
// Predicate answers whether a named task in this namespace succeeded. Passed in
// rather than called directly, because running a task is the Runner's job and
// resolving a route must be possible without one — `--dry` resolves nothing and
// hands in a Predicate that is never called.
type Predicate func(ctx context.Context, task string) bool

func (n *Namespace) Resolve(ctx context.Context, name string, ask Predicate) (Resolved, error) {
	var trail []string
	seen := map[string]bool{}
	for {
		route, ok := n.Routes[name]
		if !ok {
			return Resolved{}, fmt.Errorf("no route %q in %s", name, n.Path)
		}
		if !route.Conditional() {
			return Resolved{Name: name, Hops: route.Hops, Why: strings.Join(trail, " ")}, nil
		}
		if seen[name] {
			return Resolved{}, fmt.Errorf("route %q leads back to itself", name)
		}
		seen[name] = true

		taken, branch := route.Else, "else"
		if ask(ctx, route.If) {
			taken, branch = route.Then, "then"
		}
		trail = append(trail, fmt.Sprintf("%s: %s -> %s", name, branch, taken))
		name = taken
	}
}
```

- [ ] **Step 4: Add the Runner's predicate, with the memo**

In `internal/global/run.go`, add a field to `Runner`:

```go
	// asked remembers each predicate's answer for the life of one run. A
	// predicate is a process, and the reason this shape prefers an interface
	// check over probing the far end is that predicates are paid for; running one
	// twice for one answer is the cheap version of the expensive mistake. Never
	// cached beyond the run — the answer to "am I on the LAN" is true until it is
	// not.
	asked map[string]bool
```

and the method:

```go
// predicate returns the function Resolve asks for a branch.
//
// A predicate's non-zero exit is an ANSWER, not a failure: it is not reported,
// its status is not propagated, and it does not stop the run. That inversion is
// the whole reason this is a separate path rather than an ordinary dependency.
func (r *Runner) predicate(set *Set, n *Namespace) Predicate {
	return func(ctx context.Context, name string) bool {
		t, err := set.dep(n, name)
		if err != nil {
			// Load-time validation guarantees this cannot happen; a false answer
			// is the safe reading if it somehow does.
			return false
		}
		if r.asked == nil {
			r.asked = map[string]bool{}
		}
		// Keyed by address, so a predicate named two ways is asked once — the
		// same rule the dependency memo follows.
		if answer, done := r.asked[t.Address()]; done {
			return answer
		}
		answer := r.runLocal(ctx, t, nil) == nil
		r.asked[t.Address()] = answer
		return answer
	}
}
```

`set.dep` is written in Task 6; until then this task may inline a `n.Tasks[name]`
lookup and Task 6 replaces it. Resolving through `dep` is what lets `if:` name
another namespace's task.

- [ ] **Step 5: Update every Resolve call site**

Three of them. In `run.go`'s `run`, both the `WithRoute` branch and the `Route` branch pass `r.predicate(set, n)`. In `run.go`'s `DryRun` — Task 5 rewrites it; for now pass a predicate that panics, so a missed path is loud:

```go
	resolved, err := n.Resolve(ctx, name, func(context.Context, string) bool {
		panic("--dry must not resolve a selector")
	})
```

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/global/ ./internal/cli/`
Expected: PASS, except any `--dry` test over a conditional route, which Task 5 fixes.

- [ ] **Step 7: Commit**

```bash
git add internal/global
git commit -m "Run the predicate as a task, and remember the answer

Resolve takes a Predicate rather than shelling out itself, so resolving a
route is possible without a Runner. The answer is memoised for the run: a
predicate is a process, and running one twice for one answer is the cheap
version of the expensive mistake."
```

---

### Task 4: A failed predicate is an answer, and a listing runs nothing

**Files:**
- Test: `internal/cli/global_test.go`

**Interfaces:**
- Consumes: Task 3's `Runner.predicate`.
- Produces: no new API. Pins two behaviours the spec requires and nothing else asserts.

- [ ] **Step 1: Write the failing tests**

Add to `internal/cli/global_test.go`. The helpers there are
`installGlobal(t, map[string]string) string`, `runMain(t, dir, args...)`,
`checkCode` and `checkContains` — same shapes the existing tests in that file
use.

```go
// A predicate that exits non-zero has ANSWERED. It is not a failed run, there is
// no error, and the else: branch is taken.
func TestAFailedPredicateIsNotAFailedRun(t *testing.T) {
	installGlobal(t, map[string]string{"x.yaml": `
name: x
routes:
  a: [ { host: a.example } ]
  b: [ { host: b.example } ]
  pick: { if: never, then: a, else: b }
tasks:
  never:
    internal: true
    cmd: 'exit 3'
  show:
    route: pick
    cmd: [true]
`})
	got := runMain(t, t.TempDir(), "--dry", "global:x:show")
	checkCode(t, got, 0)
	if strings.Contains(got.stderr, "exit status 3") {
		t.Errorf("stderr mentions the predicate's status:\n%s", got.stderr)
	}
}

// Listing is a question about what is installed. It must not run anything.
func TestListingDoesNotRunAPredicate(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	installGlobal(t, map[string]string{"x.yaml": fmt.Sprintf(`
name: x
routes:
  a: [ { host: a.example } ]
  pick: { if: touchy, then: a, else: a }
tasks:
  touchy:
    internal: true
    cmd: 'touch %s'
  show: { route: pick, cmd: [true] }
`, marker)})

	got := runMain(t, t.TempDir(), "global:x:")
	checkCode(t, got, 0)
	if _, err := os.Stat(marker); err == nil {
		t.Error("listing ran the predicate")
	}
}
```

`fmt` must be added to that file's imports.

- [ ] **Step 2: Run them**

Run: `go test ./internal/cli/ -run 'Predicate' -v`
Expected: the listing test passes already (listing never resolves); the failed-predicate test fails, because `--dry` still panics from Task 3's placeholder.

- [ ] **Step 3: No implementation**

Both behaviours are properties of code written in Task 3 and Task 5. If either test fails after Task 5, the bug is there, not here.

- [ ] **Step 4: Commit**

```bash
git add internal/cli
git commit -m "Pin that a failed predicate is an answer and a listing runs nothing

Neither is asserted by any happy-path test, and both are the kind of thing
that regresses silently."
```

---

### Task 5: `--dry` prints the tree and resolves nothing

**Files:**
- Modify: `internal/global/run.go` (`Set.DryRun`)
- Test: `internal/cli/global_test.go`

**Interfaces:**
- Consumes: `Route.If`/`Then`/`Else`, `Resolved`.
- Produces: `func (s *Set) DryRun(w io.Writer, address string) error`, unchanged signature, new output for selectors.

- [ ] **Step 1: Write the failing test**

```go
// --dry runs NOTHING, predicates included: a predicate is an ordinary task and
// may modify data, so "nothing happens except the parts I judged safe" is not a
// promise --dry can make. It prints both branches instead.
func TestDryRunPrintsBothBranchesAndRunsNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	installGlobal(t, map[string]string{"x.yaml": fmt.Sprintf(`
name: x
routes:
  lan:  [ { host: 192.168.0.47, user: chris } ]
  pi:   [ { host: s1.example.com, port: 10022, user: root } ]
  home: { if: on-lan, then: lan, else: pi }
tasks:
  on-lan:
    internal: true
    cmd: 'touch %s'
  pods: { route: home, cmd: [kubectl, get, pods] }
`, marker)})

	got := runMain(t, t.TempDir(), "--dry", "global:x:pods")
	checkCode(t, got, 0)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("--dry ran the predicate")
	}
	checkContains(t, got, "stdout", got.stdout,
		"if on-lan", "then -> lan", "else -> pi",
		"192.168.0.47", "s1.example.com",
	)
}
```

- [ ] **Step 2: Run it**

Run: `go test ./internal/cli/ -run TestDryRunPrintsBothBranches -v`
Expected: FAIL — panic from the Task 3 placeholder.

- [ ] **Step 3: Rewrite DryRun's route printing**

In `internal/global/run.go`, replace the `Resolve` call in `DryRun` with a recursive walk. The hop-printing that follows stays as it is; factor it into a helper the walk calls for each leaf:

```go
// printRoute prints one route, and for a selector prints BOTH branches as a
// tree rather than choosing between them.
//
// --dry resolves nothing because a predicate is an ordinary task and may modify
// data; "nothing happens except the parts I judged safe" is not a promise. Which
// branch you would actually get is answered by running the predicate itself,
// which is a named task precisely so that it can be.
//
// The walk terminates because a cycle among routes is refused at load, by
// walkChoice.
func printRoute(w io.Writer, n *Namespace, name string, indent string) {
	route, ok := n.Routes[name]
	if !ok {
		fmt.Fprintf(w, "%sroute %s: not declared in this file\n", indent, name)
		return
	}
	if !route.Conditional() {
		fmt.Fprintf(w, "%sroute %s:\n", indent, name)
		printHops(w, indent+"  ", route.Hops)
		return
	}
	fmt.Fprintf(w, "%sroute %s: if %s\n", indent, name, route.If)
	for _, branch := range []struct{ key, to string }{{"then", route.Then}, {"else", route.Else}} {
		fmt.Fprintf(w, "%s  %s -> %s\n", indent, branch.key, branch.to)
		printRoute(w, n, branch.to, indent+"    ")
	}
}
```

`printHops` is the existing hop-listing loop from `DryRun`, lifted out unchanged so both paths print hops identically — including the "dialled from hop N" lines, which are what make a multi-hop route readable.

`DryRun` then calls `printRoute(w, n, name, "")` where it called `Resolve`, and drops the `resolved.Why` block, which no longer has anything to report.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/global/ ./internal/cli/`
Expected: PASS, including Task 4's failed-predicate test.

- [ ] **Step 5: Update the manual comment and regenerate**

In `run.go`'s `DryRun` doc comment and the `chore:manual global` block in `schema.go`, describe the new output and state the rule: `--dry` resolves nothing and runs nothing.

Run: `go run ./internal/manual/gen`

- [ ] **Step 6: Commit**

```bash
git add internal/global internal/cli internal/manual/topics
git commit -m "--dry prints both branches and runs nothing

A predicate is an ordinary task and may modify data, so --dry cannot resolve
a selector and still promise that nothing happened. It prints the selector as
a tree instead, which shows more of the file than resolving did. Which branch
you would get is answered by running the predicate task, which has a name for
exactly that reason."
```

---

### Task 6: Cross-namespace references, checked after every namespace loads

**Files:**
- Create: `internal/global/crossref.go`
- Modify: `internal/global/load.go` (call the pass at the end of `Load`)
- Modify: `internal/global/schema.go` (`validate` skips `global:`-prefixed deps; refuse a task named `global:…`)
- Test: `internal/global/load_test.go`

**Interfaces:**
- Consumes: `Set`, `Namespace`, `Task`.
- Produces: `func (s *Set) crossCheck() error`, called by `Load` after all namespaces are in the map.

- [ ] **Step 1: Write the failing tests**

```go
// A dependency or a predicate may name another namespace's task, spelled as the
// full address — the same rule the command line uses, so the call site says
// where the thing came from.
func TestACrossNamespaceReferenceResolves(t *testing.T) {
	dir := write(t, map[string]string{
		"ssh.yaml": "name: ssh\ntasks:\n  unlock: { cmd: [true] }\n",
		"lab.yaml": "name: lab\nroutes:\n  a: [ { host: h } ]\ntasks:\n  pods: { route: a, cmd: [true], deps: [global:ssh:unlock] }\n",
	})
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestACrossNamespaceReferenceThatIsWrongIsRefused(t *testing.T) {
	dir := write(t, map[string]string{
		"lab.yaml": "name: lab\nroutes:\n  a: [ { host: h } ]\ntasks:\n  pods: { route: a, cmd: [true], deps: [global:ssh:unlock] }\n",
	})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a dependency on a namespace that is not installed")
	}
	if !strings.Contains(err.Error(), "ssh") {
		t.Errorf("error does not name the missing namespace: %v", err)
	}
}

// The prefix rule cannot be undermined by a file that defines a task called
// global:something.
func TestATaskNamedGlobalIsRefused(t *testing.T) {
	dir := write(t, map[string]string{
		"x.yaml": "name: x\ntasks:\n  global:ssh:unlock: { cmd: [true] }\n",
	})
	if _, err := Load(dir); err == nil {
		t.Fatal("Load accepted a task whose name begins with global:")
	}
}
```

- [ ] **Step 2: Run them**

Run: `go test ./internal/global/ -run 'CrossNamespace|NamedGlobal' -v`
Expected: FAIL. The first fails because `validate` refuses any dep not in the same namespace; the others fail because nothing checks.

- [ ] **Step 3: Let per-file validation defer prefixed names**

In `schema.go`'s `Task.validate`, the dep loop becomes:

```go
	for _, dep := range t.Deps {
		if strings.HasPrefix(dep, "global:") {
			// Another namespace's task, which is not visible from inside one file.
			// Checked by Set.crossCheck once every namespace has loaded.
			continue
		}
		if _, ok := n.Tasks[dep]; !ok {
			return fmt.Errorf("%s: task %q depends on %q, which is not a task in this namespace", n.Path, name, dep)
		}
		if dep == name {
			return fmt.Errorf("%s: task %q depends on itself", n.Path, name)
		}
	}
```

And in `Namespace.Validate`, before the per-task loop:

```go
	for name := range n.Tasks {
		if strings.HasPrefix(name, "global:") {
			return fmt.Errorf("%s: task %q is named with the `global:` prefix, which is how an address is"+
				" written — it could never be reached, and it would make `deps: [%s]` ambiguous", n.Path, name, name)
		}
	}
```

- [ ] **Step 4: Write the cross-reference pass**

Create `internal/global/crossref.go`:

```go
package global

import (
	"fmt"
	"strings"
)

// crossCheck validates every reference that names another namespace.
//
// Separate from Namespace.Validate because of when each can run: a file is
// validated as it is read, when no other namespace exists yet, and a reference
// to `global:ssh:unlock` cannot be checked until every file is in. Two passes,
// each checking what it can actually see.
func (s *Set) crossCheck() error {
	for _, n := range s.Namespaces {
		for name, t := range n.Tasks {
			for _, dep := range t.Deps {
				if err := s.checkRef(n, name, "depends on", dep); err != nil {
					return err
				}
			}
		}
		for name, route := range n.Routes {
			if !route.Conditional() || !strings.HasPrefix(route.If, "global:") {
				continue
			}
			p, err := s.resolveRef(n, route.If)
			if err != nil {
				return fmt.Errorf("%s: route %q names %q as its `if:`: %w", n.Path, name, route.If, err)
			}
			if p.Route != "" {
				return fmt.Errorf("%s: route %q names task %q as its `if:`, but that task has `route: %s` —"+
					" a predicate must run on this machine", n.Path, name, route.If, p.Route)
			}
		}
	}
	return nil
}

func (s *Set) checkRef(n *Namespace, from, verb, ref string) error {
	if !strings.HasPrefix(ref, "global:") {
		return nil
	}
	if _, err := s.resolveRef(n, ref); err != nil {
		return fmt.Errorf("%s: task %q %s %q: %w", n.Path, from, verb, ref, err)
	}
	return nil
}

// resolveRef looks up a full address, reusing Lookup so the error naming what IS
// installed is written once.
func (s *Set) resolveRef(_ *Namespace, ref string) (*Task, error) {
	return s.Lookup(strings.TrimPrefix(ref, "global:"))
}
```

- [ ] **Step 5: Call it from Load**

In `internal/global/load.go`, at the end of `Load`, replacing `return set, nil`:

```go
	if err := set.crossCheck(); err != nil {
		return nil, err
	}
	return set, nil
```

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/global/ ./internal/cli/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/global
git commit -m "Check cross-namespace references once every namespace is in

A file is validated as it is read, when no other namespace exists yet, so
global:ssh:unlock cannot be checked there. Two passes, each checking what it
can see. Also refuses a task named with the global: prefix, which could never
be reached and would make a dep ambiguous."
```

---

### Task 7: Resolve the deps memo by address

**Files:**
- Modify: `internal/global/run.go` (`Runner.run`)
- Test: `internal/global/load_test.go`

**Interfaces:**
- Consumes: Task 6's cross-namespace deps.
- Produces: `done` keyed by `Task.Address()`, and dependencies resolved across namespaces at run time.

- [ ] **Step 1: Write the failing test**

```go
// One dependency written two ways is one dependency. From inside a namespace a
// task may be named either way, and keying the memo on the spelling would run it
// twice — which is exactly what the map exists to prevent.
func TestADependencyWrittenTwoWaysRunsOnce(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "count")
	dir := write(t, map[string]string{"x.yaml": fmt.Sprintf(`
name: x
tasks:
  once:
    internal: true
    cmd: 'echo . >> %s'
  both:
    deps: [once, global:x:once]
    cmd: [true]
`, marker)})
	set, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := &Runner{Out: io.Discard, Err: io.Discard}
	if err := r.Run(context.Background(), set, "x:both"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	if n := strings.Count(string(data), "."); n != 1 {
		t.Errorf("the dependency ran %d times, want 1", n)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test ./internal/global/ -run TestADependencyWrittenTwoWays -v`
Expected: FAIL — the dependency runs twice, because `done` is keyed on the two different strings.

- [ ] **Step 3: Resolve to an address before consulting the memo**

In `internal/global/run.go`, `run`'s dep loop:

```go
	for _, name := range t.Deps {
		dep, err := set.dep(n, name)
		if err != nil {
			return fmt.Errorf("%s: dependency %q: %w", t.Address(), name, err)
		}
		if done[dep.Address()] {
			continue
		}
		done[dep.Address()] = true
		if err := r.run(ctx, set, dep, done); err != nil {
			return fmt.Errorf("%s: dependency %q: %w", t.Address(), name, err)
		}
	}
```

And mark the root before recursing, in `Run`:

```go
	return r.run(ctx, set, t, map[string]bool{t.Address(): true})
```

Add the lookup to `crossref.go`:

```go
// dep resolves a dependency's name to the task it means: a full address names
// another namespace's task, and a bare name means this one's.
func (s *Set) dep(n *Namespace, name string) (*Task, error) {
	if strings.HasPrefix(name, "global:") {
		return s.Lookup(strings.TrimPrefix(name, "global:"))
	}
	t, ok := n.Tasks[name]
	if !ok {
		return nil, fmt.Errorf("not a task in %s", n.Path)
	}
	return t, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/global/ ./internal/cli/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/global
git commit -m "Key the dependency memo by address, and mark the root

From inside a namespace a task can be named either way, so keying on the
spelling would run one dependency twice. Marking the root turns an indirect
cycle from 'runs a task twice' into something a future cycle check can name."
```

---

### Task 8: Document the selector

**Files:**
- Modify: `internal/global/schema.go` (the `chore:manual global` block)
- Regenerate: `internal/manual/topics/global.md`
- Modify: `SPEC.md`, `README.md`, `CHANGELOG.md`

- [ ] **Step 1: Write the manual section**

In the `chore:manual global` comment block in `schema.go`, replace the conditional-route prose with:

```
// ## A route can be a choice
//
// ```yaml
// routes:
//   lan:  [ { host: 192.168.0.47, user: chris } ]
//   pi:   [ { host: s1.example.com, port: 10022, user: root } ]
//   home: { if: on-lan, then: lan, else: pi }
//
// tasks:
//   on-lan:
//     desc: this machine is on the home network
//     internal: true
//     cmd: ifconfig | grep -q 'inet 192\.168\.0\.'
// ```
//
// `if:` names a TASK, and nothing else — no inline shell line. A command
// **succeeds** when it exits 0, and the `if:` is true when its task succeeds.
// That is the same word `ignore_error:`, `on_failure` and `status:` already
// mean.
//
// Naming the task rather than inlining the test buys four things: the question
// has a `desc:`, so a listing says what is being asked; it can be run on its
// own, which is how a route choosing the wrong branch gets debugged; the parser
// never has to guess whether a string is a name or a shell line; and shell stays
// out of the routing table, which is read on machines its author is not sitting
// at.
//
// **`else:` is required.** The case this exists for is not "do something extra"
// — it is that the same task has two correct routes and which one is correct
// depends on where the laptop is. A missing branch would be a task that silently
// does nothing somewhere. It also means no `not:` is ever needed: negation is
// swapping the two names.
//
// **A predicate may not have a `route:`.** Refused at load. Measured on the
// route this was built for, a hop is 326-377 ms against 0.65 ms on the LAN —
// about 500x — so a predicate that travelled would pay a connection to answer a
// question, and it would look like slowness rather than a mistake.
//
// **A predicate exiting non-zero has answered.** It is not a failed run, nothing
// is reported, and the `else:` branch is taken. The answer is remembered for the
// rest of the run, so two routes sharing a predicate ask it once.
//
// **`--dry` resolves nothing.** A predicate is an ordinary task and may modify
// data, so "nothing happens except the parts chore judged safe" is not a promise
// `--dry` can make. It prints both branches instead, every reachable route
// indented under the branch that leads to it. To find out which branch you are
// on, run the predicate.
```

- [ ] **Step 2: Regenerate and check**

Run: `go run ./internal/manual/gen && go test ./... && git diff --stat internal/manual/topics`
Expected: PASS, and `global.md` updated.

- [ ] **Step 3: Update SPEC.md, README.md and CHANGELOG.md**

Add the selector to the `global:` section of `SPEC.md` in the same voice as its neighbours, and a CHANGELOG entry under an Unreleased heading naming `if:`/`then:`/`else:` and the removal of `when:`.

- [ ] **Step 4: Verify the manual is not stale**

Run: `chore manual:check` (or `go run ./internal/manual/gen && git diff --exit-code -- internal/manual/topics`)
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add internal/global internal/manual/topics SPEC.md README.md CHANGELOG.md
git commit -m "docs: the selector, and why the predicate has a name"
```

---

### Task 9: One cycle detector, in a package both worlds use

Two detectors exist in unrelated places and `deps:` has none in either world.
This reconciles them.

**Files:**
- Create: `internal/graph/graph.go`
- Create: `internal/graph/graph_test.go`
- Modify: `internal/tmpl/render.go` (`Resolve` checks before resolving; delete `cycleError`)
- Modify: `internal/global/crossref.go` (build the merged graph, check it)
- Modify: `internal/global/schema.go` (delete `walkChoice`; `validateChoice` keeps its branches-exist checks)

**Interfaces:**
- Consumes: Task 6's `Set.crossCheck`.
- Produces:
  - `type Graph struct{ ... }`, `func New() *Graph`
  - `func (g *Graph) Edge(from, to string)`
  - `func (g *Graph) Describe(node, text string)` — how a node is named in an error
  - `func (g *Graph) Check() error` — nil, or a cycle naming every node in it

- [ ] **Step 1: Write the failing tests**

Create `internal/graph/graph_test.go`:

```go
package graph

import "testing"

func TestAnAcyclicGraphPasses(t *testing.T) {
	g := New()
	g.Edge("a", "b")
	g.Edge("b", "c")
	g.Edge("a", "c") // a diamond is not a cycle
	if err := g.Check(); err != nil {
		t.Errorf("Check: %v", err)
	}
}

// The message names every node in the cycle, in order, because the edit that
// fixes it could be at any of them — the node that happened to close the loop
// is not more responsible than the others.
func TestACycleNamesEveryNodeInIt(t *testing.T) {
	g := New()
	g.Edge("a", "b")
	g.Edge("b", "c")
	g.Edge("c", "a")
	err := g.Check()
	if err == nil {
		t.Fatal("Check accepted a cycle")
	}
	for _, want := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestANodeReferringToItselfIsACycle(t *testing.T) {
	g := New()
	g.Edge("a", "a")
	if err := g.Check(); err == nil {
		t.Fatal("Check accepted a self-edge")
	}
}

// A graph with a shared subtree must not be walked exponentially: the path-based
// seen set that detects the cycle is not enough on its own to bound the walk.
func TestASharedSubtreeIsWalkedOnce(t *testing.T) {
	g := New()
	for i := 0; i < 40; i++ {
		g.Edge(fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1))
		g.Edge(fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1))
	}
	if err := g.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// A node's description is what the error calls it, so a caller can say "route
// pi" or "var CONFIG" rather than leaking an internal key.
func TestDescriptionsAppearInTheError(t *testing.T) {
	g := New()
	g.Describe("r:pi", "route pi")
	g.Describe("t:unlock", "task unlock")
	g.Edge("r:pi", "t:unlock")
	g.Edge("t:unlock", "r:pi")
	err := g.Check()
	if err == nil {
		t.Fatal("Check accepted a cycle")
	}
	if !strings.Contains(err.Error(), "route pi") || !strings.Contains(err.Error(), "task unlock") {
		t.Errorf("error %q does not use the descriptions", err)
	}
}
```

Add `fmt` and `strings` to the test imports.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/graph/ -v`
Expected: FAIL to build — no such package.

- [ ] **Step 3: Write the package**

Create `internal/graph/graph.go`:

```go
// Package graph answers one question for the rest of chore: does this set of
// references contain a cycle, and if so which things are in it.
//
// It exists because the question kept being answered separately. Variables had
// a detector fused into their resolver, routes had a hand-written walk, and
// `deps:` had nothing at all in either the project or the global world — an
// indirect cycle there terminated by accident on a memo, having run a task
// twice. Three answers, two of them partial, and the one gap was in the feature
// people use most.
//
// Nodes are opaque strings. A caller that has more than one kind of node
// prefixes them, so a route and a task of the same name are different nodes, and
// gives each a Describe so the error reads in the caller's own words rather than
// in its keys.
package graph

import (
	"fmt"
	"sort"
	"strings"
)

// Graph is a directed graph of named nodes. The zero value is not usable; call
// New.
type Graph struct {
	edges map[string][]string
	names map[string]string
	order []string // insertion order of first mention, so Check is deterministic
}

func New() *Graph {
	return &Graph{edges: map[string][]string{}, names: map[string]string{}}
}

// Edge records that from refers to to. Both ends are created if unknown.
func (g *Graph) Edge(from, to string) {
	g.touch(from)
	g.touch(to)
	g.edges[from] = append(g.edges[from], to)
}

// Describe sets how a node is named in an error. Without one, the node's own
// key is used.
func (g *Graph) Describe(node, text string) {
	g.touch(node)
	g.names[node] = text
}

func (g *Graph) touch(node string) {
	if _, known := g.edges[node]; !known {
		g.edges[node] = nil
		g.order = append(g.order, node)
	}
}

func (g *Graph) describe(node string) string {
	if text, ok := g.names[node]; ok {
		return text
	}
	return node
}

// Check reports the first cycle it finds, naming every node in it.
//
// Depth-first, with two sets rather than one. `onPath` detects the cycle and is
// popped on the way out; `done` bounds the walk. Without the second, a graph
// whose branches converge is re-walked once per path through it — forty nodes
// with two converging edges each is 2^40 walks, which is a hang at load with no
// error to show for it.
func (g *Graph) Check() error {
	done := map[string]bool{}
	onPath := map[string]bool{}
	var path []string

	var visit func(node string) []string
	visit = func(node string) []string {
		if onPath[node] {
			// Trim the path to where this node first appears: everything before
			// it leads to the cycle without being in it.
			for i, n := range path {
				if n == node {
					return append(append([]string{}, path[i:]...), node)
				}
			}
			return []string{node, node}
		}
		if done[node] {
			return nil
		}
		onPath[node] = true
		path = append(path, node)
		for _, next := range g.edges[node] {
			if cycle := visit(next); cycle != nil {
				return cycle
			}
		}
		path = path[:len(path)-1]
		delete(onPath, node)
		done[node] = true
		return nil
	}

	for _, node := range g.order {
		if cycle := visit(node); cycle != nil {
			return g.cycleError(cycle)
		}
	}
	return nil
}

// cycleError names every node in the cycle and what each one points at, so the
// message points at the edit that fixes it rather than only at where the loop
// closed.
func (g *Graph) cycleError(cycle []string) error {
	parts := make([]string, 0, len(cycle)-1)
	for i := 0; i < len(cycle)-1; i++ {
		parts = append(parts, fmt.Sprintf("%s references %s", g.describe(cycle[i]), g.describe(cycle[i+1])))
	}
	return fmt.Errorf("cycle: %s", strings.Join(parts, "; "))
}

// Sorted is a helper for callers building a graph from a map, so the order
// edges are added in does not change which cycle is reported first.
func Sorted[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/graph/ -v`
Expected: PASS.

- [ ] **Step 5: Move the variable detector onto it**

In `internal/tmpl/render.go`, before the resolution loop in `Scope.Resolve`, build the graph and check it:

```go
	// Checked BEFORE resolving anything, which is a change: the pass-based loop
	// below discovers a cycle only when a pass resolves nothing, by which point
	// it may already have run a `sh:` var for its side effects. A cycle is a
	// mistake in the file and nothing should happen because of it.
	g := graph.New()
	for _, name := range graph.Sorted(vars) {
		g.Describe("var:"+name, "var "+name)
		text, _ := source(vars[name])
		refs, err := references(text)
		if err != nil {
			return nil, fmt.Errorf("var %s: %w", name, err)
		}
		for _, r := range refs {
			// A var may reference its own name to read the inherited value from
			// the scope, which is not a cycle.
			if r == name {
				continue
			}
			if _, ours := vars[r]; ours {
				g.Edge("var:"+name, "var:"+r)
			}
		}
	}
	if err := g.Check(); err != nil {
		return nil, fmt.Errorf("variable dependency %w", err)
	}
```

Delete `cycleError` and the `if !progressed { ... }` branch's call to it; a pass
that resolves nothing is now unreachable, so replace it with:

```go
		if !progressed {
			// Unreachable: the graph was checked above. Kept as a guard rather
			// than a panic, because being wrong about that should not crash.
			return nil, fmt.Errorf("variable dependency cycle among %s", strings.Join(slices.Sorted(maps.Keys(pending)), ", "))
		}
```

- [ ] **Step 6: Run the tmpl tests**

Run: `go test ./internal/tmpl/ -v`
Expected: PASS. If a test asserts the exact old message `variable dependency cycle: X references Y`, the new one reads `variable dependency cycle: var X references var Y` — update the assertion, and check the change reads better rather than merely differently.

- [ ] **Step 7: Move the route walk onto it, and add the deps edges**

In `internal/global/crossref.go`, extend `crossCheck` to build one graph over both node kinds and check it:

```go
	g := graph.New()
	for _, nsName := range graph.Sorted(s.Namespaces) {
		n := s.Namespaces[nsName]
		for _, name := range graph.Sorted(n.Routes) {
			route := n.Routes[name]
			id := "route:" + nsName + ":" + name
			g.Describe(id, fmt.Sprintf("route %s", name))
			if !route.Conditional() {
				continue
			}
			g.Edge(id, s.nodeFor(n, route.If))
			g.Edge(id, "route:"+nsName+":"+route.Then)
			g.Edge(id, "route:"+nsName+":"+route.Else)
		}
		for _, name := range graph.Sorted(n.Tasks) {
			t := n.Tasks[name]
			id := "task:" + nsName + ":" + name
			g.Describe(id, "task "+t.Address())
			for _, dep := range t.Deps {
				g.Edge(id, s.nodeFor(n, dep))
			}
			if t.Route != "" {
				g.Edge(id, "route:"+nsName+":"+t.Route)
			}
		}
	}
	if err := g.Check(); err != nil {
		return err
	}
```

with:

```go
// nodeFor turns a task reference into a graph node id, resolving a full address
// to the namespace it names.
func (s *Set) nodeFor(n *Namespace, ref string) string {
	if !strings.HasPrefix(ref, "global:") {
		return "task:" + n.Name + ":" + ref
	}
	rest := strings.TrimPrefix(ref, "global:")
	ns, task, ok := strings.Cut(rest, ":")
	if !ok {
		return "task:" + rest
	}
	return "task:" + ns + ":" + task
}
```

Then delete `walkChoice` from `schema.go` and the cycle half of `validateChoice`
— its checks that `then:` and `else:` name real routes stay, since those are
per-file and catch the typo earlier with a better message.

- [ ] **Step 8: Write the test that proves the merged graph is covered**

Add to `internal/global/load_test.go`:

```go
// The cycle this exists for crosses the boundary that used to separate two
// graphs: a route chooses by a task, that task depends on another, and that one
// travels the route. Neither the old route walk nor the deps memo could see it.
func TestACycleThroughARouteAndATaskIsRefused(t *testing.T) {
	dir := write(t, map[string]string{"x.yaml": `
name: x
routes:
  a: [ { host: h } ]
  pick: { if: ask, then: a, else: a }
tasks:
  ask:
    internal: true
    deps: [loop]
    cmd: [true]
  loop:
    internal: true
    route: pick
    cmd: [true]
`})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a cycle running through a route and a task")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error does not call it a cycle: %v", err)
	}
}
```

Note this file also fails Task 2's rule — `loop` has a `route:` and is reached
from `ask`, not named by `if:`, so Task 2's check does not fire on it. If it
does fire, adjust the fixture so the predicate itself is route-free and the
cycle runs through its dependency.

- [ ] **Step 9: Run everything**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/graph internal/tmpl internal/global
git commit -m "One cycle detector, in a package both worlds use

Three answers to one question, two of them partial. Variables had a detector
fused into their resolver, routes had a hand-written walk, and deps: had none
at all in either world — an indirect cycle there terminated by accident on a
memo, having run a task twice.

internal/graph is a directed graph of opaque string nodes with a Describe so
errors read in the caller's words. Check is depth-first with two sets: onPath
finds the cycle, done bounds the walk, because a graph whose branches converge
is otherwise re-walked once per path through it.

Variables are now checked BEFORE resolution rather than when a pass stops
making progress, so a cycle no longer runs a sh: var on the way to being
discovered. And the global world gets one graph over routes and tasks together,
which is the only place the boundary-crossing cycle is visible at all."
```
