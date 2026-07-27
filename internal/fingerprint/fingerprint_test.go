package fingerprint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// ---------- fakes ----------

// exitErr is what internal/shell promises for a non-zero exit: an error that
// carries the code. Everything in this package hinges on being able to tell it
// apart from "the shell never ran".
type exitErr int

func (e exitErr) Error() string  { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitErr) ExitCode() int  { return int(e) }
func (e exitErr) String() string { return e.Error() }

// startErr is a shell that could not run at all — no exit code exists.
var startErr = errors.New("fork/exec /bin/sh: no such file or directory")

type fakeShell struct {
	codes  map[string]int   // script -> non-zero exit status
	broken map[string]error // script -> failure to start
	calls  []string
}

func (f *fakeShell) Run(ctx context.Context, script string) error {
	f.calls = append(f.calls, script)
	if err, ok := f.broken[script]; ok {
		return err
	}
	if c := f.codes[script]; c != 0 {
		return exitErr(c)
	}
	return nil
}

type fakeCapturer struct {
	out string
	err error
}

func (f fakeCapturer) Capture(ctx context.Context, script string) (string, error) {
	return f.out, f.err
}

// fakeRenderer does just enough templating to prove that patterns and status
// commands are rendered before use. Like the real scope, an undefined variable
// renders to the empty string rather than staying literal — which is exactly
// how a status command can end up empty.
type fakeRenderer struct {
	vars map[string]string
	err  error
}

var placeholder = regexp.MustCompile(`{{\s*\.[A-Za-z0-9_]+\s*}}`)

func (f fakeRenderer) Render(text string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	out := text
	for k, v := range f.vars {
		out = strings.ReplaceAll(out, "{{."+k+"}}", v)
	}
	return placeholder.ReplaceAllString(out, ""), nil
}

// ---------- helpers ----------

func writeFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return abs
}

func removeFile(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("remove %s: %v", rel, err)
	}
}

func newTask(name string) *chorefile.Task {
	return &chorefile.Task{Name: name}
}

// check runs UpToDate and fails on an unexpected error, so the table cases can
// talk about booleans only.
func check(t *testing.T, task *chorefile.Task, r Renderer, sh Runner, dir, cache string) bool {
	t.Helper()
	ok, err := UpToDate(context.Background(), task, r, sh, dir, cache)
	if err != nil {
		t.Fatalf("UpToDate(%s): unexpected error: %v", task.Name, err)
	}
	return ok
}

func mustSave(t *testing.T, task *chorefile.Task, dir, cache string) {
	t.Helper()
	if err := Save(task, dir, cache); err != nil {
		t.Fatalf("Save(%s): %v", task.Name, err)
	}
}

// ---------- status ----------

func TestStatusExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		status []string
		codes  map[string]int
		want   bool
	}{
		{
			name:   "single command exits zero",
			status: []string{"test -f /dev/null"},
			want:   true,
		},
		{
			name:   "all commands exit zero",
			status: []string{"a", "b", "c"},
			want:   true,
		},
		{
			name:   "first command exits non-zero",
			status: []string{"a", "b"},
			codes:  map[string]int{"a": 1},
			want:   false,
		},
		{
			name:   "last command exits non-zero",
			status: []string{"a", "b", "c"},
			codes:  map[string]int{"c": 127},
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := newTask("t")
			task.Status = tc.status
			sh := &fakeShell{codes: tc.codes}

			// A failing status check is the normal "needs to run" signal, never
			// an error: if this ever returns an error the whole run aborts on a
			// task that simply is not done yet.
			got, err := UpToDate(context.Background(), task, nil, sh, t.TempDir(), "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("UpToDate = %v, want %v (calls: %v)", got, tc.want, sh.calls)
			}
		})
	}
}

