// Package fingerprint decides whether a task can be skipped.
//
// Two independent mechanisms, both taken from Task because rest-mail's
// Taskfiles use both: `status:` (shell commands whose exit codes answer the
// question directly) and `sources:`/`generates:` (content checksums over the
// files a task reads and writes).
//
// The package deliberately declares its own tiny interfaces (Renderer, Runner,
// Capturer) instead of importing internal/tmpl and internal/shell. The concrete
// types satisfy them structurally, so the dependency arrow can point inward
// without a compile-time edge — which also means the whole package is testable
// with fakes and no shell.
//
// Two design choices worth stating up front:
//
//  1. **`status:` wins over `sources:`/`generates:` when both are present.**
//     `status:` is a direct assertion by the Taskfile author about the world
//     ("the container is already running"), while a checksum is an inference
//     from the file system. The explicit signal beats the inferred one. It is
//     also the cheaper check — no directory walking — and it short-circuits.
//  2. **Checksums are content-based, never timestamp-based.** `touch`ing a
//     source must not invalidate anything: rebuilding because a mtime moved is
//     the single most common way a task runner wastes a developer's time, and
//     checkouts, rsync and Docker bind mounts all move mtimes for free.
package fingerprint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

const (
	// DefaultCacheDir is used when the caller passes an empty cacheDir. A
	// relative cache dir is resolved against the task's directory, so the
	// default lands at <dir>/.chore.
	DefaultCacheDir = ".chore"

	// fingerprintsDir keeps checksums in their own subdirectory so the cache
	// dir stays available for whatever else needs a scratch space later.
	fingerprintsDir = "fingerprints"

	// formatVersion is baked into both the file and the hash input. Bumping it
	// invalidates every stored fingerprint, which is what we want if the hash
	// definition ever changes: a stale hash that happens to still compare equal
	// would skip a task that must run.
	formatVersion = 1

	// hashPrefix seeds every digest so an empty source set still produces a
	// version-specific value rather than the digest of nothing.
	hashPrefix = "chore-fingerprint-v1\n"
)

// excludedDirs are never descended into while expanding a glob. `.git` and
// `node_modules` hold tens of thousands of files that no task meaningfully
// depends on, and walking them turns a cheap check into a slow one. The cache
// dir is excluded too, otherwise writing a fingerprint would change the very
// checksum that fingerprint records.
//
// The exclusion applies to traversal only. A source listed as a plain path
// (no glob metacharacters) is honoured wherever it lives, because naming a file
// outright is unambiguous intent.
var excludedDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
}

// Capturer runs a script and returns its stdout. `status:` only needs an exit
// code, so this package does not require it — it is declared because callers
// hold shells that expose capture-only helpers, and RunnerFromCapturer adapts
// one into the Runner this package does need.
type Capturer interface {
	Capture(ctx context.Context, script string) (string, error)
}

// Runner runs a script and reports failure. A non-zero exit must come back as
// an error implementing ExitCoder; see the ExitCoder docs for why.
type Runner interface {
	Run(ctx context.Context, script string) error
}

// Renderer expands templates in a string ({{.ROOT_DIR}} and friends). It is
// satisfied by *tmpl.Scope.
type Renderer interface {
	Render(text string) (string, error)
}

// ExitCoder is an error that carries a command's exit status.
//
// This is the line between "the check said no" and "the check could not run".
// A `status:` command exiting non-zero is the normal request to run the task,
// not a failure; a shell that cannot start at all is a real error the user must
// see. The only way to tell them apart is for the shell to report exit codes as
// a distinguishable error type, which internal/shell does. *exec.ExitError
// satisfies this interface already.
//
// Anything that is not an ExitCoder (and not a cancelled context) is treated as
// an execution failure and propagated.
type ExitCoder interface {
	error
	ExitCode() int
}

// RunnerFromCapturer turns a capture-only shell into a Runner by discarding the
// captured output. Useful for callers whose shell handle predates this package.
func RunnerFromCapturer(c Capturer) Runner {
	return capturerRunner{c}
}

type capturerRunner struct{ c Capturer }

func (r capturerRunner) Run(ctx context.Context, script string) error {
	_, err := r.c.Capture(ctx, script)
	return err
}

