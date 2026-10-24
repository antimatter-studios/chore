package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// cacheFixture is a fixture whose runner keeps replays under a directory of its
// own and reads a clock the test moves.
func cacheFixture(t *testing.T, tasks map[string]*chorefile.Task) (*fixture, *time.Time) {
	t.Helper()
	f := newFixture(t, nil, tasks)
	f.r.CacheDir = filepath.Join(t.TempDir(), "cache")
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	f.r.Now = func() time.Time { return now }
	return f, &now
}

// lines counts the lines of a file the task appends to once per real run.
func (f *fixture) lines(rel string) int {
	f.t.Helper()
	if !f.exists(rel) {
		return 0
	}
	return strings.Count(f.read(rel), "\n")
}

// since is what the task printed after mark: one invocation's stdout.
func (f *fixture) since(mark int) string {
	return f.out.String()[mark:]
}

// entries counts the replay files under the cache directory.
func entries(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

const minute = chorefile.Duration(time.Minute)

func TestCacheReplaysWithinItsLifetime(t *testing.T) {
	f, _ := cacheFixture(t, map[string]*chorefile.Task{
		"queue": {Cache: 5 * minute, Cmds: steps("echo ran >> runs", "echo the answer")},
	})

	f.mustRun("queue", nil, nil)
	first := f.out.String()
	mark := len(first)
	f.mustRun("queue", nil, nil)

	if got := f.lines("runs"); got != 1 {
		t.Errorf("the task ran %d times inside its lifetime, want 1", got)
	}
	if replay := f.since(mark); replay != first {
		t.Errorf("replay = %q, want exactly what the run printed, %q", replay, first)
	}
}

func TestCacheRunsAgainOnceExpired(t *testing.T) {
	f, now := cacheFixture(t, map[string]*chorefile.Task{
		"queue": {Cache: 5 * minute, Cmds: steps("echo ran >> runs")},
	})

	f.mustRun("queue", nil, nil)
	*now = now.Add(5*time.Minute - time.Second)
	f.mustRun("queue", nil, nil)
	if got := f.lines("runs"); got != 1 {
		t.Fatalf("a second before expiry the task ran %d times, want 1", got)
	}
	*now = now.Add(2 * time.Second)
	f.mustRun("queue", nil, nil)
	if got := f.lines("runs"); got != 2 {
		t.Errorf("after expiry the task ran %d times, want 2", got)
	}
}

// A failure is never replayed: the next call has to ask again, or a network
// blip would read as the answer for the rest of the lifetime.
func TestCacheKeepsNoFailure(t *testing.T) {
	f, _ := cacheFixture(t, map[string]*chorefile.Task{
		"queue": {Cache: 5 * minute, Cmds: steps("echo ran >> runs", "echo partial", "exit 3")},
	})

	f.mustFail("queue", nil, nil)
	f.mustFail("queue", nil, nil)

	if got := f.lines("runs"); got != 2 {
		t.Errorf("a failed task ran %d times in two calls, want 2", got)
	}
	if n := entries(t, f.r.CacheDir); n != 0 {
		t.Errorf("a failed run left %d cache entries", n)
	}
}

func TestCacheKeysOnArguments(t *testing.T) {
	f, _ := cacheFixture(t, map[string]*chorefile.Task{
		"queue": {
			Cache: 5 * minute,
			Args:  chorefile.Args{{Name: "project"}},
			Cmds:  steps("echo {{.PROJECT}} >> runs", "echo queue of {{.PROJECT}}"),
		},
	})

	f.mustRun("queue", []string{"a"}, nil)
	f.mustRun("queue", []string{"b"}, nil)
	mark := len(f.out.String())
	f.mustRun("queue", []string{"a"}, nil)

	if got := f.read("runs"); got != "a\nb\n" {
		t.Errorf("runs = %q; want a and b once each", got)
	}
	if got := f.since(mark); got != "queue of a\n" {
		t.Errorf("the replay for a = %q", got)
	}
}

// --force runs the task and the new answer replaces the kept one.
func TestCacheForceRunsAndRefreshes(t *testing.T) {
	f, _ := cacheFixture(t, map[string]*chorefile.Task{
		"queue": {Cache: 5 * minute, Cmds: steps("echo ran >> runs", "echo run $(wc -l < runs)")},
	})

	f.mustRun("queue", nil, nil)
	f.r.Force = true
	f.mustRun("queue", nil, nil)
	f.r.Force = false
	mark := len(f.out.String())
	f.mustRun("queue", nil, nil)

	if got := f.lines("runs"); got != 2 {
		t.Errorf("ran %d times, want 2: once, then once forced", got)
	}
	if got := strings.TrimSpace(f.since(mark)); got != "run 2" {
		t.Errorf("after --force the replay is %q, want the forced run's %q", got, "run 2")
	}
}

// A replay is the task's whole output, so it stands in for its deps and hooks:
// none of them runs again, and what they printed is replayed with the rest.
func TestCacheReplaySkipsDepsAndHooks(t *testing.T) {
	f, _ := cacheFixture(t, map[string]*chorefile.Task{
		"prep":  {Cmds: steps("echo prep >> preps", "echo prepared")},
		"queue": {Cache: 5 * minute, Deps: depsOn("prep"), Before: steps("echo before >> hooks"), Cmds: steps("echo answer")},
	})

	f.mustRun("queue", nil, nil)
	mark := len(f.out.String())
	f.mustRun("queue", nil, nil)

	if f.lines("preps") != 1 || f.lines("hooks") != 1 {
		t.Errorf("a replay ran a dep (%d) or a hook (%d) again", f.lines("preps"), f.lines("hooks"))
	}
	if got := f.since(mark); !strings.Contains(got, "prepared") || !strings.Contains(got, "answer") {
		t.Errorf("the replay = %q; want the dep's output and the task's", got)
	}
}

// Two tasks running side by side share one stdout; only the cached task's own
// output may end up in its entry.
func TestCacheRecordsOnlyItsOwnOutput(t *testing.T) {
	f, _ := cacheFixture(t, map[string]*chorefile.Task{
		"queue": {Cache: 5 * minute, Cmds: steps("sleep 0.2", "echo mine")},
		"other": {Cmds: steps("echo theirs", "sleep 0.3", "echo theirs again")},
		"top":   {Deps: depsOn("queue", "other")},
	})

	f.mustRun("top", nil, nil)
	mark := len(f.out.String())
	f.mustRun("queue", nil, nil)

	if got := f.since(mark); got != "mine\n" {
		t.Errorf("the replay = %q; want only the cached task's own %q", got, "mine\n")
	}
}

func TestCacheMissesOnceTheTaskIsEdited(t *testing.T) {
	task := &chorefile.Task{Cache: 5 * minute, Cmds: steps("echo ran >> runs", "echo old")}
	f, _ := cacheFixture(t, map[string]*chorefile.Task{"queue": task})

	f.mustRun("queue", nil, nil)
	task.Cmds = steps("echo ran >> runs", "echo new")
	mark := len(f.out.String())
	f.mustRun("queue", nil, nil)

	if got := f.since(mark); got != "new\n" {
		t.Errorf("after an edit the output = %q; want the edited task's %q", got, "new\n")
	}
}

// Two projects with a task of the same name and the same arguments keep one
// entry each.
func TestCacheIsPerChorefile(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "cache")
	var fs []*fixture
	for range 2 {
		f, _ := cacheFixture(t, map[string]*chorefile.Task{
			"queue": {Cache: 5 * minute, Cmds: steps("echo ran >> runs", "basename \"$PWD\"")},
		})
		f.r.CacheDir = shared
		fs = append(fs, f)
	}
	for _, f := range fs {
		f.mustRun("queue", nil, nil)
		f.mustRun("queue", nil, nil)
	}
	for i, f := range fs {
		if got := f.lines("runs"); got != 1 {
			t.Errorf("project %d ran %d times, want 1", i, got)
		}
		if got := strings.TrimSpace(f.out.String()); got != filepath.Base(f.dir)+"\n"+filepath.Base(f.dir) {
			t.Errorf("project %d replayed %q, not its own directory", i, got)
		}
	}
}