func TestStatusIsRenderedAndShortCircuits(t *testing.T) {
	task := newTask("t")
	task.Status = []string{"docker inspect {{.NAME}}", "second"}
	sh := &fakeShell{codes: map[string]int{"docker inspect mail4": 1}}
	r := fakeRenderer{vars: map[string]string{"NAME": "mail4"}}

	got, err := UpToDate(context.Background(), task, r, sh, t.TempDir(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("UpToDate = true, want false")
	}
	if want := []string{"docker inspect mail4"}; !reflect.DeepEqual(sh.calls, want) {
		t.Errorf("calls = %v, want %v (rendered, and stopping at the first failure)", sh.calls, want)
	}
}

func TestStatusErrors(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name   string
		ctx    context.Context
		status []string
		r      Renderer
		sh     Runner
		want   string // substring of the error
	}{
		{
			name:   "shell cannot start",
			status: []string{"a"},
			sh:     &fakeShell{broken: map[string]error{"a": startErr}},
			want:   "no such file or directory",
		},
		{
			name:   "no shell supplied",
			status: []string{"a"},
			sh:     nil,
			want:   "needs a shell",
		},
		{
			name:   "renderer fails",
			status: []string{"a"},
			r:      fakeRenderer{err: errors.New("undefined variable")},
			sh:     &fakeShell{},
			want:   "undefined variable",
		},
		{
			name:   "command renders to nothing",
			status: []string{"{{.MISSING}}"},
			r:      fakeRenderer{},
			sh:     &fakeShell{},
			want:   "rendered empty",
		},
		{
			name:   "context cancelled",
			ctx:    canceled,
			status: []string{"a"},
			sh:     &fakeShell{broken: map[string]error{"a": context.Canceled}},
			want:   "context canceled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			task := newTask("t")
			task.Status = tc.status

			got, err := UpToDate(ctx, task, tc.r, tc.sh, t.TempDir(), "")
			if err == nil {
				t.Fatalf("UpToDate = (%v, nil), want an error", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if got {
				t.Error("UpToDate = true alongside an error; a failed check must never skip a task")
			}
		})
	}
}

// TestStatusWinsOverSources pins the documented precedence: `status:` is the
// author's explicit assertion and overrides the inferred checksum answer, in
// both directions.
func TestStatusWinsOverSources(t *testing.T) {
	cases := []struct {
		name string
		code int  // status exit code
		save bool // whether a matching fingerprint exists
		want bool
	}{
		{name: "status zero, checksum would say stale", code: 0, save: false, want: true},
		{name: "status non-zero, checksum would say fresh", code: 1, save: true, want: false},
		{name: "status zero, checksum would say fresh", code: 0, save: true, want: true},
		{name: "status non-zero, checksum would say stale", code: 1, save: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "src/a.go", "package a")

			task := newTask("build")
			task.Status = []string{"check"}
			task.Sources = []string{"src/*.go"}

			if tc.save {
				mustSave(t, task, dir, "")
			}
			sh := &fakeShell{codes: map[string]int{"check": tc.code}}
			if got := check(t, task, nil, sh, dir, ""); got != tc.want {
				t.Errorf("UpToDate = %v, want %v", got, tc.want)
			}
			if len(sh.calls) == 0 {
				t.Error("status command was not run; sources must not shadow status")
			}
		})
	}
}

// ---------- sources / generates ----------

// fixture is a saved-and-up-to-date task: two sources, one generated file.
func fixture(t *testing.T) (dir string, task *chorefile.Task) {
	t.Helper()
	dir = t.TempDir()
	writeFile(t, dir, "src/a.go", "package a")
	writeFile(t, dir, "src/b.go", "package b")
	writeFile(t, dir, "out/app", "binary")

	task = newTask("build")
	task.Sources = []string{"src/*.go"}
	task.Generates = []string{"out/app"}
	mustSave(t, task, dir, "")
	return dir, task
}