// UpToDate reports whether t can be skipped.
//
// dir is the directory the task runs in; every glob and every recorded path is
// relative to it. cacheDir is where fingerprints live ("" means
// <dir>/DefaultCacheDir). A task with no status/sources/generates is never up
// to date: with no evidence, the only safe answer is "run it".
//
// A false return is not an error. The error return is reserved for problems
// that stop the check from producing an answer at all — a template that will
// not render, a shell that will not start, an unreadable source file.
func UpToDate(ctx context.Context, t *chorefile.Task, r Renderer, sh Runner, dir, cacheDir string) (bool, error) {
	if t == nil {
		return false, nil
	}
	r = orIdentity(r)

	// status: wins. See the package doc for why.
	if len(t.Status) > 0 {
		return statusUpToDate(ctx, t, r, sh)
	}
	if len(t.Sources) == 0 && len(t.Generates) == 0 {
		return false, nil
	}
	return checksumUpToDate(t, r, dir, cacheDir)
}

// statusUpToDate runs every status command until one fails. Short-circuiting is
// safe because a status command that changes the world is already a bug, and it
// keeps the common "not up to date" path cheap.
func statusUpToDate(ctx context.Context, t *chorefile.Task, r Renderer, sh Runner) (bool, error) {
	if sh == nil {
		return false, fmt.Errorf("task %q: status check needs a shell", t.Name)
	}
	for _, raw := range t.Status {
		script, err := r.Render(raw)
		if err != nil {
			return false, fmt.Errorf("task %q: status %q: %w", t.Name, raw, err)
		}
		if strings.TrimSpace(script) == "" {
			// An empty script after rendering would be a no-op that always
			// succeeds, which would silently make the task look up to date.
			// Almost certainly a variable that did not resolve.
			return false, fmt.Errorf("task %q: status %q rendered empty", t.Name, raw)
		}
		err = sh.Run(ctx, script)
		if err == nil {
			continue
		}
		// A cancelled run is being torn down; do not report that as "needs to
		// run", the answer is simply unavailable.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("task %q: status %q: %w", t.Name, raw, ctxErr)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, fmt.Errorf("task %q: status %q: %w", t.Name, raw, err)
		}
		var ec ExitCoder
		if errors.As(err, &ec) {
			return false, nil // the check said "not done" — normal
		}
		return false, fmt.Errorf("task %q: status %q: %w", t.Name, raw, err)
	}
	return true, nil
}

// checksumUpToDate compares the current source content against the last
// recorded run and verifies the outputs are still there.
func checksumUpToDate(t *chorefile.Task, r Renderer, dir, cacheDir string) (bool, error) {
	cache := resolveCacheDir(dir, cacheDir)

	// Outputs first: it is the cheapest way to say no, and a missing output
	// makes the source checksum irrelevant.
	gen, missing, err := expand(dir, t.Generates, r, cache)
	if err != nil {
		return false, fmt.Errorf("task %q: generates: %w", t.Name, err)
	}
	if len(missing) > 0 {
		return false, nil
	}

	srcs, _, err := expand(dir, t.Sources, r, cache)
	if err != nil {
		return false, fmt.Errorf("task %q: sources: %w", t.Name, err)
	}
	// A sources pattern matching nothing is not an error: a generated tree can
	// legitimately be empty, and the recorded fingerprint will be empty too.
	cur, err := hashFiles(srcs)
	if err != nil {
		return false, fmt.Errorf("task %q: sources: %w", t.Name, err)
	}

	prev, ok := load(fingerprintPath(cache, t.Name))
	if !ok {
		// Missing, unreadable or corrupt: no usable record of a previous run,
		// so the task must run. Refusing to run because a cache file got
		// truncated would be the wrong failure mode — the cache is derived
		// data and is always safe to discard.
		return false, nil
	}
	if prev.Version != formatVersion || prev.Hash != cur.Hash {
		return false, nil
	}

	// Every file the last successful run produced must still exist. The
	// `generates` check above only proves each *pattern* still matches
	// something; with `generates: [bin/*]` and two binaries, deleting one would
	// otherwise go unnoticed.
	for _, rel := range prev.Generates {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			return false, nil
		}
	}
	_ = gen // matched purely to prove the patterns still resolve
	return true, nil
}

// Save records the current source checksum after a successful run. Call it only
// on success: a fingerprint written after a failed run would skip the retry.
//
// Save takes no Renderer, so it uses the patterns verbatim. If sources or
// generates contain templates, use SaveWith with the same Renderer that was
// passed to UpToDate — otherwise the two would hash different file sets and the
// task could never be up to date.
func Save(t *chorefile.Task, dir, cacheDir string) error {
	return SaveWith(t, nil, dir, cacheDir)
}

