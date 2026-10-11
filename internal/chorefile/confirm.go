package chorefile

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Confirm is the question a task puts to the person who ran it, immediately
// before its own commands.
//
// It is deliberately NOT a precondition, and the difference is the whole of the
// design. A precondition is a fact about the world: checkable, cacheable,
// listed rather than executed by `--dry`, and bypassed by nothing. A
// confirmation is intent from a human. Putting the second among the first is
// what would force `--yes` to mean "skip some preconditions but not others",
// and — decisively — preconditions run BEFORE `before:` and `deps:`, so a
// question asked there would be asked before the thing that informs the answer
// had been produced. Here it is asked last, after everything that could fail
// has failed and everything worth looking at is on the screen.
type Confirm struct {
	// Text is the question, rendered in the task's scope.
	Text string `yaml:"text"`
	// Show is a command whose output is printed before the question — a
	// `pulumi preview`, a `helm diff`, a `kubectl config current-context`. It
	// runs locally in the task's directory, like a precondition's `sh:`.
	//
	// It exists because the useful confirmations are about something you have to
	// look at first, and the alternative spelling — an answer called "show me the
	// diff" — is not an answer at all: it does not terminate, so the question
	// would have to be asked again afterwards, and the feature would be a menu.
	Show string `yaml:"show"`
	// Default is the answer a bare Enter gives. No unless the file says
	// otherwise, so the key that is pressed without reading cannot do anything.
	Default bool `yaml:"default"`
}

// confirmDefault accepts yes/no as well as true/false. YAML 1.2 — which
// yaml.v3 implements — resolves `yes` as the STRING "yes", so `default: no`
// written by someone who has used any other configuration format in their life
// would otherwise fail to decode into a bool with a message about types.
type confirmDefault bool

func (d *confirmDefault) UnmarshalYAML(n *yaml.Node) error {
	switch strings.ToLower(strings.TrimSpace(n.Value)) {
	case "y", "yes", "true":
		*d = true
	case "n", "no", "false":
		*d = false
	default:
		return fmt.Errorf("line %d: confirm `default:` is yes or no, not %s", n.Line, quoteForError(n.Value))
	}
	return nil
}

func (c *Confirm) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag != "!!str" || strings.TrimSpace(n.Value) == "" {
			return fmt.Errorf("line %d: `confirm:` is a question, or `{text, show, default}`", n.Line)
		}
		c.Text = n.Value
		return nil
	case yaml.MappingNode:
		if err := knownFields(n, "a confirm", "text", "show", "default"); err != nil {
			return err
		}
		var r struct {
			Text    string         `yaml:"text"`
			Show    string         `yaml:"show"`
			Default confirmDefault `yaml:"default"`
		}
		if err := n.Decode(&r); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		if strings.TrimSpace(r.Text) == "" {
			return fmt.Errorf("line %d: a confirm needs a non-empty `text:` — the question", n.Line)
		}
		var hasShow bool
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "show" {
				hasShow = true
			}
		}
		if hasShow && strings.TrimSpace(r.Show) == "" {
			return fmt.Errorf("line %d: confirm `show:` is a command to run before asking; leave it out rather than empty", n.Line)
		}
		*c = Confirm{Text: r.Text, Show: r.Show, Default: bool(r.Default)}
		return nil
	default:
		return fmt.Errorf("line %d: `confirm:` is a question, or `{text, show, default}`", n.Line)
	}
}
