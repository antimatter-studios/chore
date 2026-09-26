// Package global discovers taskfiles installed for the current user and makes
// their tasks addressable without depending on the current project directory.
//
// Global files use chore's ordinary task schema. The global layer contributes
// only discovery and the `global:` address prefix; tasks keep chore's normal
// execution semantics.
package global

import (
	"fmt"
	"slices"
	"strings"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// chore:manual global
// title: Global tasks
// summary: declaring and running tasks available from any directory
// order: 10
//
// # Global tasks
//
// A global task is an ordinary chore task declared in a user-wide taskfile. It
// is available from any working directory; global declarations do not change
// what a task can do or how it runs.
//
// ```yaml
// name: homelab
// version: '3'
// tasks:
//   status:
//     desc: show cluster status
//     cmds: [kubectl get nodes]
// ```
//
// Files live in `${XDG_CONFIG_HOME:-$HOME/.config}/chore/global.d/*.yaml`, one
// namespace per file. The `name:` is the namespace segment in the address:
//
// ```
// chore global:                       list installed namespaces
// chore global:homelab:               list a namespace's tasks
// chore global:homelab:status         run an ordinary chore task
// ```
//
// `--dry`, task arguments, dependencies, includes and lifecycle hooks keep their
// ordinary chore meanings. A taskfile's namespace is its name, while the task
// name and task schema remain unchanged.

// Namespace is one taskfile installed in global.d.
type Namespace struct {
	Name    string
	Path    string
	Project *chorefile.Project
}

// Task looks up a namespace-local task name, which may itself contain colons.
func (n *Namespace) Task(name string) (*chorefile.Task, bool) {
	t, ok := n.Project.Tasks[name]
	return t, ok
}

// Address returns the global spelling of a task name.
func Address(namespace, task string) string {
	return "global:" + namespace + ":" + task
}

// SplitAddress removes the global prefix and separates the namespace from the
// task at the first colon, preserving colons in the task name.
func SplitAddress(address string) (namespace, task string, ok bool) {
	if !strings.HasPrefix(address, "global:") {
		return "", "", false
	}
	return strings.Cut(strings.TrimPrefix(address, "global:"), ":")
}

// Set is the machine-wide task surface installed for this user.
type Set struct {
	Dir        string
	Namespaces map[string]*Namespace
}

// Lookup resolves a global address and suggests installed namespaces or tasks
// when the name is mistyped.
func (s *Set) Lookup(address string) (*Namespace, string, *chorefile.Task, error) {
	namespace, taskName, ok := SplitAddress(address)
	if !ok || namespace == "" {
		return nil, "", nil, fmt.Errorf("%q is not a global task address; use `global:<namespace>:<task>`", address)
	}
	n, ok := s.Namespaces[namespace]
	if !ok {
		return nil, "", nil, fmt.Errorf("no global namespace %q in %s%s", namespace, s.Dir, s.suggestNamespace())
	}
	if taskName == "" {
		return n, "", nil, nil
	}
	t, ok := n.Task(taskName)
	if !ok {
		return n, taskName, nil, fmt.Errorf("no task %q in global:%s (%s)%s", taskName, namespace, n.Path, n.suggestTask())
	}
	return n, taskName, t, nil
}

// Names returns installed namespace names in stable order.
func (s *Set) Names() []string { return sortedKeys(s.Namespaces) }

func (s *Set) suggestNamespace() string {
	names := sortedKeys(s.Namespaces)
	if len(names) == 0 {
		return " — no namespaces are installed there"
	}
	return " — installed: " + strings.Join(names, ", ")
}

func (n *Namespace) suggestTask() string {
	var names []string
	for name, task := range n.Project.Tasks {
		if !task.Internal && name == task.Name {
			names = append(names, name)
		}
	}
	sortStrings(names)
	if len(names) == 0 {
		return ""
	}
	return " — it has: " + strings.Join(names, ", ")
}

func sortStrings(values []string) {
	slices.Sort(values)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sortStrings(out)
	return out
}
