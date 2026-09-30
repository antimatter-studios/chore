package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/global"
	"github.com/antimatter-studios/chore/internal/global/sshtest"
)

// routeTo is testRoute that also hands back the server, for a test that asks
// what arrived at the far end.
func routeTo(t *testing.T, shell bool) (string, *sshtest.Server) {
	t.Helper()
	sock, pub := sshtest.Agent(t)
	server := sshtest.NewServer(t, pub)
	server.Shell = shell
	old := dialerOverride
	dialerOverride = &global.Dialer{AgentSock: sock, KnownHosts: sshtest.KnownHosts(t, server)}
	t.Cleanup(func() { dialerOverride = old })
	hop := sshtest.Hop(t, server)
	return fmt.Sprintf("routes:\n  r: [ { host: %s, port: %d, user: %s } ]\n", hop.Host, hop.Port, hop.User), server
}

// The key in the file is what picks the terminal: a routed task with `pty: true`
// asks for one and the same task without it does not.
func TestARoutedTaskAsksForATerminalOnlyWhenItSaysSo(t *testing.T) {
	routes, server := routeTo(t, false)
	dir := writeTree(t, map[string]string{"chores.yml": routes + `
tasks:
  plain: { route: r, cmd: [hostname] }
  shell: { route: r, pty: true, cmd: [bash, -l] }
`})
	got := runMain(t, dir, "plain")
	checkCode(t, got, 0)
	if n := len(server.PTYRequests()); n != 0 {
		t.Fatalf("a task without `pty:` asked for %d terminal(s)", n)
	}
	got = runMain(t, dir, "shell")
	checkCode(t, got, 0)
	if n := len(server.PTYRequests()); n != 1 {
		t.Fatalf("a task with `pty: true` made %d terminal requests, want 1", n)
	}
}

// Every step of a routed task — and its `defer:` — runs at the far end, over one
// connection; a `- task:` step runs wherever the task it names runs.
func TestEveryStepOfARoutedTaskTravels(t *testing.T) {
	routes, server := routeTo(t, false)
	dir := writeTree(t, map[string]string{"chores.yml": routes + `
tasks:
  here: { cmd: echo here }
  there:
    route: r
    cmds:
      - echo one
      - defer: echo cleanup
      - task: here
      - [echo, two words]
`})
	got := runMain(t, dir, "there")
	checkCode(t, got, 0)
	var sent []string
	for _, c := range server.Commands() {
		sent = append(sent, c[strings.Index(c, "; ")+2:])
	}
	want := []string{"echo one", "'echo' 'two words'", "echo cleanup"}
	if strings.Join(sent, "|") != strings.Join(want, "|") {
		t.Errorf("the far end ran %q, want %q", sent, want)
	}
	checkContains(t, got, "stdout", got.stdout, "here")
}

// A remote failure carries its own status, as a local one does.
func TestARoutedFailureCarriesItsExitStatus(t *testing.T) {
	routes, _ := routeTo(t, true)
	dir := writeTree(t, map[string]string{"chores.yml": routes + "tasks:\n  t: { route: r, cmd: 'exit 7' }\n"})
	got := runMain(t, dir, "t")
	checkCode(t, got, 7)
}

// A predicate that must unlock something first runs its `deps:`, and a
// `run: once` dependency it shares with the task choosing the route runs once —
// an unlock asked twice is a passphrase prompted twice.
func TestAPredicateRunsItsDependenciesAndSharesRunOnce(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	installGlobal(t, map[string]string{"x.yaml": fmt.Sprintf(`
routes:
  a: [ { host: a.example } ]
  b: [ { host: b.example } ]
  pick: { if: check, then: a, else: b }
tasks:
  unlock: { run: once, cmd: 'echo unlock >> %[1]s' }
  check: { deps: [unlock], cmd: 'echo check >> %[1]s' }
  show:
    deps: [unlock]
    with_route: pick
    cmd: 'echo "route=$CHORE_ROUTE host=$CHORE_ROUTE_HOST"'
`, log)})
	got := runMain(t, t.TempDir(), "global:x:show")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "route=a host=a.example")
	data, _ := os.ReadFile(log)
	if got := strings.Fields(string(data)); strings.Join(got, " ") != "unlock check" {
		t.Errorf("ran %q, want unlock once then check once", got)
	}
}

// Two routes sharing a predicate ask it once per run.
func TestAPredicateIsAskedOncePerRun(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	installGlobal(t, map[string]string{"x.yaml": fmt.Sprintf(`
routes:
  a: [ { host: a.example } ]
  one: { if: check, then: a, else: a }
  two: { if: check, then: a, else: a }
tasks:
  check: { cmd: 'echo asked >> %s' }
  first: { with_route: one, cmd: 'true' }
  second: { with_route: two, cmd: 'true' }
  both: { cmds: [{task: first}, {task: second}] }
`, log)})
	got := runMain(t, t.TempDir(), "global:x:both")
	checkCode(t, got, 0)
	data, _ := os.ReadFile(log)
	if n := strings.Count(string(data), "asked"); n != 1 {
		t.Errorf("the predicate was asked %d times, want 1", n)
	}
}

// `exports:` hands what a task printed to everything after it — including a
// task whose variables were settled before that task ran.
func TestExportsReachLaterSteps(t *testing.T) {
	dir := writeTree(t, map[string]string{"chores.yml": `
tasks:
  unlock: { exports: true, cmd: 'echo "export UNLOCKED_SOCK=/tmp/agent.sock"; echo "not an assignment"' }
  use: { deps: [unlock], cmd: 'echo "sock=$UNLOCKED_SOCK"' }
`})
	t.Setenv("UNLOCKED_SOCK", "stale")
	got := runMain(t, dir, "use")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "sock=/tmp/agent.sock")
	checkNotContains(t, got, "stdout", got.stdout, "export UNLOCKED_SOCK", "not an assignment")
}

// Routes are a feature of every task, so a project's chores.yml declares and
// checks them the same way a global file does.
func TestAProjectRouteIsCheckedAtLoad(t *testing.T) {
	dir := writeTree(t, map[string]string{"chores.yml": "tasks:\n  t: { route: nowhere, cmd: echo }\n"})
	got := runMain(t, dir, "t")
	checkCode(t, got, 1)
	checkContains(t, got, "stderr", got.stderr, "nowhere", "declares no routes")
}

// A dependency cycle never terminates, so it is refused where it is written —
// for a project as for a global file.
func TestADependencyCycleIsRefusedAtLoad(t *testing.T) {
	dir := writeTree(t, map[string]string{"chores.yml": "tasks:\n  a: { deps: [b], cmd: echo a }\n  b: { deps: [a], cmd: echo b }\n  c: { cmd: echo c }\n"})
	got := runMain(t, dir, "c")
	checkCode(t, got, 1)
	checkContains(t, got, "stderr", got.stderr, "cycle", "task a", "task b")
}
