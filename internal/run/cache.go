package run

// cache.go — `cache:`, a task's recorded stdout replayed for its lifetime.
//
// chore is a single-shot command, so what one run learns has to be on disk for
// the next to find. A task that declares `cache: 5m` is run as it always is,
// its stdout streaming live, while everything it writes there is copied aside.
// A run that succeeds is kept; for the next five minutes the task is not run
// at all, and chore prints the copy instead. A run that fails keeps nothing,
// so a failure is never replayed as an answer.
//
// WHERE. One file per entry, under the user's cache directory:
//
//	$XDG_CACHE_HOME/chore/replay/<chorefile>/<key>.replay
//
// <chorefile> is a hash of the chorefile's path, so two projects' `queue:list`
// never meet. <key> is a hash of the task's name, its declared variables and
// arguments (Scope.Declared — never the process environment, which differs
// from one shell to the next) and the steps a replay stands in for: its cmds,
// deps and before. Editing any of those makes the old entries unreachable.
//
// The file is one JSON line — task, created, expires, chore version — and then
// the recorded bytes. The lifetime is written into the entry when it is made,
// not inferred from the file's age. An entry is written to a temporary file
// beside it and renamed over it, so a reader never sees half of one, and every
// write removes the chorefile's expired entries, so the directory does not grow
// without end.
//
// WHAT IS RECORDED is what the task writes to stdout, its deps', hooks' and
// `- task:` steps' included — a replay runs none of them, so it has to stand in
// for all of them. It is recorded through the context (see output), not by
// wrapping the runner's stdout, so a task running beside this one, which
// shares that stdout, is never captured into its entry. stderr is not recorded:
// warnings belong to the run that produced them.
//
// WHEN THERE IS NO CACHE: with no cache directory, under --dry, or when the
// task's output is being discarded (a named precondition check), the task runs
// as if it declared nothing. A cache that cannot be written is no cache; the
// task still runs and still answers.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/tmpl"
)

// chore:manual cache
// title: Replaying an answer
// summary: cache: — a task's output, replayed for a lifetime instead of run again
// aliases: replay replays ttl lifetime
// order: 10
//
// # Replaying an answer
//
// ```yaml
// queue:list:
//   cache: 5m
//   cmds: [ scripts/am-queue {{.CLI_ARGS}} ]
// ```
//
// A task that reads something remote — an issue tracker, a CI system, a
// package registry — is run again and again within minutes, and the answer
// has almost never moved. `cache:` says how long it stays true. The first run
// is an ordinary run, its output streaming as always, and what it writes to
// stdout is recorded. For the next five minutes the task is not run: chore
// prints the recording and exits 0.
//
// - **Only a success is kept.** A run that fails records nothing, so the next
//   call runs the task again rather than replaying a network error as the
//   answer for five minutes.
// - **The whole task is replayed.** Its `deps:`, hooks and `- task:` steps are
//   recorded with it and are not run on a replay either. A task running
//   alongside it is never recorded into its replay.
// - **Different arguments are different answers.** The replay is kept per
//   chorefile, task, arguments and declared variables, and per the text of
//   its `cmds:`, `deps:` and `before:` — editing the task makes the old
//   replays unreachable.
// - **The environment is not part of it.** Variables that differ from one
//   shell to the next must not make every call miss, so the process
//   environment is left out. A task whose answer depends on an environment
//   variable should declare it in `vars:`, where the replay can see it.
// - **stderr is not replayed.** Warnings belong to the run that printed them.
//
// `--force` runs the task anyway and replaces the recording; `--dry` runs
// nothing and keeps nothing. A replay is for tasks that read: a task whose
// point is a side effect must not declare one. `cache:` with `interactive:` or
// `confirm:` is refused when the file loads — there is no terminal session to
// play back, and a replayed question is a question not asked.
//
// Replays live in the user's cache directory — `$XDG_CACHE_HOME/chore/replay`,
// or `~/.cache/chore/replay` — one file each, written whole or not at all, and
// a project's expired replays are removed whenever it writes a new one. With
// neither directory known, the task simply runs.

// recordKey carries, in a context, the writer a cached task's output goes to.
type recordKey struct{}

