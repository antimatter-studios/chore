// Package loader turns a Taskfile on disk into a chorefile.Project: it reads the
// root file, resolves `includes` depth-first, and flattens every task into one
// namespaced map (`postgres:up`, `a:b:migrate`).
//
// The rule that matters here is variable flow. Task flattens an included file's
// variables into the parent's namespace, so two includes silently overwrite each
// other — the reason rest-mail cannot include reference-mailserver at all. In
// this loader there is exactly ONE variable flow between files: an include's
// `vars:` block is merged down onto the included file's own vars (the include
// wins). Nothing flows upward from a child, and nothing flows sideways between
// siblings; two includes of the same path each decode their own File, so each
// gets its own copy of the vars.
//
// Everything the loader discovers is written back into the schema types
// (Name, File, Path, Dir, RootDir) — no side tables — so later packages only
// need the *chorefile.Project.
package loader

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// DefaultFilename is the file looked for when a path (the root argument, an
// include's `taskfile:`, or its `dir:`) names a directory rather than a file.
const DefaultFilename = "chores.yml"

// Filenames are the names an include may resolve to when it names a directory,
// in order. Taskfile.yml is accepted last so an included peer repository that
// still ships one keeps working.
var Filenames = []string{"chores.yml", "chores.yaml", "Taskfile.yml", "Taskfile.yaml"}

// Load reads the Taskfile at path — a file, or a directory holding
// Taskfile.yml — and every file it includes, and returns the flattened project.
func Load(path string) (*chorefile.Project, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("taskfile %s: %w", path, err)
	}
	rootPath, err := locate(abs)
	if err != nil {
		return nil, fmt.Errorf("taskfile %s: %w", abs, err)
	}

	entries, err := load(request{path: rootPath})
	if err != nil {
		return nil, err
	}
	tasks, err := register(entries)
	if err != nil {
		return nil, err
	}

	root := entries[0].file
	return &chorefile.Project{
		Root:  root,
		Tasks: tasks,
		// RootDir is the directory physically holding the root file, even if the
		// root file were reached through a symlinked path: it is {{.ROOT_DIR}}
		// and the default working directory for every task.
		RootDir: filepath.Dir(root.Path),
	}, nil
}

// entry is one loaded file plus the namespace its tasks live under ("" for the
// root, "postgres" for an include, "a:b" for a nested one). The slice of
// entries produced by load() is in deterministic order — a file before its
// includes, includes in sorted key order — so duplicate-name errors are stable.
type entry struct {
	prefix string
	file   *chorefile.File
}

// request is one file to read, as described by the file that includes it.
type request struct {
	path   string                   // absolute path of the file to read
	prefix string                   // namespace for its tasks
	vars   map[string]chorefile.Var // the include's `vars:`, merged onto the file
	dir    string                   // the include's `dir:`, absolute, or ""
	chain  []string                 // absolute paths of the ancestors, for cycle detection
}

func load(req request) ([]entry, error) {
	// A cycle is checked before reading, so A→B→A errors instead of recursing
	// until the stack dies.
	if i := slices.Index(req.chain, req.path); i >= 0 {
		cycle := append(slices.Clone(req.chain[i:]), req.path)
		return nil, fmt.Errorf("include cycle: %s", strings.Join(cycle, " -> "))
	}

	data, err := os.ReadFile(req.path)
	if err != nil {
		return nil, fmt.Errorf("read taskfile: %w", err)
	}
	f, err := chorefile.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", req.path, err)
	}

	f.Path = req.path
	f.Dir = filepath.Dir(req.path)
	if req.dir != "" {
		// An include's `dir:` is the working directory mapped in for this file's
		// tasks, so it replaces Dir. filepath.Dir(f.Path) still gives the
		// directory physically holding the file, which is how the runner tells
		// the two apart (f.Dir != filepath.Dir(f.Path) means an include mapped a
		// directory) and is what the loader itself uses to resolve relative
		// include paths below.
		f.Dir = req.dir
	}
	f.Vars = mergeVars(f.Vars, req.vars)

	entries := []entry{{prefix: req.prefix, file: f}}
	chain := append(slices.Clone(req.chain), req.path)

	// Sorted so that any error — and any later output derived from Tasks — is
	// the same on every run and on every machine.
	for _, name := range slices.Sorted(maps.Keys(f.Includes)) {
		inc := f.Includes[name]
		if inc == nil { // `includes: {ns:}` — nothing to pull in
			continue
		}
		child, skip, err := childRequest(f, name, inc, req.prefix, chain)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		sub, err := load(child)
		if err != nil {
			return nil, err
		}
		entries = append(entries, sub...)
	}
	return entries, nil
}

