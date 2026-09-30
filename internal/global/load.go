// Package global is where machine-wide tasks LIVE, and how an ssh route is
// travelled. It is not a second kind of task.
//
// A file in ~/.config/chore/global.d is an ordinary taskfile, loaded by
// internal/loader and run by internal/run like any other; the only thing that
// sets it apart is that its tasks are addressed as `global:<file>:<task>`. What
// is here is the part that genuinely belongs to no project: finding that
// directory, loading every file in it, attaching them to whatever else is
// running, and the ssh transport that any task's `route:` uses.
package global

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/loader"
)

// chore:manual global
// title: Global tasks
// summary: machine-wide tasks, reachable from any directory as global:<file>:<task>
// aliases: globals global-d
// order: 10
//
// # Global tasks
//
// Tasks that belong to a MACHINE rather than to a project, kept in
// `~/.config/chore/global.d/*.yaml` and reachable from any directory, with or
// without a `chores.yml` in sight.
//
// ```
// chore global:                       the files installed here
// chore global:ssh:                   the tasks in global.d/ssh.yaml
// chore global:ssh:unlock             run one
// ```
//
// **A global file is an ordinary taskfile.** Same schema, same arguments, same
// everything: `args:`, `vars:`, `cmds:`, hooks, `--help`, `--dry`, `--` all mean
// what they mean in a `chores.yml`. The only difference is where it lives, and
// that its tasks are addressed through the fixed `global:<file>:` prefix — the
// filename, without `.yaml`, is the namespace:
//
// ```yaml
// # ~/.config/chore/global.d/agents.yaml
// tasks:
//   limit:
//     desc: the rate limit left, as a table or as JSON
//     args:
//       - account
//       - { name: json, type: bool }
//     vars: { account: all }
//     cmd: agent-limits {{.ACCOUNT}} {{if .JSON}}--json{{end}}
// ```
//
// ```
// chore global:agents:limit                 # account=all
// chore global:agents:limit work --json
// chore global:agents:limit --help
// ```
//
// `$XDG_CONFIG_HOME` is honoured when set, and `~/.config` is the fallback —
// which matters because the variable is unset on macOS by default, and these
// files are meant to arrive on both by the same dotfiles repository.
//
// ## Loaded on every run
//
// Every file in global.d is loaded every time chore runs, so anything in one —
// a task, a route, a predicate — can be named from anywhere as
// `global:<file>:<name>`: from another global file, or from a project.
//
// ```yaml
// # a project's chores.yml
// tasks:
//   deploy:
//     deps: [global:ssh:unlock]
//     route: global:homelab:pi
//     cmd: ./deploy.sh
// ```
//
// A bare name is always in the file it is written in. A global file is loaded
// with the same strictness as any other, so one that is present and wrong is an
// error naming it rather than a namespace that silently stopped existing.
//
// ## What differs, and why
//
// - **`global:` is mandatory.** `chore ssh:unlock` is a task in the current
//   project; `chore global:ssh:unlock` is the machine's. The prefix makes the
//   call site readable without knowing what is installed, and a project cannot
//   shadow a machine's task — a project task may not be named `global:…`.
// - **A global task runs in the directory you ran chore from**, not beside its
//   file: it belongs to the machine, and `chore global:tools:fmt` should format
//   what you are standing in. `{{.TASKFILE_DIR}}` is still its own directory.
// - **Its environment is its own.** A global file is the root of its own tree:
//   its `dotenv:`, `env:` and `vars:` apply to its tasks, and a project's do not
//   leak into it when a project task depends on one.
// - **`chore --list` does not include them.** They are listed by `chore global:`.
// - **`name:` is not needed.** A file written when it was still accepted loads as
//   long as it matches the filename.

// Dir returns the directory global files are read from.
//
// $XDG_CONFIG_HOME when set, ~/.config otherwise. The fallback is not
// decoration: the variable is unset on macOS by default, and these files are
// meant to arrive on macOS and Linux from one dotfiles repository, at one path,
// with no per-machine setup.
func Dir() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "chore", "global.d"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to read global tasks from: %w", err)
	}
	return filepath.Join(home, ".config", "chore", "global.d"), nil
}

// Set is every global file installed on this machine, each loaded as the
// ordinary taskfile it is.
type Set struct {
	Dir string
	// Namespaces are keyed by filename without its extension: the word between
	// `global:` and the task.
	Namespaces map[string]*chorefile.Project
}

