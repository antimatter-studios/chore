package global

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// write makes a global.d with the given files in it.
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

func mustLoad(t *testing.T, files map[string]string) *Set {
	t.Helper()
	set, err := Load(write(t, files))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return set
}

func loadErr(t *testing.T, files map[string]string, want ...string) {
	t.Helper()
	_, err := Load(write(t, files))
	if err == nil {
		t.Fatalf("Load succeeded; want an error containing %q", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not contain %q", err, w)
		}
	}
}

// A global file is an ordinary taskfile: its tasks are addressed through the
// fixed prefix and the filename, and nothing else about them changes.
func TestAGlobalFileIsAnOrdinaryTaskfile(t *testing.T) {
	set := mustLoad(t, map[string]string{"homelab.yaml": `
tasks:
  k3s:pods:
    args: [namespace]
    vars: { namespace: default }
    cmds: ['kubectl get pods -n {{.NAMESPACE}}']
`})
	p, err := set.Project("homelab")
	if err != nil {
		t.Fatal(err)
	}
	task, ok := p.Tasks["global:homelab:k3s:pods"]
	if !ok {
		t.Fatalf("tasks = %v", p.Tasks)
	}
	if len(task.Args) != 1 || task.Args[0].Name != "namespace" {
		t.Errorf("args = %v", task.Args)
	}
	if p.Root.Path != filepath.Join(set.Dir, "homelab.yaml") || !p.Root.Global {
		t.Errorf("root = %+v", p.Root)
	}
}

func TestAMissingDirectoryIsNotAnError(t *testing.T) {
	set, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(set.Namespaces) != 0 {
		t.Fatalf("Load = %v, %v", set, err)
	}
}

// A file that is present and wrong is an error naming it, not a namespace that
// silently stopped existing.
func TestABrokenFileIsReported(t *testing.T) {
	loadErr(t, map[string]string{"ok.yaml": "tasks: {a: {cmd: echo}}\n", "bad.yaml": "tasks:\n  a:\n    cmmd: echo\n"},
		"bad.yaml", "cmmd")
}

func TestBothYamlSpellingsLoadAndCannotShareANamespace(t *testing.T) {
	set := mustLoad(t, map[string]string{"a.yaml": "tasks: {x: {cmd: echo}}\n", "b.yml": "tasks: {x: {cmd: echo}}\n", ".hidden.yaml": "junk"})
	if len(set.Namespaces) != 2 {
		t.Errorf("namespaces = %v", set.Namespaces)
	}
	loadErr(t, map[string]string{"ssh.yaml": "tasks: {}\n", "ssh.yml": "tasks: {}\n"}, "global:ssh:", "ssh.yaml", "ssh.yml")
}

// `name:` is not needed, and a file that still says it must agree.
func TestNameMustMatchTheFilename(t *testing.T) {
	mustLoad(t, map[string]string{"agents.yaml": "name: agents\ntasks: {}\n"})
	loadErr(t, map[string]string{"agents.yaml": "name: robots\ntasks: {}\n"}, "disagrees with the filename")
}

// A task cannot be named with the prefix an address is written with: it could
// never be reached, and `deps: [global:x:y]` would have two readings.
func TestATaskNamedGlobalIsRefused(t *testing.T) {
	loadErr(t, map[string]string{"x.yaml": "tasks:\n  'global:y:z': {cmd: echo}\n"}, "`global:` prefix")
}

// One global file may name another's task or route; a name that finds nothing
// is refused at load, not when somebody runs it.
func TestACrossFileReferenceIsChecked(t *testing.T) {
	mustLoad(t, map[string]string{
		"ssh.yaml":     "tasks:\n  unlock: {cmd: echo}\n",
		"homelab.yaml": "routes:\n  pi: [{host: h}]\ntasks:\n  pods: {deps: [global:ssh:unlock], route: pi, cmd: echo}\n",
	})
	loadErr(t, map[string]string{
		"ssh.yaml":     "tasks:\n  unlock: {cmd: echo}\n",
		"homelab.yaml": "tasks:\n  pods: {deps: [global:ssh:unlcok], cmd: echo}\n",
	}, "global:ssh:unlcok", "global:ssh:unlock")
	loadErr(t, map[string]string{
		"homelab.yaml": "tasks:\n  pods: {route: 'global:net:pi', cmd: echo}\n",
	}, "global:net:pi", "not a route")
}

