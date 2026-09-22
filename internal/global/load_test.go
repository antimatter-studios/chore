package global

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write puts one namespace file in a temp global.d and returns the directory.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const homelab = `
name: homelab
routes:
  pi:
    - { host: s1.example.com, port: 10022, user: root }
    - { host: 127.0.0.1, port: 2222, user: chris }
tasks:
  k3s:pods:
    desc: pods everywhere
    route: pi
    cmd: [kubectl, get, pods, -A]
  k3s:proxy:
    route: pi
    forward: { remote: 127.0.0.1:6443, local: 127.0.0.1:6443 }
`

func TestLoadReadsANamespace(t *testing.T) {
	set, err := Load(write(t, map[string]string{"homelab.yaml": homelab}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n, ok := set.Namespaces["homelab"]
	if !ok {
		t.Fatalf("no homelab namespace in %v", sortedKeys(set.Namespaces))
	}
	if len(n.Routes["pi"].Hops) != 2 {
		t.Errorf("route pi has %d hops, want 2", len(n.Routes["pi"].Hops))
	}
	task, err := set.Lookup("homelab:k3s:pods")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	// A task name may contain colons of its own; the namespace is what precedes
	// the FIRST one, or `k3s:pods` would be unaddressable.
	if task.Name != "k3s:pods" || task.Address() != "global:homelab:k3s:pods" {
		t.Errorf("resolved %q / %q", task.Name, task.Address())
	}
}

// ssh's own defaults, filled in at load so a listing and an error name the port
// and user the connection will actually use.
func TestHopDefaultsMatchSSH(t *testing.T) {
	set, err := Load(write(t, map[string]string{"x.yaml": `
name: x
routes:
  r: [ { host: example.com } ]
tasks:
  t: { route: r, cmd: [true] }
`}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hop := set.Namespaces["x"].Routes["r"].Hops[0]
	if hop.Port != 22 {
		t.Errorf("port = %d, want 22", hop.Port)
	}
	if hop.User == "" {
		t.Error("user should default to the local username, as ssh does")
	}
}

// A missing directory is the ordinary state of a machine with no global tasks.
// A file that IS there and wrong is not ordinary, and must not be skipped: a
// namespace that failed to load silently is a command that stopped existing
// without saying so.
func TestAMissingDirectoryIsNotAnError(t *testing.T) {
	set, err := Load(filepath.Join(t.TempDir(), "nothing-here"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Namespaces) != 0 {
		t.Errorf("got %d namespaces", len(set.Namespaces))
	}
}

func TestLoadRefusesWhatCannotWork(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{
			name: "no name",
			body: "routes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, cmd: [true] }\n",
			want: "needs a `name:`",
		},
		{
			name: "a colon in the name",
			body: "name: a:b\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, cmd: [true] }\n",
			want: "cannot contain a colon",
		},
		{
			name: "a route with no hops",
			body: "name: x\nroutes:\n  r: []\ntasks:\n  t: { route: r, cmd: [true] }\n",
			want: "has no hops",
		},
		{
			name: "a hop with no host",
			body: "name: x\nroutes:\n  r: [ { port: 22 } ]\ntasks:\n  t: { route: r, cmd: [true] }\n",
			want: "needs a `host:`",
		},
		{
			// A tunnel with no route has nowhere to forward FROM, unlike a command,
			// which simply runs here.
			name: "a forward with no route",
			body: "name: x\ntasks:\n  t: { forward: { remote: a:1, local: b:2 } }\n",
			want: "there is nowhere to forward from",
		},
		{
			// Named a route that does not exist: the message lists the ones that do,
			// because the answer is almost always a typo.
			name: "an unknown route",
			body: "name: x\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: nope, cmd: [true] }\n",
			want: `which is not one of: r`,
		},
		{
			name: "both cmd and forward",
			body: "name: x\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, cmd: [true], forward: { remote: a:1, local: b:2 } }\n",
			want: "sets both `cmd:` and `forward:`",
		},
		{
			name: "neither cmd nor forward",
			body: "name: x\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r }\n",
			want: "needs `cmd:`",
		},
		{
			name: "a forward that is not host:port",
			body: "name: x\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, forward: { remote: 6443, local: 127.0.0.1:6443 } }\n",
			want: "which is not host:port",
		},
		{
			// The same rule a taskfile follows: a typo in a key is likelier than a
			// deliberate extension, and ignoring it turns the typo into silence.
			name: "an unknown field",
			body: "name: x\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, cmdd: [true] }\n",
			want: "field cmdd not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, map[string]string{"x.yaml": tc.body}))
			if err == nil {
				t.Fatal("want an error at load time")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// One key says WHAT to run and another says WHERE: a task with a route runs at
// the far end of it, and a task without one runs here. That is the whole rule,
// and it replaced three keys that all meant "run a thing" with no way to tell
// which to reach for.
func TestNoRouteMeansItRunsHere(t *testing.T) {
	set, err := Load(write(t, map[string]string{"x.yaml": `
name: x
routes:
  r: [ { host: h } ]
tasks:
  unlock:
    internal: true
    cmd: [trove, unlock, /vault.kdbx, --env]
  check:
    deps: [unlock]
    route: r
    cmd: [hostname]
`}))
	if err != nil {
		t.Fatalf("a routeless task is legal and runs here: %v", err)
	}
	n := set.Namespaces["x"]
	if n.Tasks["unlock"].Route != "" {
		t.Error("the local task should name no route")
	}
	if got := n.Tasks["check"].Deps; len(got) != 1 || got[0] != "unlock" {
		t.Errorf("deps = %v", got)
	}
}

// The two forms of cmd: are not shorthands for each other. The list is quoted by
// chore so an argument survives whole; the string is passed through so it can
// pipe — which the list form cannot express at all.
func TestCmdTakesEitherForm(t *testing.T) {
	set, err := Load(write(t, map[string]string{"x.yaml": `
name: x
routes:
  r: [ { host: h } ]
tasks:
  argv:  { route: r, cmd: [kubectl, get, pods, -A] }
  line:  { route: r, cmd: 'kubectl get pods -A | wc -l' }
`}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := set.Namespaces["x"].Tasks["argv"].Cmd.String(); got != `'kubectl' 'get' 'pods' '-A'` {
		t.Errorf("argv form renders as %s", got)
	}
	if got := set.Namespaces["x"].Tasks["line"].Cmd.String(); got != "kubectl get pods -A | wc -l" {
		t.Errorf("shell form renders as %q, want it byte for byte", got)
	}
}

// A dependency that does not exist is a typo, and naming it at load is the
// difference between a clear error and a task that half-runs.
func TestAnUnknownDependencyIsRefused(t *testing.T) {
	_, err := Load(write(t, map[string]string{"x.yaml": `
name: x
routes:
  r: [ { host: h } ]
tasks:
  t: { route: r, cmd: [true], deps: [nope] }
`}))
	if err == nil || !strings.Contains(err.Error(), "which is not a task in this namespace") {
		t.Fatalf("err = %v", err)
	}
}

// Two files claiming one name means one of them is unreachable, and which one
// would depend on directory order.
func TestTwoNamespacesCannotShareAName(t *testing.T) {
	_, err := Load(write(t, map[string]string{
		"a.yaml": "name: same\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, cmd: [true] }\n",
		"b.yaml": "name: same\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, cmd: [true] }\n",
	}))
	if err == nil || !strings.Contains(err.Error(), "two namespaces are called") {
		t.Fatalf("err = %v, want a collision naming both files", err)
	}
	for _, want := range []string{"a.yaml", "b.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// .yml as well as .yaml. One spelling would be tidier; the other would fail
// silently, which is the trade this program never takes.
func TestBothYamlSpellingsLoad(t *testing.T) {
	set, err := Load(write(t, map[string]string{
		"a.yaml": "name: a\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, cmd: [true] }\n",
		"b.yml":  "name: b\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, cmd: [true] }\n",
		"c.txt":  "not a namespace",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Namespaces) != 2 {
		t.Errorf("loaded %v, want a and b", sortedKeys(set.Namespaces))
	}
}

// $XDG_CONFIG_HOME when set, ~/.config otherwise — and the fallback is the part
// that matters, because the variable is unset on macOS by default and these
// files arrive on both platforms from one dotfiles repository.
func TestDirHonoursXDGAndFallsBack(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/somewhere/config")
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/somewhere/config", "chore", "global.d"); dir != want {
		t.Errorf("Dir() = %q, want %q", dir, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	dir, err = Dir()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, ".config", "chore", "global.d"); dir != want {
		t.Errorf("Dir() = %q, want %q", dir, want)
	}
}

// An address that names a namespace and no task is a question, not a mistake:
// the answer says how to see what is in it.
func TestANamespaceWithoutATaskSaysHowToLook(t *testing.T) {
	set, err := Load(write(t, map[string]string{"homelab.yaml": homelab}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = set.Lookup("homelab")
	if err == nil || !strings.Contains(err.Error(), "chore global:homelab:") {
		t.Fatalf("err = %v, want it to suggest the listing", err)
	}
}

// A route can be a CHOICE between two others, decided per invocation by a shell
// predicate on this machine. if/else and not if: the other branch is a different
// route, not doing nothing.
func TestAConditionalRoutePicksABranch(t *testing.T) {
	set, err := Load(write(t, map[string]string{"x.yaml": `
name: x
routes:
  lan: [ { host: 192.168.0.47, user: chris } ]
  pi:
    - { host: s1.example.com, port: 10022, user: root }
    - { host: 127.0.0.1, port: 2222, user: chris }
  athome: { if: yes-here, then: lan, else: pi }
  away:   { if: no-here,  then: lan, else: pi }
tasks:
  yes-here: { internal: true, cmd: 'exit 0' }
  no-here:  { internal: true, cmd: 'exit 1' }
  t: { route: athome, cmd: [true] }
`}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n := set.Namespaces["x"]

	r := &Runner{Out: io.Discard, Err: io.Discard}
	home, err := n.Resolve(context.Background(), "athome", r.predicate(set, n, map[string]bool{}, map[string]bool{}))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if home.Name != "lan" || len(home.Hops) != 1 {
		t.Errorf("a true predicate resolved to %q with %d hops", home.Name, len(home.Hops))
	}
	// The trail is what --dry prints. A predicate that quietly always takes one
	// branch looks exactly like one that works, so the choice has to be visible
	// without running the task.
	if !strings.Contains(home.Why, "then -> lan") {
		t.Errorf("Why = %q, want it to name the branch", home.Why)
	}

	away, err := n.Resolve(context.Background(), "away", r.predicate(set, n, map[string]bool{}, map[string]bool{}))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if away.Name != "pi" || len(away.Hops) != 2 {
		t.Errorf("a false predicate resolved to %q with %d hops", away.Name, len(away.Hops))
	}
}

// The values a tool that does its own ssh needs: the target hop, and everything
// before it in the -J form ssh itself takes.
func TestResolvedEnvIsAConnectionSpec(t *testing.T) {
	set, err := Load(write(t, map[string]string{"x.yaml": `
name: x
routes:
  pi:
    - { host: s1.example.com, port: 10022, user: root }
    - { host: 127.0.0.1, port: 2222, user: chris }
  direct: [ { host: 192.168.0.47, user: chris } ]
tasks:
  t: { route: pi, cmd: [true] }
`}))
	if err != nil {
		t.Fatal(err)
	}
	n := set.Namespaces["x"]

	jumped, _ := n.Resolve(context.Background(), "pi", neverAsked(t))
	env := strings.Join(jumped.Env(), " ")
	for _, want := range []string{
		"CHORE_ROUTE_HOST=127.0.0.1",
		"CHORE_ROUTE_PORT=2222",
		"CHORE_ROUTE_USER=chris",
		"CHORE_ROUTE_JUMP=root@s1.example.com:10022",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("env %q is missing %q", env, want)
		}
	}
	// A direct route has no jump, and an empty value is the honest answer rather
	// than the variable being absent — a consumer interpolating it gets "".
	direct, _ := n.Resolve(context.Background(), "direct", neverAsked(t))
	if !strings.Contains(strings.Join(direct.Env(), " "), "CHORE_ROUTE_JUMP=") {
		t.Error("a direct route should still declare an empty jump")
	}
}

// A mapping route is decoded by Route.UnmarshalYAML, which the outer decoder's
// KnownFields cannot reach into. Without a check of its own a typo is dropped
// and the route merely stops being a selector — reported as "has no hops",
// which names neither the key nor the mistake.
func TestAnUnknownKeyInASelectorIsRefused(t *testing.T) {
	dir := write(t, map[string]string{
		"x.yaml": "name: x\nroutes:\n  a: [ { host: h } ]\n  c: { if: p, thn: a, else: a }\ntasks:\n  p: { cmd: [true] }\n  t: { route: c, cmd: [true] }\n",
	})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a selector with an unknown key")
	}
	if !strings.Contains(err.Error(), "thn") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

// neverAsked is the predicate for a route that has no selector in it: a plain
// hop list must resolve without anything being run, and this fails the test if
// that stops being true.
func neverAsked(t *testing.T) Predicate {
	t.Helper()
	return func(_ context.Context, task string) bool {
		t.Errorf("a route with no selector asked for predicate %q", task)
		return false
	}
}

func TestConditionalRoutesAreCheckedAtLoad(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{
			// if/else, not if: a missing branch is a task that silently does
			// nothing somewhere in the world.
			name: "no else",
			body: "name: x\nroutes:\n  a: [ { host: h } ]\n  c: { if: p, then: a }\ntasks:\n  p: { cmd: [true] }\n  t: { route: c, cmd: [true] }\n",
			want: "no `else:`",
		},
		{
			name: "a branch that is not a route",
			body: "name: x\nroutes:\n  a: [ { host: h } ]\n  c: { if: p, then: a, else: nope }\ntasks:\n  p: { cmd: [true] }\n  t: { route: c, cmd: [true] }\n",
			want: "which is not one of",
		},
		{
			// A cycle would surface at dial time as a hang, which is a poor way to
			// learn about a typo.
			name: "a cycle",
			body: "name: x\nroutes:\n  a: [ { host: h } ]\n  c: { if: p, then: d, else: a }\n  d: { if: p, then: c, else: a }\ntasks:\n  p: { cmd: [true] }\n  t: { route: c, cmd: [true] }\n",
			want: "cycle:",
		},
		{
			// A typo here would otherwise be a route that silently always took
			// else:, which looks exactly like a predicate that works.
			name: "if names a task that does not exist",
			body: "name: x\nroutes:\n  a: [ { host: h } ]\n  c: { if: nope, then: a, else: a }\ntasks:\n  t: { route: c, cmd: [true] }\n",
			want: "not a task in this namespace",
		},
		{
			// A predicate answers a question about THIS machine. One that
			// travelled would pay a connection to answer it, and that reads as
			// slowness rather than as a mistake.
			name: "a predicate may not travel a route",
			body: "name: x\nroutes:\n  a: [ { host: h } ]\n  c: { if: p, then: a, else: a }\ntasks:\n  p: { route: a, cmd: [true] }\n  t: { route: c, cmd: [true] }\n",
			want: "must run on this machine",
		},
		{
			name: "with_route and route together",
			body: "name: x\nroutes:\n  a: [ { host: h } ]\ntasks:\n  t: { route: a, with_route: a, cmd: [true] }\n",
			want: "sets both `route:` and `with_route:`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, map[string]string{"x.yaml": tc.body}))
			if err == nil {
				t.Fatal("want an error at load time")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A dependency or a predicate may name another namespace's task, spelled as the
// full address — the same rule the command line uses, so the call site says
// where the thing came from without the reader knowing what is installed.
func TestACrossNamespaceReferenceResolves(t *testing.T) {
	dir := write(t, map[string]string{
		"ssh.yaml": "name: ssh\ntasks:\n  unlock: { cmd: [true] }\n",
		"lab.yaml": "name: lab\nroutes:\n  a: [ { host: h } ]\ntasks:\n  pods: { route: a, cmd: [true], deps: [global:ssh:unlock] }\n",
	})
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// Checked once every namespace is in, because one file cannot see another.
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

// A predicate in another namespace is checked the same way, including the rule
// that it may not travel a route.
func TestACrossNamespacePredicateIsChecked(t *testing.T) {
	dir := write(t, map[string]string{
		"ssh.yaml": "name: ssh\nroutes:\n  far: [ { host: h } ]\ntasks:\n  ask: { route: far, cmd: [true] }\n",
		"lab.yaml": "name: lab\nroutes:\n  a: [ { host: h } ]\n  c: { if: 'global:ssh:ask', then: a, else: a }\ntasks:\n  t: { route: c, cmd: [true] }\n",
	})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a predicate in another namespace that travels a route")
	}
	if !strings.Contains(err.Error(), "must run on this machine") {
		t.Errorf("error = %v", err)
	}
}

// The prefix rule cannot be undermined by a file defining a task whose own name
// begins with global: — it could never be reached, and deps: naming it would be
// ambiguous.
func TestATaskNamedGlobalIsRefused(t *testing.T) {
	dir := write(t, map[string]string{
		"x.yaml": "name: x\ntasks:\n  global:ssh:unlock: { cmd: [true] }\n",
	})
	if _, err := Load(dir); err == nil {
		t.Fatal("Load accepted a task whose name begins with global:")
	}
}

// One dependency written two ways is one dependency. From inside a namespace a
// task can be named either way — `once` or `global:x:once` — the way an
// absolute path to a file in the current directory is still that file. Keying
// the memo on the spelling would run it twice, which is exactly what the memo
// exists to prevent.
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

// The cycle this exists for crosses the boundary that used to separate two
// graphs: a route chooses by a task, that task depends on another, and that one
// travels the route. The route walk stopped at the task boundary and the
// dependency memo never looked at routes, so neither could see it.
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

// A predicate's deps: must run. This is the capability the design named as the
// deciding reason a predicate is a task rather than a block of its own — "a
// predicate that needs a vault unlocked before it can answer is an ordinary
// thing to want". Skipping them does not fail: the predicate answers wrongly,
// else: is taken, and the task travels the slow route in silence.
func TestAPredicateRunsItsDependencies(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "unlocked")
	dir := write(t, map[string]string{"x.yaml": fmt.Sprintf(`
name: x
routes:
  lan: [ { host: lan.example } ]
  pi:  [ { host: pi.example } ]
  home: { if: ready, then: lan, else: pi }
tasks:
  unlock: { cmd: 'touch %[1]s' }
  ready:  { deps: [unlock], cmd: 'test -f %[1]s' }
  t: { route: home, cmd: [true] }
`, marker)})
	set, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n := set.Namespaces["x"]
	r := &Runner{Out: io.Discard, Err: io.Discard}

	got, err := n.Resolve(context.Background(), "home", r.predicate(set, n, map[string]bool{}, map[string]bool{}))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "lan" {
		t.Errorf("route = %q, want lan — the predicate's dependency did not run, so it answered wrongly", got.Name)
	}
}

// A task that is both a dependency and a predicate runs ONCE. The memo that
// stops a dependency running twice and the one that remembers an answer are
// about the same task, and an unlock asked twice is a passphrase prompted
// twice.
func TestATaskUsedAsBothDependencyAndPredicateRunsOnce(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "count")
	dir := write(t, map[string]string{"x.yaml": fmt.Sprintf(`
name: x
routes:
  lan: [ { host: lan.example } ]
  pi:  [ { host: pi.example } ]
  home: { if: probe, then: lan, else: pi }
tasks:
  probe: { cmd: 'echo . >> %s' }
  t:
    with_route: home
    deps: [probe]
    cmd: [true]
`, marker)})
	set, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := &Runner{Out: io.Discard, Err: io.Discard}
	if err := r.Run(context.Background(), set, "x:t"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	if n := strings.Count(string(data), "."); n != 1 {
		t.Errorf("the task ran %d times as dependency and predicate, want 1", n)
	}
}

// An answer lasts one run and no longer. "Am I on the LAN" is true until it is
// not, and a Runner reused across two invocations must ask again.
func TestAPredicateAnswerDoesNotOutliveTheRun(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "count")
	dir := write(t, map[string]string{"x.yaml": fmt.Sprintf(`
name: x
routes:
  lan: [ { host: lan.example } ]
  pi:  [ { host: pi.example } ]
  home: { if: probe, then: lan, else: pi }
tasks:
  probe: { cmd: 'echo . >> %s' }
  t: { with_route: home, cmd: [true] }
`, marker)})
	set, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := &Runner{Out: io.Discard, Err: io.Discard}
	for i := 0; i < 2; i++ {
		if err := r.Run(context.Background(), set, "x:t"); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	if n := strings.Count(string(data), "."); n != 2 {
		t.Errorf("the predicate was asked %d times across two runs, want 2", n)
	}
}
