package chorefile

import (
	"maps"
	"slices"
	"strings"
)

// GlobalPrefix starts every address in a file under global.d:
// `global:ssh:unlock` is the task `unlock` in global.d/ssh.yaml. It is a fixed
// prefix and nothing more — what follows it is an ordinary task, route or
// predicate name.
const GlobalPrefix = "global:"

// IsGlobalRef reports whether a reference names something in global.d.
func IsGlobalRef(s string) bool { return strings.HasPrefix(s, GlobalPrefix) }

// Reference resolves a task name written INSIDE a file — a `- task:` step, a
// `deps:` entry, a route's `if:` — to the name the project knows it by.
//
// A reference is relative to the file it is written in: `- task: deps` inside
// tasks/webmail.yml means webmail's own `deps`, never a root task that happens to
// share the name. The prefix is applied even when the reference already contains
// a colon, which is what makes tasks/monitoring.yml work: it holds a task
// literally named "prometheus:up", and `- task: prometheus:up` beside it means
// monitoring:prometheus:up.
//
// A leading colon escapes to the root of the file's own tree, as in
// `- task: :build` — go-task's behaviour. And `global:<ns>:<task>` is absolute:
// the same spelling reaches the same task from any file, which is how a project
// task depends on a machine's `global:ssh:unlock`.
func Reference(f *File, name string) string {
	if IsGlobalRef(name) {
		return name
	}
	if rest, ok := strings.CutPrefix(name, ":"); ok {
		return qualify(RootOf(f).namespace(), rest)
	}
	return qualify(f.namespace(), name)
}

// RouteRef resolves a route name written inside f to the namespace of the file
// that declares it and the route's name there. The rules are Reference's: bare
// is this file, a leading colon is the root of this file's tree, and
// `global:<ns>:<route>` is the root file global.d/<ns>.yaml.
func RouteRef(f *File, name string) (namespace, route string) {
	if IsGlobalRef(name) {
		ns, route, _ := strings.Cut(strings.TrimPrefix(name, GlobalPrefix), ":")
		return GlobalPrefix + ns, route
	}
	if rest, ok := strings.CutPrefix(name, ":"); ok {
		return RootOf(f).namespace(), rest
	}
	return f.namespace(), name
}

// RootOf walks up the include chain to the file that started it: the project's
// chores.yml, or a file in global.d. Nil for nil.
func RootOf(f *File) *File {
	for f != nil && f.Parent != nil {
		f = f.Parent
	}
	return f
}

func (f *File) namespace() string {
	if f == nil {
		return ""
	}
	return f.Namespace
}

func qualify(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + ":" + name
}

func joinSorted[V any](m map[string]V) string { return strings.Join(slices.Sorted(maps.Keys(m)), ", ") }
