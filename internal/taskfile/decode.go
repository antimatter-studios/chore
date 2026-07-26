package taskfile

import (
	"bytes"
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Decode parses a Taskfile. Unknown fields are an error: a typo in a key is far
// more likely than a deliberate extension, and Task's habit of ignoring what it
// does not recognise turns a typo into silence.
func Decode(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("taskfile: %w", err)
	}
	for name, t := range f.Tasks {
		if t == nil {
			// `foo:` with an empty body is legal and means "a task that does
			// nothing" — usually a placeholder or an alias target.
			f.Tasks[name] = &Task{}
		}
	}
	return &f, nil
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
