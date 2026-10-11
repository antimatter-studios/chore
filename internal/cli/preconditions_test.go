package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoTaskPreconditionsGiveOnlyTheirMessage(t *testing.T) {
	dir := writeTree(t, map[string]string{"chores.yml": `version: '3'
tasks:
  release:
    preconditions:
      - sh: echo hidden; echo hidden >&2; exit 7
        msg: 'Commit first: a release names a commit'
    before: [echo before]
    deps: [dep]
    cmds: [echo body]
    on_failure: [echo failure]
    after: [echo after]
  dep:
    cmd: echo dependency
`})
	for _, flags := range [][]string{nil, {"--force"}, {"--no-lifecycle"}} {
		got := runMain(t, dir, append(flags, "release")...)
		checkCode(t, got, 7)
		if got.stdout != "" || got.stderr != "task: release: Commit first: a release names a commit\n" {
			t.Fatalf("refusal output: %s", got)
		}
	}
	got := runMain(t, dir, "-v", "release")
	checkCode(t, got, 7)
	checkContains(t, got, "stdout", got.stdout, "echo hidden; echo hidden >&2; exit 7")
	checkContains(t, got, "stdout", got.stdout, "hidden")
	checkContains(t, got, "stderr", got.stderr, "hidden")
	checkNotContains(t, got, "stderr", got.stderr, "failing step")
}

func TestPreconditionReferencesResolveInIncludedAndGlobalFiles(t *testing.T) {
	installGlobal(t, map[string]string{"checks.yaml": `tasks:
  ready:
    cmd: test "$VALUE" = yes
`})
	dir := writeTree(t, map[string]string{
		"chores.yml": `includes:
  child: {taskfile: child.yml}
tasks:
  root-check:
    cmd: echo root >> checks.txt
`,
		"child.yml": `tasks:
  work:
    preconditions:
      - task: local-check
      - task: ':root-check'
      - task: global:checks:ready
        vars: {VALUE: yes}
    before: [echo before]
    cmds: [echo body]
  local-check:
    cmd: echo local >> checks.txt
`,
	})
	got := runMain(t, dir, "child:work")
	checkCode(t, got, 0)
	if got.stdout != "before\nbody\n" || got.stderr != "" {
		t.Fatalf("check output leaked or before changed: %s", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "checks.txt"))
	if err != nil || string(data) != "local\nroot\n" {
		t.Fatalf("references not run in order: %q %v", data, err)
	}
}

func TestRefusedDependenciesAndTaskStepsFailTheirCaller(t *testing.T) {
	dir := writeTree(t, map[string]string{"chores.yml": `tasks:
  dep-parent:
    deps: [refused]
    cmd: echo should-not-run
  step-parent:
    cmds:
      - task: refused
      - echo should-not-run
  refused:
    preconditions:
      - task: failing-check
        msg: Release what is on main
  failing-check:
    cmd: echo hidden; exit 6
`})
	for _, task := range []string{"dep-parent", "step-parent"} {
		got := runMain(t, dir, task)
		checkCode(t, got, 6)
		if got.stdout != "" || !strings.Contains(got.stderr, "Release what is on main") || strings.Contains(got.stderr, "exit status") {
			t.Fatalf("refused caller: %s", got)
		}
	}
}

func TestDryPreconditionsListWithoutResolvingTaskVariables(t *testing.T) {
	dir := writeTree(t, map[string]string{"chores.yml": `tasks:
  work:
    preconditions:
      - task: check
        vars:
          VALUE: {sh: touch vars-ran; echo yes}
      - sh: touch shell-ran
    cmd: echo body
  check:
    cmd: touch task-ran
`})
	got := runMain(t, dir, "--dry", "work")
	checkCode(t, got, 0)
	if got.stdout != "check: task check\ncheck: touch shell-ran\necho body\n" {
		t.Fatalf("dry output: %s", got)
	}
	for _, name := range []string{"vars-ran", "shell-ran", "task-ran"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("dry run created %s: %v", name, err)
		}
	}
}
