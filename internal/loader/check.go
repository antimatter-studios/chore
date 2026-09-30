package loader

import (
	"fmt"
	"strings"

	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/graph"
)

// Check validates what no single file can: a route or predicate named in one
// file and declared in another, and cycles through `deps:`, routes and
// predicates, which can run through several files at once.
//
// strict is false while a tree is loaded on its own, when a `global:` name has
// nowhere to resolve yet, and true once global.d is attached — from then on a
// reference that finds nothing is refused here rather than at run time.
func Check(p *chorefile.Project, strict bool) error {
	missing := func(ref string) bool { return strict || !chorefile.IsGlobalRef(ref) }

	for _, ns := range graph.Sorted(p.Files) {
		f := p.Files[ns]
		for _, name := range graph.Sorted(f.Routes) {
			route := f.Routes[name]
			if !route.Conditional() {
				continue
			}
			where := fmt.Sprintf("%s: route %q", f.Path, name)
			pred := chorefile.Reference(f, route.If)
			t, ok := p.Tasks[pred]
			switch {
			case !ok && missing(route.If):
				return fmt.Errorf("%s names %q as its `if:`, which is not a task%s", where, route.If, suggestFrom(p, pred))
			case ok && t.Route != "":
				// A predicate answers a question about THIS machine; one that
				// travelled would pay a connection to answer it — 326-377 ms
				// against 0.65 ms on the LAN — and read as slowness, not a mistake.
				return fmt.Errorf("%s names task %q as its `if:`, but that task has `route: %s` —"+
					" a predicate must run on this machine", where, route.If, t.Route)
			}
			for _, branch := range []struct{ key, ref string }{{"then", route.Then}, {"else", route.Else}} {
				if _, ok := LookupRoute(p, f, branch.ref); !ok && missing(branch.ref) {
					return fmt.Errorf("%s names %q as its `%s:`, which is not a route", where, branch.ref, branch.key)
				}
			}
		}
	}

	for _, name := range graph.Sorted(p.Tasks) {
		t := p.Tasks[name]
		if t.Name != name {
			continue // an alias; the task is checked under its own name
		}
		for _, ref := range []struct{ key, value string }{{"route", t.Route}, {"with_route", t.WithRoute}} {
			if ref.value == "" {
				continue
			}
			if _, ok := LookupRoute(p, t.File, ref.value); !ok && missing(ref.value) {
				return fmt.Errorf("%s: task %q names `%s: %s`, which is not a route", t.File.Path, name, ref.key, ref.value)
			}
		}
		// A dependency on another file's task is checked once that file is in.
		// Within one tree a missing dependency is still found at run time, as it
		// always has been; this is about the reference that crosses into global.d.
		if strict {
			for _, d := range t.Deps {
				if !chorefile.IsGlobalRef(d.Task) || isTemplated(d.Task) {
					continue
				}
				if _, ok := p.Tasks[d.Task]; !ok {
					return fmt.Errorf("%s: task %q depends on %q, which is not a task%s", t.File.Path, name, d.Task, suggestFrom(p, d.Task))
				}
			}
		}
	}
	return checkCycles(p)
}

// LookupRoute finds the route a name written in file f refers to.
func LookupRoute(p *chorefile.Project, f *chorefile.File, ref string) (chorefile.Route, bool) {
	ns, name := chorefile.RouteRef(f, ref)
	owner, ok := p.Files[ns]
	if !ok {
		return chorefile.Route{}, false
	}
	r, ok := owner.Routes[name]
	return r, ok
}

// RouteFile is the file a route reference lands in, so a selector's own
// branches and predicate are read from where the selector is written.
func RouteFile(p *chorefile.Project, f *chorefile.File, ref string) *chorefile.File {
	ns, _ := chorefile.RouteRef(f, ref)
	return p.Files[ns]
}

// checkCycles walks tasks and routes as ONE graph: `deps:` joins two tasks,
// `route:` leads from a task to a route, and a selector's `if:` leads from a
// route back to a task. A cycle can run through all three, and through several
// files.
//
// Every task is in it, not only routed ones. A dependency cycle never
// terminates — deps always run — so refusing it at load costs nothing a file
// could have meant, and it used to keep spawning until something gave out.
// A name built from a template cannot be followed until the task runs, so it is
// left out; the graph holds what can be known from the file.
func checkCycles(p *chorefile.Project) error {
	g := graph.New()
	for _, ns := range graph.Sorted(p.Files) {
		f := p.Files[ns]
		for _, name := range graph.Sorted(f.Routes) {
			route := f.Routes[name]
			id := routeNode(ns, name)
			g.Describe(id, fmt.Sprintf("route %s in %s", name, f.Path))
			if !route.Conditional() {
				continue
			}
			if pred := chorefile.Reference(f, route.If); p.Tasks[pred] != nil {
				g.Edge(id, taskNode(p.Tasks[pred].Name))
			}
			for _, branch := range []string{route.Then, route.Else} {
				if _, ok := LookupRoute(p, f, branch); ok {
					bns, bname := chorefile.RouteRef(f, branch)
					g.Edge(id, routeNode(bns, bname))
				}
			}
		}
	}
	for _, name := range graph.Sorted(p.Tasks) {
		t := p.Tasks[name]
		if t.Name != name {
			continue
		}
		id := taskNode(name)
		g.Describe(id, "task "+name)
		for _, d := range t.Deps {
			if isTemplated(d.Task) {
				continue
			}
			if dep := p.Tasks[chorefile.Reference(t.File, d.Task)]; dep != nil {
				g.Edge(id, taskNode(dep.Name))
			}
		}
		for _, ref := range []string{t.Route, t.WithRoute} {
			if ref == "" {
				continue
			}
			if _, ok := LookupRoute(p, t.File, ref); ok {
				rns, rname := chorefile.RouteRef(t.File, ref)
				g.Edge(id, routeNode(rns, rname))
			}
		}
	}
	return g.Check()
}

func routeNode(ns, name string) string { return "route:" + ns + "|" + name }
func taskNode(name string) string      { return "task:" + name }

func isTemplated(s string) bool { return strings.Contains(s, "{{") }

// suggestFrom names a few tasks in the same namespace, since a typo is the
// likeliest reason a reference found nothing.
func suggestFrom(p *chorefile.Project, ref string) string {
	ns := ref
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		ns = ref[:i+1]
	} else {
		ns = ""
	}
	var near []string
	for _, name := range graph.Sorted(p.Tasks) {
		t := p.Tasks[name]
		if t.Internal || t.Name != name || !strings.HasPrefix(name, ns) {
			continue
		}
		near = append(near, name)
		if len(near) == 5 {
			break
		}
	}
	if len(near) == 0 {
		return ""
	}
	return " — there is: " + strings.Join(near, ", ")
}
