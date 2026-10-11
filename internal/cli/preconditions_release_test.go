package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Real git state reproduces the motivating release refusal: a dirty tree wins
// over the branch check, and a clean tree on another branch reaches that check.
func TestReleasePreconditionsExplainDirtyTreeAndWrongBranch(t *testing.T) {
	dir := writeTree(t, map[string]string{"chores.yml": `tasks:
  release:
    preconditions:
      - sh: git diff --quiet HEAD
        msg: 'Commit first: a release names a commit'
      - task: on-main
        msg: 'Release what is on main: check out main and pull'
    before: [touch prepared]
    cmds: [touch released]
  on-main:
    cmd: test "$(git symbolic-ref --short HEAD)" = main
`, "tracked.txt": "committed\n"})
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("-c", "user.name=Precondition test", "-c", "user.email=check@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "initial")
	git("checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runMain(t, dir, "release")
	checkCode(t, got, 1)
	if got.stdout != "" || got.stderr != "task: release: Commit first: a release names a commit\n" {
		t.Fatalf("dirty-tree refusal: %s", got)
	}
	git("restore", "tracked.txt")
	got = runMain(t, dir, "release")
	checkCode(t, got, 1)
	if got.stdout != "" || got.stderr != "task: release: Release what is on main: check out main and pull\n" {
		t.Fatalf("branch refusal: %s", got)
	}
	for _, marker := range []string{"prepared", "released"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); !os.IsNotExist(err) {
			t.Fatalf("refused release created %s: %v", marker, err)
		}
	}
	git("checkout", "-q", "main")
	got = runMain(t, dir, "release")
	checkCode(t, got, 0)
	for _, marker := range []string{"prepared", "released"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err != nil {
			t.Fatalf("allowed release did not create %s: %v", marker, err)
		}
	}
}
