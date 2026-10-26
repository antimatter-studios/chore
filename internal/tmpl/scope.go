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

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// Kind says what a Value holds.
type Kind int

// Value kinds. String is the zero value and by far the common case.
const (
	String Kind = iota
	List
	Map
)

// Value is what one variable holds: a string, or the list or map a `vars:`
// entry was written as.
//
// Every Value has a string form, and that is all a template or the environment
// ever sees — `{{.HOSTS}}` and `$HOSTS` read it. The items are kept beside it for
// `for:`, which is the one consumer that needs them apart. Keeping templates on
// strings is deliberate: text/template on a map[string]any prints "<no value>"
// for a missing key even under missingkey=zero, and every `{{.X | default "y"}}`
// in every file is written assuming it gets "".
//
// Immutable once built: Push copies layers but not the slices inside, which is
// safe only because nothing writes to them.
type Value struct {
	Kind Kind
	// Items is a list's elements, or a map's values in written order.
	Items []string
	// Keys is a map's keys, parallel to Items. nil for anything else.
	Keys []string
	str  string
}

// StringValue wraps a plain string.
func StringValue(s string) Value { return Value{str: s} }

// ListValue builds a list. Its string form is chorefile.JoinList.
func ListValue(items []string) Value {
	return Value{Kind: List, Items: items, str: chorefile.JoinList(items)}
}

// MapValue builds a map from parallel keys and values, in that order. Its string
// form is chorefile.JoinMap.
func MapValue(keys, values []string) Value {
	return Value{Kind: Map, Keys: keys, Items: values, str: chorefile.JoinMap(keys, values)}
}

// String is the value as templates and the environment see it.
func (v Value) String() string { return v.str }

// Scope is a stack of variable layers. Later layers have higher priority.
type Scope struct {
	// layers[0] is the lowest priority. Every map is non-nil and owned by this
	// Scope, which is what makes Set safe and Push non-mutating.
	layers []map[string]Value
}

// New returns a scope whose only — and therefore lowest priority — layer is the
// process environment, in os.Environ() form ("KEY=value").
//
// Entries with no "=" or an empty key are skipped rather than reported: this is
// inherited data, not something the user typed, and one malformed entry must not
// stop a run. (Only dotenv files, which the user does write, are strict — a
// missing one is an error, per SPEC "Fixed semantics" #4.)
func New(env []string) *Scope {
	base := make(map[string]Value, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		base[k] = StringValue(v)
	}
	return &Scope{layers: []map[string]Value{base}}
}

// Push returns a child scope with vars as its new highest priority layer.
//
// The child is a snapshot: every layer is copied, so nothing the child does can
// be seen by the parent, and nothing done later to the parent — or to the
// caller's vars map — can leak into the child. Scopes are small (a handful of
// layers of a handful of variables, pushed once per task invocation), so the
// copying costs less than the aliasing bugs it removes.
func (s *Scope) Push(vars map[string]string) *Scope {
	layer := make(map[string]Value, len(vars))
	for k, v := range vars {
		layer[k] = StringValue(v)
	}
	return s.PushValues(layer)
}

// PushValues is Push for a layer that may hold lists and maps — the shape
// ResolveValues returns, so a `vars:` block keeps its collections on the way
// into the scope.
func (s *Scope) PushValues(vars map[string]Value) *Scope {
	child := &Scope{layers: make([]map[string]Value, 0, len(s.layers)+1)}
	for _, l := range s.layers {
		child.layers = append(child.layers, copyOf(l))
	}
	child.layers = append(child.layers, copyOf(vars))
	return child
}

// Set writes into the highest priority layer, so the value outranks everything
// the scope inherited. Resolve uses it to make an already-resolved variable
// visible to the ones still to be resolved.
func (s *Scope) Set(k, v string) { s.SetValue(k, StringValue(v)) }

// SetValue is Set for a value that may be a list or a map.
func (s *Scope) SetValue(k string, v Value) {
	if len(s.layers) == 0 {
		// A zero-value Scope is still usable; give Set somewhere to write.
		s.layers = []map[string]Value{{}}
	}
	s.layers[len(s.layers)-1][k] = v
}

// Get returns the string form of the highest priority value for k.
func (s *Scope) Get(k string) (string, bool) {
	v, ok := s.Lookup(k)
	return v.String(), ok
}

// Lookup returns the highest priority value for k, list or map included. A
// string set above a list shadows it, exactly as it would another string:
// `chore deploy HOSTS="a b"` replaces the file's list with a string, which
// `for:` then splits.
func (s *Scope) Lookup(k string) (Value, bool) {
	for i := len(s.layers) - 1; i >= 0; i-- {
		if v, ok := s.layers[i][k]; ok {
			return v, true
		}
	}
	return Value{}, false
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
		for k, v := range l {
			out[k] = v.String()
		}
	}
	return out
}

// Declared flattens every layer above the process environment: the values the
// chorefile, its dotenv files and the command line declared, and nothing the
// shell that started chore happened to carry. A key that has to mean the same
// thing from one shell to the next — a cache's — is built from this, not All.
func (s *Scope) Declared() map[string]string {
	out := map[string]string{}
	for i := 1; i < len(s.layers); i++ {
		for k, v := range s.layers[i] {
			out[k] = v.String()
		}
	}
	return out
}

// copyOf returns a non-nil copy, so every layer is writable by Set.
func copyOf(m map[string]Value) map[string]Value {
	out := make(map[string]Value, len(m))
	maps.Copy(out, m)
	return out
}
