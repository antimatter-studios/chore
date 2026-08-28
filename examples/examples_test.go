// Package examples runs the curated example taskfiles and compares what they
// print against a golden file recorded beside each one.
//
// These are documentation that cannot go stale. A comment claiming `after` runs
// on both paths is a claim; a golden file showing it is evidence, and CI fails
// the moment the claim stops being true. They are also the only tests here that
// exercise the whole program end to end — flags, loading, scheduling, hooks and
// exit status — through the same entry point a person types.
//
// Update them all with:
//
//	go test ./examples -update
//
// By default they drive cli.Main in this process, which is fast and gives real
// stack traces. Set CHORE_BIN to a built binary and they exec THAT instead:
//
//	chore build
//	CHORE_BIN=$PWD/bin/chore go test ./examples -count=1
//
// Same goldens either way, which is the point — it checks the artifact people
// actually install, not just the library it is built from. A difference between
// the two modes is a packaging bug (a missing embed, a stamp, a linker flag)
// that no in-process test could see.
package examples

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/cli"
)

var update = flag.Bool("update", false, "rewrite the .golden files from what the examples actually print")

// run is one invocation recorded in a golden file: a label, the words after
// `chore`, and everything it produced.
type run struct {
	name string
	args []string
}

// cases maps each example to the invocations worth recording for it. Several
// examples only mean something as a PAIR of runs — the same file with and
// without a flag, or run twice to reach the up-to-date path — so the golden
// file holds the whole sequence rather than one command.
var cases = map[string][]run{
	"01-order.yml":        {{"success path", []string{"demo"}}},
	"02-failure.yml":      {{"failure path", []string{"demo"}}},
	"03-before-gates.yml": {{"a failing gate", []string{"demo"}}},
	"04-best-effort.yml": {
		{"a failing after does not fail the task", []string{"hook-failure-is-not-fatal"}},
		{"a failing defer does", []string{"defer-failure-is-fatal"}},
	},
	"05-child-hooks.yml": {{"the subtree is silent, the coordinator is not", []string{"all"}}},
	"06-lifecycle.yml":   {{"run level around task level", []string{"demo"}}},
	"07-no-lifecycle.yml": {
		{"hooks on", []string{"demo"}},
		{"hooks off, deps and defer untouched", []string{"--no-lifecycle", "demo"}},
	},
	"08-up-to-date.yml": {
		{"first run: the task does the work", []string{"build"}},
		{"second run: skipped, but the hooks still fire", []string{"build"}},
	},
	"09-run-once.yml":   {{"three references, one run, one after", []string{"demo"}}},
	"10-task-scope.yml": {{"a hook reads the task's own argument", []string{"build", "ext4"}}},
}

func TestExamples(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("hooks", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no examples found")
	}
	sort.Strings(files)

	// chore's own -C changes the PROCESS's working directory, so a relative path
	// read after the first run would resolve somewhere else. Everything below is
	// absolute, and the directory is put back between runs.
	home, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(home) })

	for _, path := range files {
		path := filepath.Join(home, path)
		base := filepath.Base(path)
		runs, ok := cases[base]
		if !ok {
			// An example with no recorded invocation is documentation nothing
			// verifies, which is the exact failure these files exist to prevent.
			t.Errorf("%s has no entry in `cases` — every example must be executed", base)
			continue
		}
		t.Run(base, func(t *testing.T) {
			got := record(t, path, runs)
			golden := strings.TrimSuffix(path, ".yml") + ".golden"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v — run `go test ./examples -update` to record it", err)
			}
			if got != string(want) {
				t.Errorf("output changed.\n\n--- want ---\n%s\n--- got ---\n%s", want, got)
			}
		})
	}
}

// record runs every invocation for one example in a fresh directory and renders
// the transcript that becomes the golden file.
//
// The taskfile is COPIED into a temp directory rather than run in place: the
// up-to-date example writes files and records a fingerprint, and an example that
// left artefacts in the repository would pass once and then fail.
func record(t *testing.T, path string, runs []run) string {
	t.Helper()
	// Put the working directory back after every example, since -C moved it.
	if home, err := os.Getwd(); err == nil {
		defer func() { _ = os.Chdir(home) }()
	}
	dir := t.TempDir()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(path)
	if err := os.WriteFile(filepath.Join(dir, name), src, 0o644); err != nil {
		t.Fatal(err)
	}
	// One example builds a file from a source; give it the source.
	if err := os.WriteFile(filepath.Join(dir, "src.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	for i, r := range runs {
		if i > 0 {
			b.WriteString("\n")
		}
		var out, errOut bytes.Buffer
		// -C keeps chore's own working directory out of it, and NO_COLOR is
		// implicit because neither destination is a terminal.
		args := append([]string{"-C", dir, "-f", filepath.Join(dir, name)}, r.args...)
		code := invoke(t, args, &out, &errOut)

		fmt.Fprintf(&b, "$ chore %s\n", strings.Join(r.args, " "))
		fmt.Fprintf(&b, "# %s\n", r.name)
		b.WriteString(clean(out.String(), dir))
		if e := clean(errOut.String(), dir); e != "" {
			for _, line := range strings.Split(strings.TrimRight(e, "\n"), "\n") {
				fmt.Fprintf(&b, "(stderr) %s\n", line)
			}
		}
		fmt.Fprintf(&b, "exit %d\n", code)
	}
	return b.String()
}

// invoke runs one command line, either in this process or through the binary
// named by CHORE_BIN.
func invoke(t *testing.T, args []string, out, errOut *bytes.Buffer) int {
	t.Helper()
	bin := os.Getenv("CHORE_BIN")
	if bin == "" {
		return cli.Main(args, out, errOut)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = out, errOut
	// The examples pass an absolute -f and -C, so the child's own working
	// directory does not matter; setting it anyway keeps a failure legible.
	cmd.Dir = t.TempDir()
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	default:
		t.Fatalf("running %s: %v", bin, err)
		return 1
	}
}

// clean removes what would make a golden file machine-specific: the temp
// directory's path, and the dev-build banner that only appears when the version
// is not a release.
func clean(s, dir string) string {
	s = strings.ReplaceAll(s, dir, "<dir>")
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "development build") || strings.Contains(line, "chore dev") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}
