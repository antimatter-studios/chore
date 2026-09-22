package global

import (
	"fmt"
	"strings"

	"github.com/antimatter-studios/chore/internal/graph"
)

// crossCheck validates every reference that names another namespace.
//
// Separate from Namespace.Validate because of when each can run: a file is
// validated as it is read, when no other namespace exists yet, so
// `global:ssh:unlock` cannot be checked there. Two passes, each checking what
// it can actually see.
func (s *Set) crossCheck() error {
	for _, nsName := range sortedKeys(s.Namespaces) {
		n := s.Namespaces[nsName]
		for _, name := range sortedKeys(n.Tasks) {
			for _, ref := range n.Tasks[name].Deps {
				if !strings.HasPrefix(ref, "global:") {
					continue
				}
				if _, err := s.dep(n, ref); err != nil {
					return fmt.Errorf("%s: task %q depends on %q: %w", n.Path, name, ref, err)
				}
			}
		}
		for _, name := range sortedKeys(n.Routes) {
			route := n.Routes[name]
			if !route.Conditional() || !strings.HasPrefix(route.If, "global:") {
				continue
			}
			p, err := s.dep(n, route.If)
			if err != nil {
				return fmt.Errorf("%s: route %q names %q as its `if:`: %w", n.Path, name, route.If, err)
			}
			if p.Route != "" {
				return fmt.Errorf("%s: route %q names task %q as its `if:`, but that task has `route: %s` —"+
					" a predicate must run on this machine", n.Path, name, route.If, p.Route)
			}
		}
	}
	return s.checkCycles()
}

// checkCycles walks routes and tasks as ONE graph.
//
// They stopped being two the moment a predicate became a task: `if:` is an edge
// from a route to a task, `deps:` joins two tasks, and `route:` leads from a
// task back to a route. A cycle can run through all three, and neither of the
// walks that existed could see it — the route walk stopped at the task
// boundary, and the dependency memo never looked at routes.
func (s *Set) checkCycles() error {
	g := graph.New()
	for _, nsName := range graph.Sorted(s.Namespaces) {
		n := s.Namespaces[nsName]
		for _, name := range graph.Sorted(n.Routes) {
			route := n.Routes[name]
			id := routeNode(nsName, name)
			g.Describe(id, fmt.Sprintf("route %s in %s", name, n.Path))
			if !route.Conditional() {
				continue
			}
			g.Edge(id, s.taskNode(n, route.If))
			g.Edge(id, routeNode(nsName, route.Then))
			g.Edge(id, routeNode(nsName, route.Else))
		}
		for _, name := range graph.Sorted(n.Tasks) {
			t := n.Tasks[name]
			id := taskNodeIn(nsName, name)
			g.Describe(id, "task "+t.Address())
			for _, ref := range t.Deps {
				g.Edge(id, s.taskNode(n, ref))
			}
			if t.Route != "" {
				g.Edge(id, routeNode(nsName, t.Route))
			}
			if t.WithRoute != "" {
				g.Edge(id, routeNode(nsName, t.WithRoute))
			}
		}
	}
	return g.Check()
}

func routeNode(ns, name string) string  { return "route:" + ns + ":" + name }
func taskNodeIn(ns, name string) string { return "task:" + ns + ":" + name }

// taskNode turns a reference as written into a node id, resolving a full
// address to the namespace it names and a bare one to the file it is in.
func (s *Set) taskNode(n *Namespace, ref string) string {
	if !strings.HasPrefix(ref, "global:") {
		return taskNodeIn(n.Name, ref)
	}
	ns, task, ok := strings.Cut(strings.TrimPrefix(ref, "global:"), ":")
	if !ok {
		return "task:" + strings.TrimPrefix(ref, "global:")
	}
	return taskNodeIn(ns, task)
}

// dep resolves a reference to the task it names.
//
// Two spellings, one rule, the same one the command line uses: a `global:`
// prefix is a full address naming any namespace's task, and a bare name is this
// namespace's own. Written here rather than inline at each call site because
// `deps:` and a selector's `if:` both need it and must agree.
func (s *Set) dep(n *Namespace, ref string) (*Task, error) {
	if strings.HasPrefix(ref, "global:") {
		return s.Lookup(strings.TrimPrefix(ref, "global:"))
	}
	t, ok := n.Tasks[ref]
	if !ok {
		return nil, fmt.Errorf("not a task in %s", n.Path)
	}
	return t, nil
}