// SaveWith is Save with template expansion for sources/generates patterns.
func SaveWith(t *chorefile.Task, r Renderer, dir, cacheDir string) error {
	if t == nil || (len(t.Sources) == 0 && len(t.Generates) == 0) {
		return nil // nothing to remember
	}
	r = orIdentity(r)
	cache := resolveCacheDir(dir, cacheDir)

	srcs, _, err := expand(dir, t.Sources, r, cache)
	if err != nil {
		return fmt.Errorf("task %q: sources: %w", t.Name, err)
	}
	fp, err := hashFiles(srcs)
	if err != nil {
		return fmt.Errorf("task %q: sources: %w", t.Name, err)
	}
	gen, _, err := expand(dir, t.Generates, r, cache)
	if err != nil {
		return fmt.Errorf("task %q: generates: %w", t.Name, err)
	}
	fp.Version = formatVersion
	fp.Task = t.Name
	fp.UpdatedAt = time.Now().UTC()
	for _, g := range gen {
		fp.Generates = append(fp.Generates, g.rel)
	}
	return store(fingerprintPath(cache, t.Name), fp)
}

// Path is where t's fingerprint is stored, so callers (--force, a cache clean
// command) can find or remove it without duplicating the naming rules.
func Path(t *chorefile.Task, dir, cacheDir string) string {
	name := ""
	if t != nil {
		name = t.Name
	}
	return fingerprintPath(resolveCacheDir(dir, cacheDir), name)
}

// ---------- fingerprint file ----------

