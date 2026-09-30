package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/global"
	"github.com/antimatter-studios/chore/internal/global/sshtest"
)

// #68: a global task is an ordinary task that lives somewhere else. Everything
// in this file runs the SAME task body from a project's chores.yml, from
// global.d, and routed over ssh, and asserts the three behave identically —
// which is the only way to keep them from drifting apart again.

// probeTask is the body under test. Every way a value can reach a script is
// printed: the template, the environment, a bool flag, and `--` passthrough.
const probeTask = `
    desc: probe
    args:
      - account
      - { name: json, type: bool }
    vars: { account: all }
    cmd: 'echo "account=[{{.ACCOUNT}}] env=[$ACCOUNT] json=[{{.JSON}}] cli=[${CLI_ARGS-unset}]"'
`

// testRoute starts an ssh server that really runs what it is sent, points the
// command line's dialler at it, and returns a `routes:` block reaching it.
func testRoute(t *testing.T) string {
	t.Helper()
	sock, pub := sshtest.Agent(t)
	server := sshtest.NewServer(t, pub)
	server.Shell = true
	old := dialerOverride
	dialerOverride = &global.Dialer{AgentSock: sock, KnownHosts: sshtest.KnownHosts(t, server)}
	t.Cleanup(func() { dialerOverride = old })
	hop := sshtest.Hop(t, server)
	return fmt.Sprintf("routes:\n  r: [ { host: %s, port: %d, user: %s } ]\n", hop.Host, hop.Port, hop.User)
}

// where builds one way of reaching the probe, returning the directory to run
// from and the name to type.
type where struct {
	name  string
	setup func(t *testing.T) (dir, task string)
}

func probeLocations() []where {
	return []where{
		{"project", func(t *testing.T) (string, string) {
			installGlobal(t, nil)
			return writeTree(t, map[string]string{"chores.yml": "tasks:\n  t:" + probeTask}), "t"
		}},
		{"global", func(t *testing.T) (string, string) {
			installGlobal(t, map[string]string{"zzprobe.yaml": "tasks:\n  t:" + probeTask})
			return t.TempDir(), "global:zzprobe:t"
		}},
		{"global, routed", func(t *testing.T) (string, string) {
			routes := testRoute(t)
			installGlobal(t, map[string]string{"zzprobe.yaml": routes + "tasks:\n  t:" + probeTask + "    route: r\n"})
			return t.TempDir(), "global:zzprobe:t"
		}},
		{"project, routed", func(t *testing.T) (string, string) {
			installGlobal(t, nil)
			routes := testRoute(t)
			return writeTree(t, map[string]string{"chores.yml": routes + "tasks:\n  t:" + probeTask + "    route: r\n"}), "t"
		}},
	}
}

func TestATaskBindsArgumentsTheSameWayWhereverItLives(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"the vars default", nil, "account=[all] env=[all] json=[] cli=[]"},
		{"positional", []string{"work"}, "account=[work] env=[work] json=[] cli=[]"},
		{"NAME=value", []string{"ACCOUNT=work"}, "account=[work] env=[work] json=[] cli=[]"},
		{"a flag", []string{"--account", "work"}, "account=[work] env=[work] json=[] cli=[]"},
		{"a bool flag", []string{"--json"}, "account=[all] env=[all] json=[true] cli=[]"},
		{"everything", []string{"work", "--json", "--", "a", "b"}, "account=[work] env=[work] json=[true] cli=[a b]"},
	} {
		for _, loc := range probeLocations() {
			t.Run(tc.name+"/"+loc.name, func(t *testing.T) {
				dir, task := loc.setup(t)
				got := runMain(t, dir, append([]string{task}, tc.args...)...)
				checkCode(t, got, 0)
				if line := strings.TrimSpace(got.stdout); line != tc.want {
					t.Errorf("got  %s\nwant %s\n%v", line, tc.want, got)
				}
			})
		}
	}
}

// The bug that opened #68: a word after a global task's name was dropped
// without a sound, and the task ran as if it had not been given. A word the
// task does not declare is an error, wherever the task lives.
func TestAnUndeclaredWordIsRefusedWhereverTheTaskLives(t *testing.T) {
	for _, loc := range probeLocations() {
		t.Run(loc.name, func(t *testing.T) {
			dir, task := loc.setup(t)
			for _, extra := range [][]string{
				{"work", "extra"},
				{"--nonsense"},
			} {
				got := runMain(t, dir, append([]string{task}, extra...)...)
				if got.code == 0 {
					t.Errorf("%s %s exited 0; an undeclared word must be refused\n%v", task, strings.Join(extra, " "), got)
				}
				if strings.Contains(got.stdout, "account=") {
					t.Errorf("the task ran despite the refused word\n%v", got)
				}
			}
		})
	}
}

