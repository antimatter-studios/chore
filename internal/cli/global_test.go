package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installGlobal writes namespace files into a temp global.d and points the CLI
// at it for the duration of one test.
func installGlobal(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := globalDirOverride
	globalDirOverride = dir
	t.Cleanup(func() { globalDirOverride = old })
	return dir
}

const homelabNamespace = `
name: homelab
version: '3'
tasks:
  k3s:pods:
    desc: pods across every namespace
    cmds: [echo pods]
  _helper:
    internal: true
    cmds: [echo helper]
`

// The point of a global task: it answers from a directory with no chores.yml in
// it or above it. Demanding a project file would defeat the feature in exactly
// the case it exists for — "check the cluster from wherever I am standing".
func TestGlobalWorksWithNoTaskfileAnywhere(t *testing.T) {
	installGlobal(t, map[string]string{"homelab.yaml": homelabNamespace})
	empty := t.TempDir()

	got := runMain(t, empty, "global:")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "global:homelab:")

	got = runMain(t, empty, "global:homelab:")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "k3s:pods", "pods across every namespace")

	// `internal: true` means the same here as in a project: not part of the
	// surface a person types at.
	if strings.Contains(got.stdout, "_helper") {
		t.Errorf("an internal task was listed:\n%s", got)
	}
}

// Global tasks use the ordinary chore task execution model.
func TestGlobalDryRunUsesTheOrdinaryTaskRunner(t *testing.T) {
	installGlobal(t, map[string]string{"homelab.yaml": homelabNamespace})

	got := runMain(t, t.TempDir(), "--dry", "global:homelab:k3s:pods")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout,
		"echo pods",
	)
}

// A namespace or task that is not there says so, and says what IS there —
// because the answer is almost always a typo.
func TestGlobalNamesWhatIsInstalled(t *testing.T) {
	installGlobal(t, map[string]string{"homelab.yaml": homelabNamespace})

	got := runMain(t, t.TempDir(), "global:hoemlab:k3s:pods")
	checkCode(t, got, 1)
	checkContains(t, got, "stderr", got.stderr, "no global namespace", "installed: homelab")

	got = runMain(t, t.TempDir(), "global:homelab:k3s:nodes")
	checkCode(t, got, 1)
	checkContains(t, got, "stderr", got.stderr, "no task", "it has: k3s:pods")
}

// An internal task is refused from the command line, exactly as a project's is.
func TestGlobalInternalTaskIsRefused(t *testing.T) {
	installGlobal(t, map[string]string{"homelab.yaml": homelabNamespace})
	got := runMain(t, t.TempDir(), "global:homelab:_helper")
	checkCode(t, got, 1)
	checkContains(t, got, "stderr", got.stderr, "is internal")
}

// A namespace file that is present and wrong is an error rather than a skip: a
// namespace that quietly failed to load is a command that stopped existing
// without saying so.
func TestABrokenNamespaceIsReportedNotSkipped(t *testing.T) {
	installGlobal(t, map[string]string{
		"good.yaml":   "name: good\nversion: '3'\ntasks:\n  t: { cmds: [true] }\n",
		"broken.yaml": "name: broken\nversion: '3'\ntasks:\n  t: { typo: true }\n",
	})
	got := runMain(t, t.TempDir(), "global:")
	checkCode(t, got, 1)
	checkContains(t, got, "stderr", got.stderr, "broken.yaml")
}

// The prefix is mandatory, so a project cannot define a task that shadows the
// global surface — and one that tries is refused where it is written rather than
// becoming a task that exists and can never run.
func TestAProjectCannotDefineAGlobalTask(t *testing.T) {
	root := writeTree(t, map[string]string{
		"chores.yml": "version: '3'\ntasks:\n  global:homelab:\n    cmds: [true]\n",
	})
	got := runMain(t, root, "--list")
	checkCode(t, got, 1)
	checkContains(t, got, "stderr", got.stderr, "reserved for machine-wide tasks")
}

// With nothing installed, `chore global:` says where it looked. A silent empty
// listing would leave the reader unable to tell "no namespaces" from "wrong
// directory".
func TestGlobalWithNothingInstalledSaysWhereItLooked(t *testing.T) {
	dir := installGlobal(t, nil)
	got := runMain(t, t.TempDir(), "global:")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, dir)
}
