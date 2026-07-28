package chorefile

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Decode parses a Taskfile. Unknown fields are an error: a typo in a key is far
// more likely than a deliberate extension, and Task's habit of ignoring what it
// does not recognise turns a typo into silence.
// readable turns a yaml decode error into something aimed at whoever wrote the
// file rather than at whoever wrote this package. yaml.v3 reports an unknown key
// as `field dotenv not found in type chorefile.Task`, which names a Go type the
// reader has never heard of and cannot act on.
func readable(err error) error {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "yaml: unmarshal errors:\n")
	// `field X not found in type chorefile.Y` → `unknown field "X" in a Y`
	msg = unknownField.ReplaceAllStringFunc(msg, func(m string) string {
		g := unknownField.FindStringSubmatch(m)
		kind := strings.ToLower(g[2])
		article := "a"
		if strings.ContainsRune("aeiou", rune(kind[0])) {
			article = "an"
		}
		return fmt.Sprintf("unknown field %q in %s %s", g[1], article, kind)
	})
	msg = strings.ReplaceAll(msg, "chorefile.", "")
	return errors.New(strings.TrimSpace(msg))
}

var unknownField = regexp.MustCompile(`field ([A-Za-z0-9_-]+) not found in type chorefile\.([A-Za-z]+)`)

func Decode(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, readable(err)
	}
	for name, t := range f.Tasks {
		if t == nil {
			// `foo:` with an empty body is legal and means "a task that does
			// nothing" — usually a placeholder or an alias target.
			f.Tasks[name] = &Task{}
			continue
		}
		for i, arg := range t.Args {
			if !validParamName(arg.Name) {
				return nil, fmt.Errorf("taskfile: task %q: args entry %d is %s, which cannot be used as a variable —"+
					" a parameter name must start with a letter or underscore and contain only letters, digits and underscores",
					name, i+1, describeParam(arg.Name))
			}
			switch arg.Type {
			case "", TypeString, TypeBool, TypeInt:
			default:
				return nil, fmt.Errorf("taskfile: task %q: parameter %q has type %q; expected %s, %s or %s",
					name, arg.Name, arg.Type, TypeString, TypeBool, TypeInt)
			}
		}
	}
	return &f, nil
}

// validParamName reports whether a declared parameter can actually be referenced
// as {{.Name}}. A name that cannot be is always a mistake, and one particular
// mistake is easy to make: `- !config` is YAML TAG syntax, and yaml.v3 decodes it
// to the empty string rather than failing, so the parameter silently becomes
// unnameable. Rejecting it here is the difference between a clear error and a
// task that mysteriously receives nothing.
func validParamName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func describeParam(s string) string {
	if s == "" {
		return "empty (a leading \"!\" is YAML tag syntax — remove it)"
	}
	return quoteForError(s)
}

// UnmarshalYAML accepts either form a variable can take:
//
//	FOO: bar
//	FOO: {sh: git rev-parse HEAD}
func (v *Var) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		s, err := scalarString(n)
		if err != nil {
			return err
		}
		v.Value = s
		return nil
	case yaml.MappingNode:
		if err := knownFields(n, "a variable", "sh"); err != nil {
			return err
		}
		var m struct {
			Sh *string `yaml:"sh"`
		}
		if err := n.Decode(&m); err != nil {
			return fmt.Errorf("line %d: a variable mapping accepts only `sh`: %w", n.Line, err)
		}
		if m.Sh == nil {
			return fmt.Errorf("line %d: a variable mapping must set `sh`", n.Line)
		}
		v.Sh = *m.Sh
		return nil
	default:
		return fmt.Errorf("line %d: a variable must be a value or a mapping with `sh`", n.Line)
	}
}

// UnmarshalYAML accepts a shell command as a plain string, or a mapping calling
// another task:
//
//	cmds:
//	  - go build ./...
//	  - task: postgres:up
//	    vars: {PORT: '5432'}
func (c *Cmd) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		s, err := scalarString(n)
		if err != nil {
			return err
		}
		c.Cmd = s
		return nil
	case yaml.MappingNode:
		if err := knownFields(n, "a command", "cmd", "task", "vars", "silent", "ignore_error", "defer"); err != nil {
			return err
		}
		type raw struct {
			Cmd         string         `yaml:"cmd"`
			Task        string         `yaml:"task"`
			Vars        map[string]Var `yaml:"vars"`
			Silent      bool           `yaml:"silent"`
			IgnoreError bool           `yaml:"ignore_error"`
			// Defer nests a whole command: `- defer: { task: e2e:down }` or
			// `- defer: docker rm -f box`.
			Defer *Cmd `yaml:"defer"`
		}
		var r raw
		if err := n.Decode(&r); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		if r.Defer != nil {
			if r.Cmd != "" || r.Task != "" {
				return fmt.Errorf("line %d: `defer` is a command of its own; do not combine it with `cmd` or `task`", n.Line)
			}
			*c = *r.Defer
			c.Defer = true
			return nil
		}
		if r.Cmd != "" && r.Task != "" {
			return fmt.Errorf("line %d: a command sets either `cmd` or `task`, not both", n.Line)
		}
		if r.Cmd == "" && r.Task == "" {
			return fmt.Errorf("line %d: a command needs `cmd`, `task` or `defer`", n.Line)
		}
		c.Cmd, c.Task, c.Vars = r.Cmd, r.Task, r.Vars
		c.Silent, c.IgnoreError = r.Silent, r.IgnoreError
		return nil
	default:
		return fmt.Errorf("line %d: a command must be a string or a mapping", n.Line)
	}
}