// --help describes a global task and runs nothing — before #68 it was dropped
// with every other word, so asking a global task for help RAN it.
func TestHelpDescribesAGlobalTaskAndRunsNothing(t *testing.T) {
	installGlobal(t, map[string]string{"zzprobe.yaml": "tasks:\n  t:" + probeTask})
	for _, args := range [][]string{{"global:zzprobe:t", "--help"}, {"--help", "global:zzprobe:t"}} {
		got := runMain(t, t.TempDir(), args...)
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "chore global:zzprobe:t", "account", "json")
		checkNotContains(t, got, "stdout", got.stdout, "account=[")
	}
}

// Every global file is loaded on every run, so a project's task can depend on
// a machine's task and travel a machine's route by the same `global:` address a
// person types.
func TestAProjectTaskCanUseAGlobalTaskAndRoute(t *testing.T) {
	routes := testRoute(t)
	installGlobal(t, map[string]string{
		"ssh.yaml":     "tasks:\n  unlock:\n    cmd: echo unlocked\n",
		"homelab.yaml": routes,
	})
	dir := writeTree(t, map[string]string{"chores.yml": `
tasks:
  deploy:
    args: [env]
    deps: [global:ssh:unlock]
    route: global:homelab:r
    cmd: echo "deploying $ENV at the far end"
`})
	got := runMain(t, dir, "deploy", "prod")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "unlocked", "deploying prod at the far end")
}

// A project may not reach a global task that does not exist: the reference is
// checked when the files load, not when somebody runs the task.
func TestAProjectReferenceToAMissingGlobalTaskIsRefused(t *testing.T) {
	installGlobal(t, map[string]string{"ssh.yaml": "tasks:\n  unlock:\n    cmd: echo unlocked\n"})
	dir := writeTree(t, map[string]string{"chores.yml": "tasks:\n  a:\n    deps: [global:ssh:unlcok]\n    cmd: echo a\n  b:\n    cmd: echo b\n"})
	got := runMain(t, dir, "b")
	checkCode(t, got, 1)
	checkContains(t, got, "stderr", got.stderr, "global:ssh:unlcok", "global:ssh:unlock")
}

// `chore --list` is the project's own surface. Global tasks are attached to the
// run but listed by `chore global:`.
func TestListingAProjectLeavesGlobalTasksOut(t *testing.T) {
	installGlobal(t, map[string]string{"ssh.yaml": "tasks:\n  unlock:\n    desc: unlock the key\n    cmd: echo unlocked\n"})
	dir := writeTree(t, map[string]string{"chores.yml": "tasks:\n  build:\n    desc: build it\n    cmd: echo b\n"})
	got := runMain(t, dir, "--list")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "build")
	checkNotContains(t, got, "stdout", got.stdout, "unlock")
}

// The namespace is the filename. `name:` is no longer needed, and a file that
// still says it has to agree.
func TestTheNamespaceIsTheFilename(t *testing.T) {
	installGlobal(t, map[string]string{"agents.yaml": "name: agents\ntasks:\n  a:\n    cmd: echo a\n"})
	got := runMain(t, t.TempDir(), "global:agents:a")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "a")

	installGlobal(t, map[string]string{"agents.yaml": "name: robots\ntasks:\n  a:\n    cmd: echo a\n"})
	got = runMain(t, t.TempDir(), "global:agents:a")
	checkCode(t, got, 1)
	checkContains(t, got, "stderr", got.stderr, "name: robots", "disagrees with the filename")
}

// A global task runs where chore was started — it belongs to the machine — and
// {{.TASKFILE_DIR}} is still where its file is.
func TestAGlobalTaskRunsInTheCurrentDirectory(t *testing.T) {
	gdir := installGlobal(t, map[string]string{"tools.yaml": "tasks:\n  where:\n    cmd: 'pwd; echo file={{.TASKFILE_DIR}}'\n"})
	here := t.TempDir()
	got := runMain(t, here, "global:tools:where")
	checkCode(t, got, 0)
	lines := strings.Split(strings.TrimSpace(got.stdout), "\n")
	if len(lines) != 2 || !samePath(t, lines[0], here) {
		t.Errorf("ran in %q, want %q\n%v", lines[0], here, got)
	}
	if len(lines) == 2 && !samePath(t, strings.TrimPrefix(lines[1], "file="), gdir) {
		t.Errorf("TASKFILE_DIR = %q, want %q", lines[1], gdir)
	}
}

// A project's environment does not leak into a global task it depends on: a
// global file is the root of its own tree.
func TestAProjectDotenvDoesNotReachAGlobalDependency(t *testing.T) {
	installGlobal(t, map[string]string{"ssh.yaml": "tasks:\n  unlock:\n    cmd: echo \"secret=[${PROJECT_SECRET-unset}]\"\n"})
	dir := writeTree(t, map[string]string{
		"chores.yml": "dotenv: [.env]\ntasks:\n  a:\n    deps: [global:ssh:unlock]\n    cmd: echo \"a=[$PROJECT_SECRET]\"\n",
		".env":       "PROJECT_SECRET=hunter2\n",
	})
	got := runMain(t, dir, "a")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "secret=[unset]", "a=[hunter2]")
}
