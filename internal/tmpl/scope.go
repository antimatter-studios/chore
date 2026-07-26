// Package tmpl holds the variable scope and the template renderer.
//
// The two live together because they are inseparable: a variable's value is
// itself a template, rendered against the variables that outrank it, so the
// scope has to know how to render and the renderer has to know the scope.
//
// Precedence is fixed and applied once per invocation (SPEC, "Fixed semantics"
// #2), highest priority first:
//
//	positional args → call vars → task vars → include vars → file vars → dotenv → process environment
//
// A Scope stores that as an ordered stack of layers, lowest priority first, so
// the caller builds it bottom-up with Push and never has to reason about
// shadowing: the layer pushed last wins.
package tmpl

import (
	"maps"
	"strings"
)

// Scope is a stack of variable layers. Later layers have higher priority.
type Scope struct {
	// layers[0] is the lowest priority. Every map is non-nil and owned by this
	// Scope, which is what makes Set safe and Push non-mutating.
	layers []map[string]string
}

// New returns a scope whose only — and therefore lowest priority — layer is the
// process environment, in os.Environ() form ("KEY=value").
//
// Entries with no "=" or an empty key are skipped rather than reported: this is
// inherited data, not something the user typed, and one malformed entry must not
// stop a run. (Only dotenv files, which the user does write, are strict — a
// missing one is an error, per SPEC "Fixed semantics" #4.)
func New(env []string) *Scope {
	base := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		base[k] = v
	}
	return &Scope{layers: []map[string]string{base}}
}

// Push returns a child scope with vars as its new highest priority layer.
//
// The child is a snapshot: every layer is copied, so nothing the child does can
// be seen by the parent, and nothing done later to the parent — or to the
// caller's vars map — can leak into the child. Scopes are small (a handful of
// layers of a handful of variables, pushed once per task invocation), so the
// copying costs less than the aliasing bugs it removes.
func (s *Scope) Push(vars map[string]string) *Scope {
	child := &Scope{layers: make([]map[string]string, 0, len(s.layers)+1)}
	for _, l := range s.layers {
		child.layers = append(child.layers, copyOf(l))
	}
	child.layers = append(child.layers, copyOf(vars))
	return child
}

// Set writes into the highest priority layer, so the value outranks everything
// the scope inherited. Resolve uses it to make an already-resolved variable
// visible to the ones still to be resolved.
func (s *Scope) Set(k, v string) {
	if len(s.layers) == 0 {
		// A zero-value Scope is still usable; give Set somewhere to write.
		s.layers = []map[string]string{{}}
	}
	s.layers[len(s.layers)-1][k] = v
}

// Get returns the highest priority value for k.
func (s *Scope) Get(k string) (string, bool) {
	for i := len(s.layers) - 1; i >= 0; i-- {
		if v, ok := s.layers[i][k]; ok {
			return v, true
		}
	}
	return "", false
}

// All flattens the scope, highest priority winning. The result is a fresh map —
// it is the data passed to text/template, and the caller may keep or modify it
// without reaching into the scope.
func (s *Scope) All() map[string]string {
	n := 0
	for _, l := range s.layers {
		n += len(l)
	}
	out := make(map[string]string, n)
	for _, l := range s.layers {
		maps.Copy(out, l)
	}
	return out
}

// copyOf returns a non-nil copy, so every layer is writable by Set.
func copyOf(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}