// childRequest resolves one include against the file that declares it. skip is
// true when the include is `optional:` and its file is absent.
func childRequest(f *chorefile.File, name string, inc *chorefile.Include, prefix string, chain []string) (req request, skip bool, err error) {
	base := inc.Taskfile
	if base == "" {
		// `dir:` alone means "the Taskfile.yml in that directory".
		base = inc.Dir
	}
	if base == "" {
		return request{}, false, fmt.Errorf("%s: include %q: neither taskfile nor dir is set", f.Path, name)
	}

	// Relative to the INCLUDING file's own directory — not the process working
	// directory, and not the root file's directory, so a subtree of Taskfiles
	// can be moved or included from anywhere.
	from := filepath.Dir(f.Path)
	candidate := resolveAgainst(from, base)

	path, err := locate(candidate)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && inc.Optional {
			return request{}, true, nil
		}
		return request{}, false, fmt.Errorf("%s: include %q: %s: %w", f.Path, name, candidate, err)
	}

	dir := ""
	if inc.Dir != "" {
		dir = resolveAgainst(from, inc.Dir)
	}
	if !inc.Flatten {
		// flatten keeps the included tasks' bare names, so the include
		// contributes no namespace segment at all.
		prefix = qualify(prefix, name)
	}
	return request{path: path, prefix: prefix, vars: inc.Vars, dir: dir, chain: chain}, false, nil
}

// register flattens the loaded files into Project.Tasks and fills in the fields
// the loader owns (Name, File). It takes the whole entry list because aliases
// are resolved in a second pass: an alias must never shadow a real task just
// because its file happened to be included first.
func register(entries []entry) (map[string]*chorefile.Task, error) {
	tasks := make(map[string]*chorefile.Task)

	for _, e := range entries {
		for _, name := range slices.Sorted(maps.Keys(e.file.Tasks)) {
			t := e.file.Tasks[name]
			if t == nil {
				// `build:` with an empty body is legal YAML and a no-op task;
				// materialise it so nothing downstream has to nil-check.
				t = &chorefile.Task{}
				e.file.Tasks[name] = t
			}
			key := qualify(e.prefix, name)
			if prev, dup := tasks[key]; dup {
				return nil, fmt.Errorf("duplicate task %q: defined in %s and in %s", key, prev.File.Path, e.file.Path)
			}
			t.Name = key
			t.File = e.file
			tasks[key] = t
		}
	}

	for _, e := range entries {
		for _, name := range slices.Sorted(maps.Keys(e.file.Tasks)) {
			t := e.file.Tasks[name]
			for _, alias := range t.Aliases {
				key := qualify(e.prefix, alias)
				if prev, dup := tasks[key]; dup {
					return nil, fmt.Errorf("alias %q of task %q in %s collides with task %q in %s",
						key, t.Name, e.file.Path, prev.Name, prev.File.Path)
				}
				tasks[key] = t
			}
		}
	}
	return tasks, nil
}

// mergeVars is the only variable flow between files: the include's vars win
// over the included file's own defaults, which are therefore just that —
// defaults for when the parent maps nothing in.
func mergeVars(own, incoming map[string]chorefile.Var) map[string]chorefile.Var {
	if len(incoming) == 0 {
		return own
	}
	out := make(map[string]chorefile.Var, len(own)+len(incoming))
	maps.Copy(out, own)
	maps.Copy(out, incoming)
	return out
}

// locate turns a path into the Taskfile to read: a directory means the
// conventional Taskfile.yml inside it. A wrapped fs.ErrNotExist is returned
// when nothing is there, which is what `optional:` keys off — any other error
// (a permission problem, say) is reported even for an optional include.
func locate(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return path, nil
	}
	// A peer repository included by directory may still ship a Taskfile.yml, so
	// try the chore names first and fall back rather than refusing to load it.
	var firstErr error
	for _, name := range Filenames {
		inner := filepath.Join(path, name)
		if _, err := os.Stat(inner); err == nil {
			return inner, nil
		} else if firstErr == nil {
			firstErr = err
		}
	}
	return "", firstErr
}

func resolveAgainst(dir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(dir, path)
}

func qualify(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + ":" + name
}
