package global

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPort is what a hop with no `port:` dials, as ssh does.
const DefaultPort = 22

// Dir returns the directory global namespaces are read from.
//
// $XDG_CONFIG_HOME when set, ~/.config otherwise. The fallback is not
// decoration: the variable is unset on macOS by default, and these files are
// meant to arrive on macOS and Linux from one dotfiles repository, at one path,
// with no per-machine setup — which is the whole reason the feature exists.
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

// Set is every namespace installed on this machine.
type Set struct {
	Dir        string
	Namespaces map[string]*Namespace
}

// Load reads every namespace in dir. A missing directory is not an error —
// having no global tasks is the ordinary state of a machine — but a file that is
// present and wrong IS one, because a namespace that silently failed to load is
// a command that has stopped existing without saying so.
func Load(dir string) (*Set, error) {
	set := &Set{Dir: dir, Namespaces: map[string]*Namespace{}}
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
		n, err := loadFile(path)
		if err != nil {
			return nil, err
		}
		if other, dup := set.Namespaces[n.Name]; dup {
			// Two files claiming one name means one of them is unreachable, and
			// which one would depend on directory order. Name both.
			return nil, fmt.Errorf("two namespaces are called %q: %s and %s", n.Name, other.Path, n.Path)
		}
		set.Namespaces[n.Name] = n
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

func loadFile(path string) (*Namespace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	// Unknown fields are an error here for the same reason they are in a
	// taskfile: a typo in a key is far likelier than a deliberate extension, and
	// ignoring it turns the typo into silence.
	dec.KnownFields(true)

	var n Namespace
	if err := dec.Decode(&n); err != nil {
		return nil, fmt.Errorf("%s: %s", path, readable(err))
	}
	n.Path = path
	if err := n.Validate(); err != nil {
		return nil, err
	}
	defaults(&n)
	for name, t := range n.Tasks {
		t.Name, t.Namespace = name, n.Name
	}
	return &n, nil
}

// defaults fills in what ssh would fill in, at load rather than at dial, so a
// listing and an error message name the port and user a connection will actually
// use rather than the blanks the file left.
func defaults(n *Namespace) {
	me := ""
	if u, err := user.Current(); err == nil {
		me = u.Username
	}
	for name, route := range n.Routes {
		for i := range route {
			if route[i].Port == 0 {
				route[i].Port = DefaultPort
			}
			if route[i].User == "" {
				route[i].User = me
			}
		}
		n.Routes[name] = route
	}
}

// readable turns yaml.v3's type-shaped complaint into one aimed at whoever wrote
// the file, the same way internal/chorefile does for a taskfile.
func readable(err error) string {
	msg := strings.TrimPrefix(err.Error(), "yaml: unmarshal errors:\n")
	msg = strings.ReplaceAll(msg, "global.Namespace", "a namespace")
	msg = strings.ReplaceAll(msg, "global.Task", "a task")
	msg = strings.ReplaceAll(msg, "global.Hop", "a hop")
	msg = strings.ReplaceAll(msg, "global.Forward", "a forward")
	return strings.TrimSpace(msg)
}

// Lookup resolves an address a person typed. name is everything after `global:`,
// so `homelab:k3s:pods` — the namespace is the part before the FIRST colon,
// because a task name may contain colons of its own and a namespace may not.
func (s *Set) Lookup(name string) (*Task, error) {
	ns, task, ok := strings.Cut(name, ":")
	if !ok || task == "" {
		return nil, fmt.Errorf("global:%s names a namespace, not a task — `chore global:%s:` lists what is in it", name, ns)
	}
	n, ok := s.Namespaces[ns]
	if !ok {
		return nil, fmt.Errorf("no global namespace %q in %s%s", ns, s.Dir, s.suggestNamespace(ns))
	}
	t, ok := n.Tasks[task]
	if !ok {
		return nil, fmt.Errorf("no task %q in global:%s (%s)%s", task, ns, n.Path, n.suggestTask(task))
	}
	return t, nil
}

func (s *Set) suggestNamespace(name string) string {
	names := sortedKeys(s.Namespaces)
	if len(names) == 0 {
		return " — no namespaces are installed there"
	}
	return " — installed: " + strings.Join(names, ", ")
}

func (n *Namespace) suggestTask(name string) string {
	var names []string
	for k, t := range n.Tasks {
		if !t.Internal {
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return " — it has: " + strings.Join(names, ", ")
}

// sortedKeys gives map keys in a stable order, so a message names things the
// same way on every run rather than in whatever order the map handed them over.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
