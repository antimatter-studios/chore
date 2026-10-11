package chorefile

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Precondition is one local shell check or task call, with an optional refusal
// message. Vars are passed only to task checks, just as in a task step.
type Precondition struct {
	Sh   string         `yaml:"sh"`
	Task string         `yaml:"task"`
	Msg  string         `yaml:"msg"`
	Vars map[string]Var `yaml:"vars"`
}

type Preconditions []Precondition

func (p *Preconditions) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.SequenceNode {
		return fmt.Errorf("line %d: `preconditions:` needs a list of checks", n.Line)
	}
	if err := checkNoNullElements(n, "a preconditions list"); err != nil {
		return err
	}
	type raw Preconditions
	return n.Decode((*raw)(p))
}

func (p *Precondition) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag != "!!str" || strings.TrimSpace(n.Value) == "" {
			return fmt.Errorf("line %d: a precondition needs a non-empty shell command or task", n.Line)
		}
		p.Sh = n.Value
		return nil
	case yaml.MappingNode:
		if err := knownFields(n, "a precondition", "sh", "task", "msg", "vars"); err != nil {
			return err
		}
		type raw Precondition
		var r raw
		if err := n.Decode(&r); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		var hasSh, hasTask, hasVars bool
		for i := 0; i < len(n.Content); i += 2 {
			switch n.Content[i].Value {
			case "sh":
				hasSh = true
			case "task":
				hasTask = true
			case "vars":
				hasVars = true
			}
		}
		if hasSh == hasTask || (hasSh && strings.TrimSpace(r.Sh) == "") || (hasTask && strings.TrimSpace(r.Task) == "") {
			return fmt.Errorf("line %d: a precondition needs exactly one non-empty `sh:` or `task:`", n.Line)
		}
		if hasVars && !hasTask {
			return fmt.Errorf("line %d: precondition `vars:` can only be passed to a `task:`", n.Line)
		}
		*p = Precondition(r)
		return nil
	default:
		return fmt.Errorf("line %d: a precondition is a shell string, `{sh, msg}` or `{task, vars, msg}`", n.Line)
	}
}