// The key is what the chorefile declares, not the shell it was started from:
// a variable of the environment that changes between shells must not miss.
func TestCacheIgnoresTheProcessEnvironment(t *testing.T) {
	f, _ := cacheFixture(t, map[string]*chorefile.Task{
		"queue": {Cache: 5 * minute, Cmds: steps("echo ran >> runs")},
	})

	t.Setenv("CHORE_CACHE_TEST_NOISE", "one")
	f.mustRun("queue", nil, nil)
	t.Setenv("CHORE_CACHE_TEST_NOISE", "two")
	f.mustRun("queue", nil, nil)

	if got := f.lines("runs"); got != 1 {
		t.Errorf("a change to the environment made the task run %d times, want 1", got)
	}
}

// With nowhere to keep an answer there is no cache, and the task simply runs.
func TestCacheWithNoDirectoryAlwaysRuns(t *testing.T) {
	f, _ := cacheFixture(t, map[string]*chorefile.Task{
		"queue": {Cache: 5 * minute, Cmds: steps("echo ran >> runs")},
	})
	f.r.CacheDir = ""

	f.mustRun("queue", nil, nil)
	f.mustRun("queue", nil, nil)

	if got := f.lines("runs"); got != 2 {
		t.Errorf("with no cache directory the task ran %d times, want 2", got)
	}
}

// Writing an entry removes the project's expired ones, so the directory does
// not grow without end.
func TestCacheRemovesExpiredEntriesWhenWriting(t *testing.T) {
	f, now := cacheFixture(t, map[string]*chorefile.Task{
		"queue": {Cache: 5 * minute, Args: chorefile.Args{{Name: "project"}}, Cmds: steps("echo {{.PROJECT}}")},
	})

	f.mustRun("queue", []string{"a"}, nil)
	f.mustRun("queue", []string{"b"}, nil)
	if n := entries(t, f.r.CacheDir); n != 2 {
		t.Fatalf("two arguments left %d entries, want 2", n)
	}
	*now = now.Add(10 * time.Minute)
	f.mustRun("queue", []string{"c"}, nil)

	if n := entries(t, f.r.CacheDir); n != 1 {
		t.Errorf("after both expired and a third was written, %d entries remain, want 1", n)
	}
}

// --dry runs nothing, so it has nothing to keep and replays nothing.
func TestCacheDryRunKeepsNothing(t *testing.T) {
	f, _ := cacheFixture(t, map[string]*chorefile.Task{
		"queue": {Cache: 5 * minute, Cmds: steps("echo ran >> runs")},
	})
	f.r.DryRun = true

	f.mustRun("queue", nil, nil)

	if n := entries(t, f.r.CacheDir); n != 0 {
		t.Errorf("--dry left %d cache entries", n)
	}
}
