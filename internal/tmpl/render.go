package tmpl

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// funcs is the whole function set: Go's builtins plus `default`.
//
// rest-mail's Taskfiles call `default` 216 times and no other function, so there
// is no sprig here, no seam for it to creep in later, and no third-party
// function semantics to stay compatible with.
var funcs = template.FuncMap{"default": defaultFn}

// defaultFn implements `default d v` — v unless v is empty, in which case d.
//
// The default comes first because the pipeline form is what Taskfiles use:
// `{{.CONFIG | default "restmail.test"}}` appends the value as the last
// argument. `{{default "restmail.test" .CONFIG}}` works too, for free.
func defaultFn(def, v any) any {
	if isEmpty(v) {
		return def
	}
	return v
}

// isEmpty reports whether a value should fall back to the default.
//
// Deliberately narrower than sprig's `default`: numbers and booleans are never
// empty here. Scope values are strings, so a variable set to "0" or "false" is
// something the author wrote on purpose, and swapping it for the default would
// be exactly the sort of quiet wrong answer this program exists to remove.
func isEmpty(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return rv.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return rv.IsNil()
	default:
		return false
	}
}

// Render renders text as a Go template against the flattened scope.
//
// Reading an undefined variable yields the empty string instead of an error
// (missingkey=zero), which is what Taskfiles assume: `{{.X | default "y"}}` is
// written precisely because X often does not exist. `requires:` is where a
// missing value is supposed to become an error.
//
// Render reads the scope and nothing else — no clock, no environment lookup, no
// mutation — so the same scope and text always give the same output.
func (s *Scope) Render(text string) (string, error) {
	if !strings.Contains(text, "{{") {
		// Most commands, paths and globs contain no actions at all. A
		// 3,000-line Taskfile renders thousands of such strings per run, and
		// parsing every one of them is pure waste.
		return text, nil
	}
	t, err := parseTemplate(text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, s.All()); err != nil {
		return "", fmt.Errorf("rendering template %q: %w", text, err)
	}
	return b.String(), nil
}

// parseTemplate parses with the one function set and the one missing-key policy,
// so parsing for dependency analysis cannot disagree with parsing for output.
func parseTemplate(text string) (*template.Template, error) {
	t, err := template.New("chore").Funcs(funcs).Option("missingkey=zero").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("parsing template %q: %w", text, err)
	}
	return t, nil
}

// Capturer runs a shell script and returns its stdout.
//
// internal/shell.Shell satisfies it. Taking an interface keeps this package
// independent of the shell implementation — and lets its tests run without a
// shell at all.
type Capturer interface {
	Capture(ctx context.Context, script string) (string, error)
}

// Resolve turns a Taskfile `vars:` block into concrete values.
//
// Per variable: if Sh is set, the script is rendered, run, and its trimmed
// stdout becomes the value; otherwise Value is rendered. Sh wins if both are
// set, because a var that names a command clearly meant to run it.
//
// Variables may reference each other, and YAML mappings decode into Go maps,
// which have no order. Rather than invent one, Resolve makes repeated passes:
// each pass resolves every variable that does not reference a still-unresolved
// variable of the same block, and whatever a pass resolves is visible to the
// next one. A pass that resolves nothing means everything left is waiting on
// something left — a cycle — reported with the names involved. Each pass
// resolves at least one variable, so there are at most len(vars) passes; within
// a pass, names are visited in sorted order so output and error messages are
// deterministic.
//
// A variable may reference its own name (PATH: "{{.PATH}}:/opt/bin"); that reads
// the inherited value from the scope and is not a cycle.
//
// The receiver is not modified: values land in a child scope and are returned as
// a map for the caller to Push as its own layer.
func (s *Scope) Resolve(ctx context.Context, vars map[string]chorefile.Var, cap Capturer) (map[string]string, error) {
	out := make(map[string]string, len(vars))
	if len(vars) == 0 {
		return out, nil
	}

	// cur carries the values resolved so far, without touching s.
	cur := s.Push(nil)
	pending := make(map[string]bool, len(vars))
	for name := range vars {
		pending[name] = true
	}

	for len(pending) > 0 {
		progressed := false
		for _, name := range slices.Sorted(maps.Keys(pending)) {
			text, isSh := source(vars[name])

			refs, err := references(text)
			if err != nil {
				return nil, fmt.Errorf("var %s: %w", name, err)
			}
			if waitingOn(refs, name, pending) {
				continue
			}

			val, err := cur.Render(text)
			if err != nil {
				return nil, fmt.Errorf("var %s: %w", name, err)
			}
			if isSh {
				if cap == nil {
					return nil, fmt.Errorf("var %s: sh: %q needs a shell but none was provided", name, val)
				}
				stdout, err := cap.Capture(ctx, val)
				if err != nil {
					return nil, fmt.Errorf("var %s: sh: %q: %w", name, val, err)
				}
				// Trim again even though Capturer is documented to trim: a
				// variable holding a stray newline poisons every command it is
				// interpolated into, and that is not worth trusting to an
				// implementation detail.
				val = strings.TrimSpace(stdout)
			}

			cur.Set(name, val)
			out[name] = val
			delete(pending, name)
			progressed = true
		}
		if !progressed {
			return nil, cycleError(vars, pending)
		}
	}
	return out, nil
}

