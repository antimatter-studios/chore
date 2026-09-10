package global

import (
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
    exec: [kubectl, get, pods, -A]
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
	if len(n.Routes["pi"]) != 2 {
		t.Errorf("route pi has %d hops, want 2", len(n.Routes["pi"]))
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
  t: { route: r, exec: [true] }
`}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hop := set.Namespaces["x"].Routes["r"][0]
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
			body: "routes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, exec: [true] }\n",
			want: "needs a `name:`",
		},
		{
			name: "a colon in the name",
			body: "name: a:b\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, exec: [true] }\n",
			want: "cannot contain a colon",
		},
		{
			name: "a route with no hops",
			body: "name: x\nroutes:\n  r: []\ntasks:\n  t: { route: r, exec: [true] }\n",
			want: "has no hops",
		},
		{
			name: "a hop with no host",
			body: "name: x\nroutes:\n  r: [ { port: 22 } ]\ntasks:\n  t: { route: r, exec: [true] }\n",
			want: "needs a `host:`",
		},
		{
			name: "no route named",
			body: "name: x\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { exec: [true] }\n",
			want: "needs a `route:`",
		},
		{
			// Named a route that does not exist: the message lists the ones that do,
			// because the answer is almost always a typo.
			name: "an unknown route",
			body: "name: x\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: nope, exec: [true] }\n",
			want: `which is not one of: r`,
		},
		{
			name: "both exec and forward",
			body: "name: x\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, exec: [true], forward: { remote: a:1, local: b:2 } }\n",
			want: "sets both `exec:` and `forward:`",
		},
		{
			name: "neither exec nor forward",
			body: "name: x\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r }\n",
			want: "needs `exec:`",
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
			body: "name: x\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, exce: [true] }\n",
			want: "field exce not found",
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

// Two files claiming one name means one of them is unreachable, and which one
// would depend on directory order.
func TestTwoNamespacesCannotShareAName(t *testing.T) {
	_, err := Load(write(t, map[string]string{
		"a.yaml": "name: same\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, exec: [true] }\n",
		"b.yaml": "name: same\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, exec: [true] }\n",
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
		"a.yaml": "name: a\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, exec: [true] }\n",
		"b.yml":  "name: b\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, exec: [true] }\n",
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
