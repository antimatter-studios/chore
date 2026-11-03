package run

import (
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// A file's `json_file:` variable reaches a task three ways: as a tree in its
// command's template, as compact JSON in its environment, and to `for:` as the
// file's top-level keys in written order.
func TestFileVariableReachesTheTask(t *testing.T) {
	f := newFixture(t, &chorefile.File{Vars: map[string]chorefile.Var{
		"PROFILES": {File: "{{.TASKFILE_DIR}}/profiles.json", FileFormat: "json"},
	}}, map[string]*chorefile.Task{
		"show": {Cmds: steps(
			`echo "dir={{.PROFILES.work.directory}}"`,
			`printf '%s\n' "$PROFILES" > env.txt`,
		)},
		"each": {Cmds: chorefile.Cmds{{For: &chorefile.For{Var: "PROFILES"}, Cmd: `echo "{{.KEY}}"`}}},
	})
	f.write("profiles.json", `{"work": {"directory": "~/.claude-w"}, "personal": {"directory": "~/.claude-p"}}`)

	f.mustRun("show", nil, nil)
	if !strings.Contains(f.out.String(), "dir=~/.claude-w") {
		t.Errorf("stdout = %q, want dir=~/.claude-w", f.out)
	}
	if got := strings.TrimSpace(f.read("env.txt")); got != `{"work":{"directory":"~/.claude-w"},"personal":{"directory":"~/.claude-p"}}` {
		t.Errorf("$PROFILES = %s", got)
	}

	mark := len(f.out.String())
	f.mustRun("each", nil, nil)
	if got := f.out.String()[mark:]; got != "work\npersonal\n" {
		t.Errorf("for: walked %q, want work then personal", got)
	}
}

// A missing file stops the task before anything runs, naming the variable and
// the file.
func TestFileVariableMissingFileStopsTheTask(t *testing.T) {
	f := newFixture(t, &chorefile.File{Vars: map[string]chorefile.Var{
		"SSH": {File: "ssh.json", FileFormat: "json"},
	}}, map[string]*chorefile.Task{
		"unlock": {Cmds: steps("echo ran > ran.txt")},
	})
	err := f.mustFail("unlock", nil, nil)
	if !strings.Contains(err.Error(), "SSH") || !strings.Contains(err.Error(), "ssh.json does not exist") {
		t.Errorf("error = %v; want it to name SSH and ssh.json", err)
	}
	if f.exists("ran.txt") {
		t.Error("the task ran without its config file")
	}
}