// A route's predicate is a task, checked like one, and one with a `route:` of
// its own is refused: it would pay a connection to answer a question about here.
func TestConditionalRoutesAreCheckedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"an unknown predicate", "routes:\n  a: [{host: a}]\n  c: {if: nope, then: a, else: a}\n", []string{"nope", "not a task"}},
		{"a missing else", "routes:\n  a: [{host: a}]\n  c: {if: p, then: a}\ntasks:\n  p: {cmd: 'true'}\n", []string{"no `else:`"}},
		{"an unknown branch", "routes:\n  a: [{host: a}]\n  c: {if: p, then: a, else: b}\ntasks:\n  p: {cmd: 'true'}\n", []string{"`else:`", "\"b\""}},
		{"a routed predicate", "routes:\n  a: [{host: a}]\n  c: {if: p, then: a, else: a}\ntasks:\n  p: {route: a, cmd: 'true'}\n", []string{"must run on this machine"}},
		{"a typo in a selector", "routes:\n  a: [{host: a}]\n  c: {if: p, then: a, esle: a}\ntasks:\n  p: {cmd: 'true'}\n", []string{"esle"}},
		{"a typo in a hop", "routes:\n  a: [{host: a, prot: 22}]\n", []string{"prot"}},
		{"a route with no hops", "routes:\n  a: []\n", []string{"no hops"}},
	} {
		t.Run(tc.name, func(t *testing.T) { loadErr(t, map[string]string{"x.yaml": tc.body}, tc.want...) })
	}
}

// Routes and tasks are one graph: `route:` leads from a task to a route, and a
// selector's `if:` from a route back to a task. A cycle through both, across two
// files, is refused at load.
func TestACycleThroughARouteAndATaskIsRefused(t *testing.T) {
	loadErr(t, map[string]string{
		"a.yaml": "routes:\n  lan: [{host: l}]\n  home: {if: global:b:check, then: lan, else: lan}\ntasks:\n  pods: {route: home, cmd: echo}\n",
		"b.yaml": "tasks:\n  check: {deps: [global:a:pods], cmd: 'true'}\n",
	}, "cycle")
}

func TestDirHonoursXDGAndFallsBack(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if d, _ := Dir(); d != filepath.Join("/xdg", "chore", "global.d") {
		t.Errorf("Dir with XDG = %s", d)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/me")
	if d, _ := Dir(); d != filepath.Join("/home/me", ".config", "chore", "global.d") {
		t.Errorf("Dir without XDG = %s", d)
	}
}

// Hops get what ssh would give them, and a `$VAR` in one is expanded from the
// task's variables — strictly, since a hop to "" is a hop to nowhere.
func TestPrepareHopsFillsDefaultsAndExpands(t *testing.T) {
	env := map[string]string{"HOMELAB": "h.example.com"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	hops, err := PrepareHops("pi", []chorefile.Hop{{Host: "$HOMELAB"}}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if hops[0].Host != "h.example.com" || hops[0].Port != 22 || hops[0].User == "" {
		t.Errorf("hop = %+v", hops[0])
	}
	if _, err := PrepareHops("pi", []chorefile.Hop{{Host: "$NOPE"}}, lookup); err == nil || !strings.Contains(err.Error(), "$NOPE is not set") {
		t.Errorf("err = %v", err)
	}
}

func TestExpand(t *testing.T) {
	env := map[string]string{"A": "1", "HOME": "/h"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	for in, want := range map[string]string{
		"$HOME/v":   "/h/v",
		"${A}x":     "1x",
		"$$HOME":    "$HOME",
		"cost $":    "cost $",
		"no vars":   "no vars",
		"$A$A-${A}": "11-1",
	} {
		if got, err := Expand("t", in, lookup); err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
