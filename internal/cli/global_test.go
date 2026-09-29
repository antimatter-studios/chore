package cli

import (
	"fmt"
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
routes:
  pi:
    - { host: s1.example.com, port: 10022, user: root }
    - { host: 127.0.0.1, port: 2222, user: chris }
tasks:
  k3s:pods:
    desc: pods across every namespace
    route: pi
    cmd: [kubectl, get, pods, -A]
  k3s:proxy:
    route: pi
    forward: { remote: 127.0.0.1:6443, local: 127.0.0.1:6443 }
  _helper:
    internal: true
    route: pi
    cmd: [true]
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

// --dry answers the question a route makes hard to check by eye: which machine
// is each hop dialled FROM. It touches no network.
func TestGlobalDryRunPrintsTheRouteWithoutDialling(t *testing.T) {
	installGlobal(t, map[string]string{"homelab.yaml": homelabNamespace})

	got := runMain(t, t.TempDir(), "--dry", "global:homelab:k3s:pods")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout,
		"hop 1", "root@s1.example.com:10022",
		"hop 2", "chris@127.0.0.1:2222",
		"dialled from hop 1",
		"'kubectl' 'get' 'pods' '-A'",
	)
}

func TestGlobalDryRunPrintsAForward(t *testing.T) {
	installGlobal(t, map[string]string{"homelab.yaml": homelabNamespace})
	got := runMain(t, t.TempDir(), "--dry", "global:homelab:k3s:proxy")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "127.0.0.1:6443 on this machine", "at the far end")
}

// A PTY changes how the remote command's input and output behave, so --dry says
// when one will be allocated — and says nothing for a task that has none.
func TestGlobalDryRunSaysWhenATerminalIsAllocated(t *testing.T) {
	installGlobal(t, map[string]string{"x.yaml": `
name: x
routes:
  pi: [ { host: pi.example, user: chris } ]
tasks:
  shell: { route: pi, pty: true, cmd: [bash, -l] }
  pods: { route: pi, cmd: [kubectl, get, pods] }
`})
	got := runMain(t, t.TempDir(), "--dry", "global:x:shell")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "'bash' '-l'", "pty:     allocated for interactive input")

	got = runMain(t, t.TempDir(), "--dry", "global:x:pods")
	checkCode(t, got, 0)
	checkNotContains(t, got, "stdout", got.stdout, "pty:")
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
	checkContains(t, got, "stderr", got.stderr, "no task", "it has: k3s:pods, k3s:proxy")
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
		"good.yaml":   "name: good\nroutes:\n  r: [ { host: h } ]\ntasks:\n  t: { route: r, cmd: [true] }\n",
		"broken.yaml": "name: broken\ntasks:\n  t: { route: nope, cmd: [true] }\n",
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

// A predicate that exits non-zero has ANSWERED. It is not a failed run: nothing
// is reported, the status is not propagated, and the else: branch is taken.
// That inversion is the whole reason a predicate is a path of its own rather
// than an ordinary dependency, and nothing else asserts it.
//
// Run for real rather than under --dry, which would assert nothing: --dry runs
// no predicate at all, so every claim here would hold vacuously. `with_route:`
// is what makes a real run possible without a network — the route is resolved
// and handed over as environment rather than travelled.
func TestAFailedPredicateIsNotAFailedRun(t *testing.T) {
	installGlobal(t, map[string]string{"x.yaml": `
name: x
routes:
  a: [ { host: a.example } ]
  b: [ { host: b.example } ]
  pick: { if: never, then: a, else: b }
tasks:
  never:
    desc: a question whose answer is no
    cmd: 'exit 3'
  show:
    with_route: pick
    # The shell form on purpose: an argv is expanded HERE, at load, where
    # CHORE_ROUTE does not exist yet. The string form is handed to a shell
    # that expands it once the route has been resolved.
    cmd: 'echo landed on $CHORE_ROUTE'
`})
	got := runMain(t, t.TempDir(), "global:x:show")
	checkCode(t, got, 0)
	checkNotContains(t, got, "stderr", got.stderr, "exit status 3")
	// The answer was taken, not merely survived: exit 3 means `else:`.
	checkContains(t, got, "stdout", got.stdout, "landed on b")
}

// Listing is a question about what is installed, so it must not run anything.
// A predicate is an ordinary task and may do anything a task can.
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
		t.Errorf("listing ran the predicate:\n%s", got)
	}
}

// --dry runs NOTHING, predicates included. A predicate is an ordinary task and
// may modify data, so "nothing happens except the parts chore judged safe" is
// not a promise --dry can make. It prints both branches instead, which shows
// more of the file than resolving did; to learn which branch you are on, run
// the predicate, which has a name for exactly that reason.
func TestDryRunPrintsBothBranchesAndRunsNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	installGlobal(t, map[string]string{"x.yaml": fmt.Sprintf(`
name: x
routes:
  lan:  [ { host: 192.168.0.47, user: chris } ]
  pi:
    - { host: s1.example.com, port: 10022, user: root }
    - { host: 127.0.0.1, port: 2222, user: chris }
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
		t.Fatalf("--dry ran the predicate:\n%s", got)
	}
	checkContains(t, got, "stdout", got.stdout,
		"if on-lan", "then -> lan", "else -> pi",
		"192.168.0.47", "s1.example.com", "dialled from hop 1",
	)
}

// --dry must be BOUNDED, not merely terminating. A cycle is refused at load, so
// the walk ends — but a chain of selectors whose branches converge doubles the
// output per link, and eighteen of them printed 137MB. A route already seen is
// named rather than expanded again.
func TestDryRunDoesNotDoubleItsOutputPerSelector(t *testing.T) {
	var b strings.Builder
	b.WriteString("name: x\nroutes:\n  leaf: [ { host: leaf.example } ]\n")
	for i := 0; i < 18; i++ {
		next := "leaf"
		if i < 17 {
			next = fmt.Sprintf("r%d", i+1)
		}
		fmt.Fprintf(&b, "  r%d: { if: p, then: %s, else: %s }\n", i, next, next)
	}
	b.WriteString("tasks:\n  p: { cmd: [true] }\n  t: { route: r0, cmd: [true] }\n")
	installGlobal(t, map[string]string{"x.yaml": b.String()})

	got := runMain(t, t.TempDir(), "--dry", "global:x:t")
	checkCode(t, got, 0)
	if len(got.stdout) > 8000 {
		t.Errorf("--dry printed %d bytes for 18 chained selectors", len(got.stdout))
	}
}
