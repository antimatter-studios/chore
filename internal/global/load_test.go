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
version: '3'
tasks:
  k3s:pods:
    desc: pods everywhere
    cmds: [echo pods]
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
	task, ok := n.Task("k3s:pods")
	if !ok {
		t.Fatal("namespace is missing k3s:pods")
	}
	// A task name may contain colons of its own; the namespace is what precedes
	// the FIRST one, or `k3s:pods` would be unaddressable.
	if task.Name != "k3s:pods" || Address(n.Name, task.Name) != "global:homelab:k3s:pods" {
		t.Errorf("resolved %q / %q", task.Name, Address(n.Name, task.Name))
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

// Two files claiming one name means one of them is unreachable, and which one
// would depend on directory order.
func TestTwoNamespacesCannotShareAName(t *testing.T) {
	_, err := Load(write(t, map[string]string{
		"a.yaml": "name: same\nversion: '3'\ntasks:\n  t: { cmds: [true] }\n",
		"b.yaml": "name: same\nversion: '3'\ntasks:\n  t: { cmds: [true] }\n",
	}))
	if err == nil || !strings.Contains(err.Error(), "two global taskfiles are named") {
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
		"a.yaml": "name: a\nversion: '3'\ntasks:\n  t: { cmds: [true] }\n",
		"b.yml":  "name: b\nversion: '3'\ntasks:\n  t: { cmds: [true] }\n",
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
	_, taskName, _, err := set.Lookup("global:homelab:")
	if err != nil || taskName != "" {
		t.Fatalf("namespace lookup returned task %q and err %v, want listing request", taskName, err)
	}
}