// Load reads every file in dir. A missing directory is not an error — having
// no global tasks is the ordinary state of a machine — but a file that is
// present and wrong IS one, because a namespace that silently failed to load is
// a command that has stopped existing without saying so.
func Load(dir string) (*Set, error) {
	set := &Set{Dir: dir, Namespaces: map[string]*chorefile.Project{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return set, nil
		}
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !isNamespaceFile(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		ns := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if other, dup := set.Namespaces[ns]; dup {
			// ssh.yaml and ssh.yml would both be `global:ssh:`, and which one won
			// would depend on directory order. Name both.
			return nil, fmt.Errorf("two files are both `global:%s:`: %s and %s", ns, other.Root.Path, path)
		}
		p, err := loader.LoadGlobal(path, ns)
		if err != nil {
			return nil, err
		}
		set.Namespaces[ns] = p
	}
	// Check the set on its own, so a reference from one global file to another
	// that does not exist is reported whatever is being run.
	if _, err := set.combined(nil); err != nil {
		return nil, err
	}
	return set, nil
}

// isNamespaceFile accepts .yaml and .yml. One spelling would be tidier and the
// other one would fail silently, which is the trade chore never takes.
func isNamespaceFile(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

// Attach adds every global task, route and predicate to a project, so its own
// tasks can name them as `global:<ns>:<name>`, and checks the references that
// cross between the two now that both are in.
func (s *Set) Attach(p *chorefile.Project) error {
	_, err := s.combined(p)
	return err
}

// Project returns a project rooted at one global file, with every other global
// file attached: what `chore global:<ns>:<task>` runs. The file is the root in
// every sense a project's chores.yml is — its `lifecycle:`, its `dotenv:`, its
// `chore_min_version:`.
func (s *Set) Project(ns string) (*chorefile.Project, error) {
	root, ok := s.Namespaces[ns]
	if !ok {
		return nil, fmt.Errorf("no global:%s: in %s%s", ns, s.Dir, s.suggest())
	}
	p := &chorefile.Project{Root: root.Root, RootDir: root.RootDir,
		Tasks: map[string]*chorefile.Task{}, Files: map[string]*chorefile.File{}}
	return s.combined(p)
}

// combined merges every namespace into p (a fresh project when p is nil) and
// checks the result strictly. Keys cannot collide: every global key starts with
// `global:<ns>:`, and a project is refused a task under that prefix.
func (s *Set) combined(p *chorefile.Project) (*chorefile.Project, error) {
	if p == nil {
		p = &chorefile.Project{Tasks: map[string]*chorefile.Task{}, Files: map[string]*chorefile.File{}}
	}
	if p.Files == nil {
		p.Files = map[string]*chorefile.File{}
	}
	for _, ns := range slices.Sorted(maps.Keys(s.Namespaces)) {
		for k, t := range s.Namespaces[ns].Tasks {
			p.Tasks[k] = t
		}
		for k, f := range s.Namespaces[ns].Files {
			p.Files[k] = f
		}
	}
	if err := loader.Check(p, true); err != nil {
		return nil, err
	}
	return p, nil
}

// Tasks returns the visible tasks of one namespace, sorted by address.
func (s *Set) Tasks(ns string) ([]*chorefile.Task, error) {
	p, ok := s.Namespaces[ns]
	if !ok {
		return nil, fmt.Errorf("no global:%s: in %s%s", ns, s.Dir, s.suggest())
	}
	var out []*chorefile.Task
	for _, name := range slices.Sorted(maps.Keys(p.Tasks)) {
		if t := p.Tasks[name]; !t.Internal && t.Name == name {
			out = append(out, t)
		}
	}
	return out, nil
}

// ListNamespaces answers `chore global:` — what is installed on this machine.
func (s *Set) ListNamespaces(w io.Writer) {
	if len(s.Namespaces) == 0 {
		fmt.Fprintf(w, "no global tasks in %s\n", s.Dir)
		fmt.Fprintf(w, "\nEach file there is a taskfile, and its name is what follows `global:`.\n")
		return
	}
	fmt.Fprintf(w, "global tasks in %s\n\n", s.Dir)
	for _, ns := range slices.Sorted(maps.Keys(s.Namespaces)) {
		p := s.Namespaces[ns]
		tasks, _ := s.Tasks(ns)
		fmt.Fprintf(w, "  %-20s %d task(s), %d route(s)\n", chorefile.GlobalPrefix+ns+":", len(tasks), len(p.Root.Routes))
	}
	fmt.Fprintf(w, "\nlist one with: chore global:%s:\n", slices.Sorted(maps.Keys(s.Namespaces))[0])
}

func (s *Set) suggest() string {
	names := slices.Sorted(maps.Keys(s.Namespaces))
	if len(names) == 0 {
		return " — nothing is installed there"
	}
	return " — installed: " + strings.Join(names, ", ")
}
