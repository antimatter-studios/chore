package cli

import (
	"os"
	"path/filepath"
	"testing"
)

const confirmTree = `version: '3'
tasks:
  deploy:
    confirm: 'Deploy to production?'
    cmds: ['echo deployed > deployed.txt']
`

// Through the command line, with stdin whatever `go test` gives it: a task that
// asks and cannot be answered stops the run and says what it was asking.
func TestConfirmWithoutATerminalRefuses(t *testing.T) {
	dir := writeTree(t, map[string]string{"chores.yml": confirmTree})

	got := runMain(t, dir, "deploy")

	checkCode(t, got, 1)
	checkContains(t, got, "stderr", got.stderr, "Deploy to production?")
	checkContains(t, got, "stderr", got.stderr, "--yes")
	if exists(t, dir, "deployed.txt") {
		t.Error("an unanswered task deployed")
	}
}

func TestConfirmYesFlagAndEnvironment(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		env  string
	}{
		{"--yes", []string{"--yes", "deploy"}, ""},
		{"-y", []string{"-y", "deploy"}, ""},
		{"CHORE_YES", []string{"deploy"}, "1"},
		{"CHORE_YES=true", []string{"deploy"}, "true"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{"chores.yml": confirmTree})
			if c.env != "" {
				t.Setenv("CHORE_YES", c.env)
			}

			got := runMain(t, dir, c.args...)

			checkCode(t, got, 0)
			if !exists(t, dir, "deployed.txt") {
				t.Errorf("did not run: %s", got)
			}
		})
	}
}

// An environment variable that is explicitly off is off, so `CHORE_YES=0` in a
// profile does not silently arm every confirmation on the machine.
func TestConfirmEnvironmentCanSayNo(t *testing.T) {
	for _, value := range []string{"0", "no", "false", ""} {
		dir := writeTree(t, map[string]string{"chores.yml": confirmTree})
		t.Setenv("CHORE_YES", value)

		got := runMain(t, dir, "deploy")

		checkCode(t, got, 1)
		if exists(t, dir, "deployed.txt") {
			t.Errorf("CHORE_YES=%q answered the question", value)
		}
	}
}

// --dry never asks, so a dry run is safe to pipe anywhere.
func TestConfirmDryRunNeverAsks(t *testing.T) {
	dir := writeTree(t, map[string]string{"chores.yml": confirmTree})

	got := runMain(t, dir, "--dry", "deploy")

	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "confirm: Deploy to production?")
	if exists(t, dir, "deployed.txt") {
		t.Error("--dry deployed")
	}
}

// The flag is listed where someone looking for it would look.
func TestConfirmFlagIsInTheUsage(t *testing.T) {
	got := runMain(t, writeTree(t, map[string]string{"chores.yml": confirmTree}), "--help")
	checkContains(t, got, "stdout", got.stdout, "--yes")
	checkContains(t, got, "stdout", got.stdout, "CHORE_YES")
}

// exists reports whether the task left its marker file. The tasks here write a
// file rather than echoing: the question is whether the body RAN, and a refusal
// that prints the question would otherwise be hard to tell from a task that
// printed and then did nothing.
func exists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}