func TestChecksumLifecycle(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, dir string)
		want   bool
	}{
		{
			name: "nothing changed",
			want: true,
		},
		{
			name: "source content changed",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "src/a.go", "package a // edited")
			},
			want: false,
		},
		{
			name: "source rewritten with identical content",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "src/a.go", "package a")
			},
			want: true,
		},
		{
			name: "only the mtime moved",
			mutate: func(t *testing.T, dir string) {
				// git checkout, rsync and bind mounts all do this. Rebuilding
				// here is the classic timestamp-based waste this package exists
				// to avoid.
				future := time.Now().Add(48 * time.Hour)
				if err := os.Chtimes(filepath.Join(dir, "src", "a.go"), future, future); err != nil {
					t.Fatalf("chtimes: %v", err)
				}
			},
			want: true,
		},
		{
			name: "new file matched by the source glob",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "src/c.go", "package c")
			},
			want: false,
		},
		{
			name: "new file not matched by the source glob",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "src/README.md", "docs")
			},
			want: true,
		},
		{
			name: "source deleted",
			mutate: func(t *testing.T, dir string) {
				removeFile(t, dir, "src/b.go")
			},
			want: false,
		},
		{
			name: "source renamed, same content",
			mutate: func(t *testing.T, dir string) {
				// Paths are part of the digest, so a rename is a change even
				// though the bytes are identical.
				removeFile(t, dir, "src/b.go")
				writeFile(t, dir, "src/z.go", "package b")
			},
			want: false,
		},
		{
			name: "generated file deleted",
			mutate: func(t *testing.T, dir string) {
				removeFile(t, dir, "out/app")
			},
			want: false,
		},
		{
			name: "generated file content changed",
			mutate: func(t *testing.T, dir string) {
				// Outputs are checked for existence only: a task is not stale
				// because its own product was rewritten.
				writeFile(t, dir, "out/app", "other binary")
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, task := fixture(t)
			if tc.mutate != nil {
				tc.mutate(t, dir)
			}
			if got := check(t, task, nil, nil, dir, ""); got != tc.want {
				t.Errorf("UpToDate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFirstRunThenSave(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/a.go", "package a")

	task := newTask("build")
	task.Sources = []string{"src/*.go"}

	if check(t, task, nil, nil, dir, "") {
		t.Fatal("first run reported up to date; nothing has ever been recorded")
	}
	mustSave(t, task, dir, "")
	if !check(t, task, nil, nil, dir, "") {
		t.Fatal("still not up to date after Save")
	}

	// The whole run loop: change, rerun, save, skip.
	writeFile(t, dir, "src/a.go", "package a // v2")
	if check(t, task, nil, nil, dir, "") {
		t.Fatal("up to date after a source changed")
	}
	mustSave(t, task, dir, "")
	if !check(t, task, nil, nil, dir, "") {
		t.Fatal("not up to date after re-saving")
	}
}

func TestGeneratesDeletedUnderAGlob(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/a.go", "package a")
	writeFile(t, dir, "bin/one", "1")
	writeFile(t, dir, "bin/two", "2")

	task := newTask("build")
	task.Sources = []string{"src/*.go"}
	task.Generates = []string{"bin/*"}
	mustSave(t, task, dir, "")

	if !check(t, task, nil, nil, dir, "") {
		t.Fatal("not up to date immediately after Save")
	}
	// `bin/*` still matches bin/two, so pattern matching alone would say
	// "up to date" — the recorded output list is what catches this.
	removeFile(t, dir, "bin/one")
	if check(t, task, nil, nil, dir, "") {
		t.Error("up to date after one of two generated files was deleted")
	}
}

func TestGeneratesNeverProduced(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/a.go", "package a")

	task := newTask("build")
	task.Sources = []string{"src/*.go"}
	task.Generates = []string{"out/app"}

	// Saving without the output present (a run that failed to produce it) must
	// not make the task skippable.
	mustSave(t, task, dir, "")
	if check(t, task, nil, nil, dir, "") {
		t.Error("up to date while a generates pattern matches nothing")
	}
	writeFile(t, dir, "out/app", "binary")
	if !check(t, task, nil, nil, dir, "") {
		t.Error("not up to date once the output exists and sources are unchanged")
	}
}

func TestNoChecksConfigured(t *testing.T) {
	cases := []struct {
		name string
		task *chorefile.Task
	}{
		{name: "nil task", task: nil},
		{name: "bare task", task: newTask("run")},
		{name: "task with cmds only", task: &chorefile.Task{
			Name: "run",
			Cmds: []chorefile.Cmd{{Cmd: "echo hi"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			got, err := UpToDate(context.Background(), tc.task, nil, nil, dir, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got {
				t.Error("UpToDate = true with no checks configured; such a task must always run")
			}
			// Save is a no-op for these, and must not create a cache dir.
			if err := Save(tc.task, dir, ""); err != nil {
				t.Fatalf("Save: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, DefaultCacheDir)); !os.IsNotExist(err) {
				t.Errorf("cache dir created for a task with nothing to record (stat err: %v)", err)
			}
		})
	}
}

// ---------- glob expansion ----------

func globFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range []string{
		"a.txt",
		"one.log",
		"src/b.txt",
		"src/deep/nested/c.txt",
		"src/deep/nested/d.go",
		".git/objects/e.txt",
		".git/HEAD",
		"node_modules/pkg/f.txt",
		"vendor/node_modules/g.txt",
		".chore/h.txt",
		".chore/fingerprints/i.json",
	} {
		writeFile(t, dir, rel, "content of "+rel)
	}
	return dir
}

// expandRel is expand() reduced to the sorted relative paths it matched.
func expandRel(t *testing.T, dir string, patterns []string, cache string) []string {
	t.Helper()
	matched, _, err := expand(dir, patterns, identity{}, resolveCacheDir(dir, cache))
	if err != nil {
		t.Fatalf("expand(%v): %v", patterns, err)
	}
	out := make([]string, 0, len(matched))
	for _, m := range matched {
		out = append(out, m.rel)
	}
	sort.Strings(out)
	return out
}

func TestExpandGlobs(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		want     []string
	}{
		{
			name:     "double star recurses and skips excluded trees",
			patterns: []string{"**/*.txt"},
			want:     []string{"a.txt", "src/b.txt", "src/deep/nested/c.txt"},
		},
		{
			name:     "double star matches zero directories",
			patterns: []string{"**/a.txt"},
			want:     []string{"a.txt"},
		},
		{
			name:     "double star under a fixed prefix",
			patterns: []string{"src/**/*.txt"},
			want:     []string{"src/b.txt", "src/deep/nested/c.txt"},
		},
		{
			name:     "trailing double star takes the whole subtree",
			patterns: []string{"src/deep/**"},
			want:     []string{"src/deep/nested/c.txt", "src/deep/nested/d.go"},
		},
		{
			name:     "single star does not cross a separator",
			patterns: []string{"*.txt"},
			want:     []string{"a.txt"},
		},
		{
			name:     "literal path",
			patterns: []string{"src/b.txt"},
			want:     []string{"src/b.txt"},
		},
		{
			name:     "literal path is honoured inside an excluded tree",
			patterns: []string{".git/HEAD"},
			want:     []string{".git/HEAD"},
		},
		{
			name:     "nested node_modules is excluded too",
			patterns: []string{"vendor/**/*.txt"},
			want:     nil,
		},
		{
			name:     "the cache dir is never a source of itself",
			patterns: []string{"**/*.txt", "**/*.json"},
			want:     []string{"a.txt", "src/b.txt", "src/deep/nested/c.txt"},
		},
		{
			name:     "two patterns matching the same file yield it once",
			patterns: []string{"a.txt", "*.txt", "**/a.txt"},
			want:     []string{"a.txt"},
		},
		{
			name:     "character class",
			patterns: []string{"[ao]*.txt"},
			want:     []string{"a.txt"},
		},
		{
			name:     "no match",
			patterns: []string{"nothing/here/*.txt"},
			want:     nil,
		},
		{
			name:     "missing directory is not an error",
			patterns: []string{"absent/**/*.go"},
			want:     nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := globFixture(t)
			got := expandRel(t, dir, tc.patterns, "")
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("matched %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExpandReportsUnmatchedPatterns(t *testing.T) {
	dir := globFixture(t)
	_, unmatched, err := expand(dir, []string{"a.txt", "gone/*.txt", "also-missing"}, identity{}, resolveCacheDir(dir, ""))
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	want := []string{"gone/*.txt", "also-missing"}
	if !reflect.DeepEqual(unmatched, want) {
		t.Errorf("unmatched = %v, want %v", unmatched, want)
	}
}

// TestCacheDirIsNotASource is the loop this package must not close: the
// fingerprint file itself must never feed back into the checksum.
func TestCacheDirIsNotASource(t *testing.T) {
	for _, cache := range []string{"", ".cache", "custom/deep/cache"} {
		t.Run("cacheDir="+cache, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "src/a.txt", "a")

			task := newTask("build")
			task.Sources = []string{"**/*"} // deliberately greedy
			mustSave(t, task, dir, cache)

			if !check(t, task, nil, nil, dir, cache) {
				t.Fatal("writing the fingerprint invalidated the fingerprint")
			}
			// And a second save must not flip it either.
			mustSave(t, task, dir, cache)
			if !check(t, task, nil, nil, dir, cache) {
				t.Fatal("re-saving invalidated the fingerprint")
			}
		})
	}
}

func TestExpandRendersPatterns(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config/mail4.test/config.env", "A=1")

	task := newTask("config:check")
	task.Sources = []string{"config/{{.CONFIG}}/*.env"}
	r := fakeRenderer{vars: map[string]string{"CONFIG": "mail4.test"}}

	// Save must use the same renderer, otherwise the two hash different file
	// sets and the task can never be up to date.
	if err := SaveWith(task, r, dir, ""); err != nil {
		t.Fatalf("SaveWith: %v", err)
	}
	if !check(t, task, r, nil, dir, "") {
		t.Error("not up to date after SaveWith with the same renderer")
	}

	// The trap Save() documents: unrendered patterns match nothing, so the
	// recorded hash belongs to an empty set and never matches the rendered one.
	mustSave(t, task, dir, "")
	if check(t, task, r, nil, dir, "") {
		t.Error("plain Save produced a fingerprint that matched a rendered check")
	}

	// A renderer that fails is a real error, not "needs to run".
	if _, err := UpToDate(context.Background(), task, fakeRenderer{err: errors.New("boom")}, nil, dir, ""); err == nil {
		t.Error("expected an error when a source pattern cannot be rendered")
	}
}

// TestAbsolutePatterns covers the shape rest-mail actually writes:
// `sources: ['{{.ROOT_DIR}}/internal/**/*.go']`, which renders to an absolute
// path. The recorded paths must still come out relative to the task's dir.
func TestAbsolutePatterns(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "internal/a.go", "package a")
	writeFile(t, dir, "internal/deep/b.go", "package b")
	writeFile(t, dir, "bin/app", "binary")

	task := newTask("build")
	task.Sources = []string{"{{.ROOT_DIR}}/internal/**/*.go"}
	task.Generates = []string{"{{.ROOT_DIR}}/bin/app"}
	r := fakeRenderer{vars: map[string]string{"ROOT_DIR": dir}}

	if err := SaveWith(task, r, dir, ""); err != nil {
		t.Fatalf("SaveWith: %v", err)
	}
	b, err := os.ReadFile(Path(task, dir, ""))
	if err != nil {
		t.Fatalf("read fingerprint: %v", err)
	}
	var fp stored
	if err := json.Unmarshal(b, &fp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantSources := map[string]string{
		"internal/a.go":      sha256hex("package a"),
		"internal/deep/b.go": sha256hex("package b"),
	}
	if !reflect.DeepEqual(fp.Sources, wantSources) {
		t.Errorf("sources = %v, want %v (paths must be relative to the task dir)", fp.Sources, wantSources)
	}
	if !reflect.DeepEqual(fp.Generates, []string{"bin/app"}) {
		t.Errorf("generates = %v, want [bin/app]", fp.Generates)
	}
	if !check(t, task, r, nil, dir, "") {
		t.Error("not up to date after SaveWith with absolute patterns")
	}
	writeFile(t, dir, "internal/deep/b.go", "package b // edited")
	if check(t, task, r, nil, dir, "") {
		t.Error("up to date after an absolute-matched source changed")
	}
}

// TestNonRegularFilesAreIgnored: a broken symlink under a `**` glob must not
// fail the check — a tree with a dangling link is common (build artefacts,
// vendored trees) and hashing one would error out the whole run.
func TestNonRegularFilesAreIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/a.go", "package a")
	if err := os.Symlink(filepath.Join(dir, "src", "gone.go"), filepath.Join(dir, "src", "dangling.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	task := newTask("build")
	task.Sources = []string{"src/**/*.go"}

	got := expandRel(t, dir, task.Sources, "")
	if !reflect.DeepEqual(got, []string{"src/a.go"}) {
		t.Errorf("matched %v, want [src/a.go]", got)
	}
	mustSave(t, task, dir, "")
	if !check(t, task, nil, nil, dir, "") {
		t.Error("not up to date with a dangling symlink in the source tree")
	}
}

func TestMatchSegments(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"**/*.go", "a.go", true},
		{"**/*.go", "a/b/c.go", true},
		{"**/*.go", "a/b/c.txt", false},
		{"src/**", "src/a/b.go", true},
		{"src/**", "other/a.go", false},
		{"*.go", "a/b.go", false},
		{"src/*/x.go", "src/a/x.go", true},
		{"src/*/x.go", "src/a/b/x.go", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/y/c", false},
		{"?.go", "a.go", true},
		{"?.go", "ab.go", false},
		{"[ab].go", "a.go", true},
		{"[ab].go", "c.go", false},
		{"src/a.go", "src/a.go", true},
		{"src/a.go", "src/a.go.bak", false},
		{"src", "src/a.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+" vs "+tc.name, func(t *testing.T) {
			got := matchSegments(splitSegments(tc.pattern), splitSegments(tc.name))
			if got != tc.want {
				t.Errorf("matchSegments(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
			}
		})
	}
}

func TestBadPatternIsAnError(t *testing.T) {
	dir := t.TempDir()
	task := newTask("build")
	task.Sources = []string{"src/[unclosed*.go"}

	if _, err := UpToDate(context.Background(), task, nil, nil, dir, ""); err == nil {
		t.Error("expected an error for a malformed glob")
	}
}

// ---------- the cache ----------

func TestCacheDirCreatedOnDemand(t *testing.T) {
	cases := []string{"", ".chore", "nested/does/not/exist"}
	for _, cache := range cases {
		t.Run("cacheDir="+cache, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "src/a.go", "package a")

			task := newTask("build")
			task.Sources = []string{"src/*.go"}

			// Checking before anything exists must not fail, and must not
			// create the directory either.
			if check(t, task, nil, nil, dir, cache) {
				t.Fatal("up to date with no cache present")
			}
			mustSave(t, task, dir, cache)

			p := Path(task, dir, cache)
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("fingerprint not written to %s: %v", p, err)
			}
			if !check(t, task, nil, nil, dir, cache) {
				t.Error("not up to date after Save")
			}
		})
	}
}

func TestAbsoluteCacheDir(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(t.TempDir(), "elsewhere")
	writeFile(t, dir, "src/a.go", "package a")

	task := newTask("build")
	task.Sources = []string{"src/*.go"}
	mustSave(t, task, dir, cache)

	if !strings.HasPrefix(Path(task, dir, cache), cache) {
		t.Errorf("Path = %s, want it under %s", Path(task, dir, cache), cache)
	}
	if !check(t, task, nil, nil, dir, cache) {
		t.Error("not up to date with an absolute cache dir")
	}
	if _, err := os.Stat(filepath.Join(dir, DefaultCacheDir)); !os.IsNotExist(err) {
		t.Error("default cache dir created even though an absolute one was given")
	}
}

func TestUnusableFingerprintMeansNotUpToDate(t *testing.T) {
	cases := []struct {
		name    string
		content func(valid []byte) []byte
	}{
		{
			name:    "not json",
			content: func([]byte) []byte { return []byte("{{{ not json at all") },
		},
		{
			name:    "empty file",
			content: func([]byte) []byte { return nil },
		},
		{
			name: "truncated mid-write",
			content: func(valid []byte) []byte {
				return valid[:len(valid)/2]
			},
		},
		{
			name:    "json but no hash",
			content: func([]byte) []byte { return []byte(`{"version":1,"task":"build"}`) },
		},
		{
			name: "hash from a future format version",
			content: func(valid []byte) []byte {
				var m map[string]any
				if err := json.Unmarshal(valid, &m); err != nil {
					panic(err)
				}
				m["version"] = formatVersion + 1
				b, _ := json.Marshal(m)
				return b
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, task := fixture(t)
			p := Path(task, dir, "")
			valid, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read fingerprint: %v", err)
			}
			if err := os.WriteFile(p, tc.content(valid), 0o644); err != nil {
				t.Fatalf("corrupt fingerprint: %v", err)
			}

			// A damaged cache file is derived data: discard it and rerun, never
			// abort the run over it.
			got, err := UpToDate(context.Background(), task, nil, nil, dir, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got {
				t.Error("UpToDate = true with an unusable fingerprint")
			}
			// And a subsequent Save must repair it.
			mustSave(t, task, dir, "")
			if !check(t, task, nil, nil, dir, "") {
				t.Error("Save did not restore a usable fingerprint")
			}
		})
	}
}

func TestFingerprintFileFormat(t *testing.T) {
	dir, task := fixture(t)

	b, err := os.ReadFile(Path(task, dir, ""))
	if err != nil {
		t.Fatalf("read fingerprint: %v", err)
	}
	var got struct {
		Version   int               `json:"version"`
		Task      string            `json:"task"`
		Hash      string            `json:"hash"`
		Sources   map[string]string `json:"sources"`
		Generates []string          `json:"generates"`
		UpdatedAt string            `json:"updated_at"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("fingerprint is not valid JSON: %v\n%s", err, b)
	}
	if got.Version != formatVersion {
		t.Errorf("version = %d, want %d", got.Version, formatVersion)
	}
	if got.Task != "build" {
		t.Errorf("task = %q, want %q", got.Task, "build")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(got.Hash) {
		t.Errorf("hash = %q, want 64 hex characters", got.Hash)
	}
	// Per-file digests are keyed by slash-separated paths relative to the
	// task's directory, so the cache survives the checkout being moved.
	wantSources := map[string]string{
		"src/a.go": sha256hex("package a"),
		"src/b.go": sha256hex("package b"),
	}
	if !reflect.DeepEqual(got.Sources, wantSources) {
		t.Errorf("sources = %v, want %v", got.Sources, wantSources)
	}
	if !reflect.DeepEqual(got.Generates, []string{"out/app"}) {
		t.Errorf("generates = %v, want [out/app]", got.Generates)
	}
	if _, err := time.Parse(time.RFC3339, got.UpdatedAt); err != nil {
		t.Errorf("updated_at = %q, not RFC3339: %v", got.UpdatedAt, err)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Error("fingerprint file does not end in a newline")
	}
}

func TestHashIsStableAndPathSensitive(t *testing.T) {
	dirA := t.TempDir()
	writeFile(t, dirA, "src/a.go", "package a")
	dirB := t.TempDir()
	writeFile(t, dirB, "src/a.go", "package a")
	dirC := t.TempDir()
	writeFile(t, dirC, "src/other.go", "package a")

	hashOf := func(dir string) string {
		t.Helper()
		files, _, err := expand(dir, []string{"src/*.go"}, identity{}, resolveCacheDir(dir, ""))
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		fp, err := hashFiles(files)
		if err != nil {
			t.Fatalf("hashFiles: %v", err)
		}
		return fp.Hash
	}
	// Same relative paths + same content in two different absolute locations
	// must hash identically, or moving a checkout would rebuild the world.
	if hashOf(dirA) != hashOf(dirB) {
		t.Error("hash depends on the absolute location of the tree")
	}
	if hashOf(dirA) == hashOf(dirC) {
		t.Error("hash ignores file names; a rename would go unnoticed")
	}
}

func TestSanitiseKeepsNamesDistinctAndSafe(t *testing.T) {
	cases := []string{
		"build",
		"postgres:up",
		"postgres_up",
		"ns:sub:task",
		"weird/../name",
		strings.Repeat("x", 300),
		"",
	}
	seen := map[string]string{}
	for _, name := range cases {
		got := sanitise(name)
		if strings.ContainsAny(got, `/\:`) {
			t.Errorf("sanitise(%q) = %q, contains a path separator", name, got)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("sanitise(%q) collides with sanitise(%q) at %q", name, prev, got)
		}
		seen[got] = name
	}
	if !strings.HasPrefix(sanitise("postgres:up"), "postgres_up-") {
		t.Errorf("sanitise dropped the readable prefix: %q", sanitise("postgres:up"))
	}
	if len(sanitise(strings.Repeat("x", 300))) > 128 {
		t.Error("sanitise did not bound the filename length")
	}
}

func TestTasksDoNotShareFingerprints(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/a.go", "package a")

	// Names that sanitise to the same readable stem must not share a file:
	// one would silently mark the other up to date.
	a := newTask("postgres:up")
	a.Sources = []string{"src/*.go"}
	b := newTask("postgres_up")
	b.Sources = []string{"src/*.go"}

	mustSave(t, a, dir, "")
	if !check(t, a, nil, nil, dir, "") {
		t.Fatal("saved task is not up to date")
	}
	if check(t, b, nil, nil, dir, "") {
		t.Error("a different task was reported up to date from another task's fingerprint")
	}
}

// ---------- adapters ----------

func TestRunnerFromCapturer(t *testing.T) {
	task := newTask("t")
	task.Status = []string{"a"}

	ok, err := UpToDate(context.Background(), task, nil, RunnerFromCapturer(fakeCapturer{out: "yes"}), t.TempDir(), "")
	if err != nil || !ok {
		t.Errorf("UpToDate = (%v, %v), want (true, nil)", ok, err)
	}

	ok, err = UpToDate(context.Background(), task, nil, RunnerFromCapturer(fakeCapturer{err: exitErr(1)}), t.TempDir(), "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if ok {
		t.Error("UpToDate = true, want false for a non-zero capture")
	}
}

func TestExitCoderDetection(t *testing.T) {
	// Guards the contract with internal/shell: exit codes must arrive wrapped
	// in something that reports them. If they ever stop doing so, every failing
	// status check turns into an aborted run — hence a test on the wrapping too.
	cases := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{name: "plain exit code", err: exitErr(1)},
		{name: "wrapped exit code", err: fmt.Errorf("task failed: %w", exitErr(2))},
		{name: "no exit code", err: startErr, wantErr: true},
		{name: "wrapped plain error", err: fmt.Errorf("context: %w", startErr), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := newTask("t")
			task.Status = []string{"a"}
			sh := &fakeShell{broken: map[string]error{"a": tc.err}}

			ok, err := UpToDate(context.Background(), task, nil, sh, t.TempDir(), "")
			if ok {
				t.Error("UpToDate = true, want false")
			}
			if tc.wantErr != (err != nil) {
				t.Errorf("error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