// stored is the on-disk fingerprint. Per-file hashes are kept even though only
// Hash is compared: when a task unexpectedly reruns, the first question is
// always "which file changed", and this file is the only place that can answer.
type stored struct {
	Version   int               `json:"version"`
	Task      string            `json:"task"`
	Hash      string            `json:"hash"`
	Sources   map[string]string `json:"sources,omitempty"`
	Generates []string          `json:"generates,omitempty"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// hashFiles digests the sorted (path, content-hash) pairs. Paths are part of
// the input because a rename with identical content is still a change, and the
// order is fixed so the digest does not depend on directory iteration order.
func hashFiles(files []fileRef) (*stored, error) {
	fp := &stored{Sources: make(map[string]string, len(files))}
	h := sha256.New()
	h.Write([]byte(hashPrefix))
	for _, f := range files {
		sum, err := hashFile(f.abs)
		if err != nil {
			return nil, err
		}
		fp.Sources[f.rel] = sum
		h.Write([]byte(f.rel))
		h.Write([]byte{0})
		h.Write([]byte(sum))
		h.Write([]byte{'\n'})
	}
	fp.Hash = hex.EncodeToString(h.Sum(nil))
	return fp, nil
}

func hashFile(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// load reads a fingerprint. The bool is "usable", not "found": a corrupt file
// is reported the same as a missing one.
func load(path string) (*stored, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var fp stored
	if err := json.Unmarshal(b, &fp); err != nil {
		return nil, false
	}
	if fp.Hash == "" {
		return nil, false
	}
	return &fp, true
}

// store writes the fingerprint via a temp file and a rename, so a crash or a
// concurrent reader never sees half a file — a truncated fingerprint would be
// silently discarded and cost a rebuild.
func store(path string, fp *stored) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(fp, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func resolveCacheDir(dir, cacheDir string) string {
	if cacheDir == "" {
		cacheDir = DefaultCacheDir
	}
	if !filepath.IsAbs(cacheDir) {
		cacheDir = filepath.Join(dir, cacheDir)
	}
	return filepath.Clean(cacheDir)
}

func fingerprintPath(cache, taskName string) string {
	return filepath.Join(cache, fingerprintsDir, sanitise(taskName)+".json")
}

// sanitise makes a task name safe as a filename while keeping it recognisable.
// The short digest suffix is what makes it collision-free: namespaced names are
// `ns:task`, and mapping ':' to '_' would otherwise let `ns:task` and `ns_task`
// share one fingerprint.
func sanitise(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	safe := b.String()
	if len(safe) > 100 {
		safe = safe[:100]
	}
	sum := sha256.Sum256([]byte(name))
	if safe == "" {
		safe = "task"
	}
	return safe + "-" + hex.EncodeToString(sum[:4])
}

// ---------- globbing ----------

// fileRef is one matched file: the absolute path to read, and the path recorded
// in the fingerprint. The recorded path is relative to the task's directory so
// a fingerprint survives the checkout being moved.
type fileRef struct {
	abs string
	rel string // slash-separated
}

// expand resolves patterns to files. It returns the matches (sorted, deduped)
// and the patterns that matched nothing — the caller decides whether an
// unmatched pattern means "not up to date" (generates) or is fine (sources).
//
// `**` matches zero or more path segments; `*`, `?` and `[…]` match within one
// segment, via path.Match. Implemented here rather than pulled in as a
// dependency: it is thirty lines, and the exclusion rules have to live inside
// the walk anyway.
func expand(dir string, patterns []string, r Renderer, cacheDir string) (matched []fileRef, unmatched []string, err error) {
	seen := make(map[string]bool)
	for _, raw := range patterns {
		pattern, err := r.Render(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("pattern %q: %w", raw, err)
		}
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		found, err := expandOne(dir, pattern, cacheDir)
		if err != nil {
			return nil, nil, err
		}
		if len(found) == 0 {
			unmatched = append(unmatched, pattern)
			continue
		}
		for _, f := range found {
			if seen[f.abs] {
				continue // two patterns can match the same file
			}
			seen[f.abs] = true
			matched = append(matched, f)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].rel < matched[j].rel })
	return matched, unmatched, nil
}

func expandOne(dir, pattern string, cacheDir string) ([]fileRef, error) {
	base := dir
	rel := filepath.ToSlash(pattern)
	if filepath.IsAbs(pattern) {
		base = string(filepath.Separator)
		rel = strings.TrimPrefix(rel, "/")
	}
	segs := splitSegments(rel)
	if len(segs) == 0 {
		return nil, nil
	}
	if err := validate(segs, pattern); err != nil {
		return nil, err
	}

	// A pattern with no metacharacters is a named file: stat it, do not walk,
	// and do not apply the directory exclusions (naming a path is intent).
	if !hasMeta(segs) {
		abs := filepath.Join(base, filepath.FromSlash(rel))
		st, err := os.Stat(abs)
		if err != nil || !st.Mode().IsRegular() {
			return nil, nil
		}
		return []fileRef{newRef(dir, abs)}, nil
	}

	// Walk from the deepest fixed prefix so `build/**/*.o` does not scan the
	// whole tree.
	prefix := 0
	for prefix < len(segs) && !segMeta(segs[prefix]) && segs[prefix] != "**" {
		prefix++
	}
	root := filepath.Join(base, filepath.FromSlash(strings.Join(segs[:prefix], "/")))

	var out []fileRef
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A vanished entry or an unreadable directory must not fail the
			// whole check; the rest of the tree is still informative.
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if excludedDirs[d.Name()] || sameDir(p, cacheDir) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks to nowhere, sockets, fifos: nothing to hash
		}
		entryRel, err := filepath.Rel(base, p)
		if err != nil {
			return nil
		}
		if matchSegments(segs, splitSegments(filepath.ToSlash(entryRel))) {
			out = append(out, newRef(dir, p))
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // the fixed prefix does not exist yet
		}
		return nil, err
	}
	return out, nil
}

func newRef(dir, abs string) fileRef {
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		rel = abs // outside dir and not expressible relative to it
	}
	return fileRef{abs: abs, rel: filepath.ToSlash(rel)}
}

// sameDir compares two paths for identity without touching the file system.
// Both sides are produced from the same caller-supplied dir, so lexical
// comparison after Abs is enough — and it stays correct when the dir does not
// exist yet.
func sameDir(a, b string) bool {
	if b == "" {
		return false
	}
	aa, err := filepath.Abs(a)
	if err != nil {
		aa = filepath.Clean(a)
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		bb = filepath.Clean(b)
	}
	return aa == bb
}

func splitSegments(p string) []string {
	p = path.Clean(p)
	if p == "." || p == "/" || p == "" {
		return nil
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// validate rejects malformed patterns up front so matching itself cannot fail
// halfway through a walk, when the error would be attributed to a random file.
func validate(segs []string, pattern string) error {
	for _, s := range segs {
		if s == "**" {
			continue
		}
		if _, err := path.Match(s, "x"); err != nil {
			return fmt.Errorf("bad pattern %q: %w", pattern, err)
		}
	}
	return nil
}

func hasMeta(segs []string) bool {
	for _, s := range segs {
		if s == "**" || segMeta(s) {
			return true
		}
	}
	return false
}

func segMeta(s string) bool { return strings.ContainsAny(s, `*?[\`) }

// matchSegments matches a segmented pattern against a segmented path. `**`
// consumes zero or more segments, which is what makes `**/*.go` match a file at
// the root as well as one nested five deep.
func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Trailing `**` matches whatever is left, including nothing.
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// ---------- misc ----------

// identity renders nothing, for callers with no variable scope (Save) and as a
// nil guard so a missing Renderer cannot panic mid-check.
type identity struct{}

func (identity) Render(text string) (string, error) { return text, nil }

func orIdentity(r Renderer) Renderer {
	if r == nil {
		return identity{}
	}
	return r
}