// UnmarshalYAML accepts a dependency as a name, or as a mapping passing vars:
//
//	deps: [build, {task: sign, vars: {KEY: dev}}]
func (d *Dep) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		s, err := scalarString(n)
		if err != nil {
			return err
		}
		if s == "" {
			return fmt.Errorf("line %d: empty dependency name", n.Line)
		}
		d.Task = s
		return nil
	case yaml.MappingNode:
		if err := knownFields(n, "a dependency", "task", "vars", "silent"); err != nil {
			return err
		}
		type raw struct {
			Task   string         `yaml:"task"`
			Vars   map[string]Var `yaml:"vars"`
			Silent bool           `yaml:"silent"`
		}
		var r raw
		if err := n.Decode(&r); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		if r.Task == "" {
			return fmt.Errorf("line %d: a dependency needs `task`", n.Line)
		}
		d.Task, d.Vars, d.Silent = r.Task, r.Vars, r.Silent
		return nil
	default:
		return fmt.Errorf("line %d: a dependency must be a string or a mapping", n.Line)
	}
}

// UnmarshalYAML accepts a parameter as a bare name or as a declaration:
//
//	args:
//	  - config
//	  - {name: follow, type: bool, desc: keep streaming}
func (a *Arg) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		s, err := scalarString(n)
		if err != nil {
			return err
		}
		a.Name = s
		return nil
	case yaml.MappingNode:
		if err := knownFields(n, "a parameter", "name", "type", "desc"); err != nil {
			return err
		}
		type raw struct {
			Name string `yaml:"name"`
			Type string `yaml:"type"`
			Desc string `yaml:"desc"`
		}
		var r raw
		if err := n.Decode(&r); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		if r.Name == "" {
			return fmt.Errorf("line %d: a parameter needs `name`", n.Line)
		}
		a.Name, a.Type, a.Desc = r.Name, r.Type, r.Desc
		return nil
	default:
		return fmt.Errorf("line %d: a parameter must be a name or a mapping", n.Line)
	}
}

// UnmarshalYAML rejects an empty list entry, like Cmds and Deps.
func (as *Args) UnmarshalYAML(n *yaml.Node) error {
	if err := checkNoNullElements(n, "args"); err != nil {
		return err
	}
	var out []Arg
	if err := n.Decode(&out); err != nil {
		return err
	}
	*as = out
	return nil
}

// UnmarshalYAML rejects an empty list entry. See the Cmds type comment.
func (c *Cmds) UnmarshalYAML(n *yaml.Node) error {
	if err := checkNoNullElements(n, "cmds"); err != nil {
		return err
	}
	var out []Cmd
	if err := n.Decode(&out); err != nil {
		return err
	}
	*c = out
	return nil
}

// UnmarshalYAML rejects an empty list entry. See the Cmds type comment.
func (d *Deps) UnmarshalYAML(n *yaml.Node) error {
	if err := checkNoNullElements(n, "deps"); err != nil {
		return err
	}
	var out []Dep
	if err := n.Decode(&out); err != nil {
		return err
	}
	*d = out
	return nil
}

func checkNoNullElements(n *yaml.Node, what string) error {
	if n.Kind != yaml.SequenceNode {
		return fmt.Errorf("line %d: %s must be a list", n.Line, what)
	}
	for i, el := range n.Content {
		if el.Tag == "!!null" {
			return fmt.Errorf("line %d: %s entry %d is empty — a bare \"-\" would be dropped silently", el.Line, what, i+1)
		}
	}
	return nil
}

// knownFields rejects any key of a mapping node that is not in allowed.
//
// Decode sets KnownFields(true), but yaml.Node.Decode — the only way a custom
// UnmarshalYAML can read its node — builds a fresh decoder that does NOT
// inherit it. So inside Var, Cmd and Dep strictness has to be re-imposed by
// hand, or `{sh: date, shh: 1}` and `{cmd: x, slient: true}` decode cleanly and
// the typo becomes exactly the silence Decode exists to prevent.
func knownFields(n *yaml.Node, what string, allowed ...string) error {
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i]
		if !slices.Contains(allowed, key.Value) {
			return fmt.Errorf("line %d: unknown field %s in %s; expected one of %v",
				key.Line, quoteForError(key.Value), what, allowed)
		}
	}
	return nil
}

// scalarString renders a YAML scalar as the string the templating layer expects.
// Numbers and booleans are written unquoted all over real Taskfiles (`port: 25`,
// `silent: true` inside a vars block), and every consumer here treats a variable
// as text, so normalise rather than reject.
//
// Template text is returned byte-for-byte: a value like {{`{{.Names}}`}} passes
// a Go template through to docker's --format, and mangling it would break that.
func scalarString(n *yaml.Node) (string, error) {
	switch n.Tag {
	case "!!str", "!!int", "!!float", "!!bool", "!!timestamp":
		return n.Value, nil
	case "!!null":
		return "", nil
	default:
		var s string
		if err := n.Decode(&s); err != nil {
			return "", fmt.Errorf("line %d: expected a value, got %s", n.Line, n.Tag)
		}
		return s, nil
	}
}

// quoteForError makes a value printable inside an error message.
func quoteForError(s string) string { return strconv.Quote(s) }