// source returns the template to render for a var, and whether it is a script.
func source(v chorefile.Var) (text string, isSh bool) {
	if v.Sh != "" {
		return v.Sh, true
	}
	return v.Value, false
}

// waitingOn reports whether text reads a variable of the same block that has not
// been resolved yet. A reference to the variable's own name does not count: it
// reads the inherited value, which is already available.
func waitingOn(refs []string, self string, pending map[string]bool) bool {
	for _, r := range refs {
		if r != self && pending[r] {
			return true
		}
	}
	return false
}

// references returns the variable names text reads: the first identifier of each
// field node, so {{.CONFIG}} yields CONFIG.
//
// It parses rather than pattern-matches, so text merely passed through to
// another program is not mistaken for a reference. Taskfiles wrap another
// program's templates in a raw string — {{`{{.Names}}`}} for `docker --format` —
// and a scan for `.Ident` would read that as a dependency and could invent a
// cycle out of two variables that never reference each other. Indirect reads
// ({{index . "CONFIG"}}) are not detected; nothing does that, and the cost of
// missing one is a value resolving in the wrong order, not a crash.
func references(text string) ([]string, error) {
	if !strings.Contains(text, "{{") {
		return nil, nil
	}
	t, err := parseTemplate(text)
	if err != nil {
		return nil, err
	}
	var names []string
	seen := map[string]bool{}
	walk(t.Tree.Root, func(n parse.Node) {
		f, ok := n.(*parse.FieldNode)
		if !ok || len(f.Ident) == 0 || seen[f.Ident[0]] {
			return
		}
		seen[f.Ident[0]] = true
		names = append(names, f.Ident[0])
	})
	return names, nil
}

// walk calls fn for every node in a parsed template. Each case checks for a nil
// pointer of its own type: an absent else branch is a typed nil, which is not
// equal to a nil interface.
func walk(n parse.Node, fn func(parse.Node)) {
	if n == nil {
		return
	}
	fn(n)
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			walk(c, fn)
		}
	case *parse.PipeNode:
		if n == nil {
			return
		}
		for _, c := range n.Cmds {
			walk(c, fn)
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			walk(a, fn)
		}
	case *parse.ActionNode:
		walk(n.Pipe, fn)
	case *parse.IfNode:
		walkBranch(&n.BranchNode, fn)
	case *parse.RangeNode:
		walkBranch(&n.BranchNode, fn)
	case *parse.WithNode:
		walkBranch(&n.BranchNode, fn)
	case *parse.TemplateNode:
		walk(n.Pipe, fn)
	case *parse.ChainNode:
		walk(n.Node, fn)
	}
}

func walkBranch(b *parse.BranchNode, fn func(parse.Node)) {
	walk(b.Pipe, fn)
	walk(b.List, fn)
	walk(b.ElseList, fn)
}

// cycleError names every variable still pending and what it is waiting for, so
// the message points at the edit that fixes it.
func cycleError(vars map[string]chorefile.Var, pending map[string]bool) error {
	parts := make([]string, 0, len(pending))
	for _, name := range slices.Sorted(maps.Keys(pending)) {
		text, _ := source(vars[name])
		refs, _ := references(text) // already parsed cleanly, so this cannot fail
		var blocking []string
		for _, r := range refs {
			if r != name && pending[r] {
				blocking = append(blocking, r)
			}
		}
		parts = append(parts, fmt.Sprintf("%s references %s", name, strings.Join(blocking, " and ")))
	}
	return fmt.Errorf("variable dependency cycle: %s", strings.Join(parts, "; "))
}