// replayMeta is the first line of an entry.
type replayMeta struct {
	Task    string    `json:"task"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Chore   string    `json:"chore,omitempty"`
}

// recorder writes to the stream the task would have written to and to the
// recording. Concurrent deps write to it at once, so it is locked. A recording
// that fails to write is given up on, never the stream: the person watching
// must not lose output because the cache's disk is full.
type recorder struct {
	mu     sync.Mutex
	out    io.Writer
	file   *os.File
	failed bool
}

func (w *recorder) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.failed {
		if _, err := w.file.Write(p); err != nil {
			w.failed = true
		}
	}
	return w.out.Write(p)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// cached runs t through its replay: the recording when one is fresh, otherwise
// the task, recorded.
func (r *Runner) cached(ctx context.Context, t *chorefile.Task, scope *tmpl.Scope) error {
	if r.CacheDir == "" || r.DryRun || outputDiscarded(ctx) {
		return r.runTaskBody(ctx, t, scope)
	}
	dir := filepath.Join(r.CacheDir, "chore", "replay", hashOf(t.File.Path))
	entry := filepath.Join(dir, replayKey(t, scope)+".replay")

	if !r.Force {
		if body, ok := r.readReplay(entry); ok {
			_, err := r.output(ctx).Write(body)
			return err
		}
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return r.runTaskBody(ctx, t, scope)
	}
	tmp, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return r.runTaskBody(ctx, t, scope)
	}
	defer os.Remove(tmp.Name())
	rec := &recorder{out: r.output(ctx), file: tmp}

	runErr := r.runTaskBody(context.WithValue(ctx, recordKey{}, rec), t, scope)
	if err := tmp.Close(); err != nil {
		rec.failed = true
	}
	if runErr == nil && !rec.failed {
		r.keepReplay(dir, entry, tmp.Name(), t)
	}
	return runErr
}

// readReplay returns an entry's recorded bytes when it exists and is fresh.
// Anything else — missing, unreadable, malformed, expired, or written in what
// is now the future by a clock that moved — is no entry.
func (r *Runner) readReplay(entry string) ([]byte, bool) {
	data, err := os.ReadFile(entry)
	if err != nil {
		return nil, false
	}
	head, body, ok := bytes.Cut(data, []byte("\n"))
	if !ok {
		return nil, false
	}
	var meta replayMeta
	if json.Unmarshal(head, &meta) != nil {
		return nil, false
	}
	now := r.now()
	if now.Before(meta.Created) || !now.Before(meta.Expires) {
		return nil, false
	}
	return body, true
}

// keepReplay writes the entry — its metadata line, then the recording — and
// renames it into place, then removes the chorefile's expired entries. Every
// failure here is silent: the task has already run and answered, and an entry
// that could not be kept is only a run that will happen again.
func (r *Runner) keepReplay(dir, entry, recording string, t *chorefile.Task) {
	now := r.now()
	head, err := json.Marshal(replayMeta{
		Task:    t.Name,
		Created: now,
		Expires: now.Add(time.Duration(t.Cache)),
		Chore:   r.ChoreVersion,
	})
	if err != nil {
		return
	}
	body, err := os.ReadFile(recording)
	if err != nil {
		return
	}
	out, err := os.CreateTemp(dir, ".entry-*")
	if err != nil {
		return
	}
	defer os.Remove(out.Name())
	w := bufio.NewWriter(out)
	w.Write(head)
	w.WriteByte('\n')
	w.Write(body)
	if w.Flush() != nil || out.Close() != nil {
		return
	}
	if os.Rename(out.Name(), entry) != nil {
		return
	}
	r.pruneReplays(dir, entry)
}

// pruneReplays removes the expired entries in dir, except the one just written.
func (r *Runner) pruneReplays(dir, keep string) {
	names, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, n := range names {
		path := filepath.Join(dir, n.Name())
		if n.IsDir() || path == keep || !strings.HasSuffix(n.Name(), ".replay") {
			continue
		}
		if _, fresh := r.readReplay(path); !fresh {
			os.Remove(path)
		}
	}
}

// replayKey is the hash of what makes two runs of t the same question.
func replayKey(t *chorefile.Task, scope *tmpl.Scope) string {
	vars := scope.Declared()
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(t.Name)
	for _, k := range names {
		b.WriteString("\x1f")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(vars[k])
	}
	// The steps a replay stands in for. Plain data — strings, bools, maps of
	// them — so encoding/json writes the same bytes for the same steps, maps in
	// sorted key order.
	steps, _ := json.Marshal(struct {
		Cmds   chorefile.Cmds
		Deps   chorefile.Deps
		Before chorefile.Cmds
	}{t.Cmds, t.Deps, t.Before})
	b.WriteString("\x1e")
	b.Write(steps)
	return hashOf(b.String())
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}
