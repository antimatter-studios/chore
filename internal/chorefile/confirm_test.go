package chorefile

import (
	"strings"
	"testing"
)

func TestConfirmShortAndLongForms(t *testing.T) {
	f, err := Decode([]byte(`
tasks:
  short:
    confirm: 'Deploy {{.STACK}}?'
    cmds: ['true']
  long:
    confirm:
      text: 'Install into {{.CONTEXT}}?'
      show: kubectl config current-context
      default: yes
    cmds: ['true']
  none:
    cmds: ['true']
`))
	if err != nil {
		t.Fatal(err)
	}
	short := f.Tasks["short"].Confirm
	if short == nil || short.Text != "Deploy {{.STACK}}?" || short.Show != "" || short.Default {
		t.Errorf("short form = %+v", short)
	}
	long := f.Tasks["long"].Confirm
	if long == nil || long.Show != "kubectl config current-context" || !long.Default {
		t.Errorf("long form = %+v", long)
	}
	// nil, not an empty struct: "no question" has to be distinguishable from
	// "a question with no text", or every task would stop and ask.
	if f.Tasks["none"].Confirm != nil {
		t.Errorf("a task with no confirm: got %+v", f.Tasks["none"].Confirm)
	}
}

// `no` and `yes` are strings in YAML 1.2, so a bool field alone would refuse
// the spelling anybody would actually write.
func TestConfirmDefaultAcceptsYesAndNo(t *testing.T) {
	for _, c := range []struct {
		value string
		want  bool
	}{{"yes", true}, {"'yes'", true}, {"true", true}, {"y", true},
		{"no", false}, {"'no'", false}, {"false", false}, {"n", false}} {
		f, err := Decode([]byte("tasks:\n  work:\n    confirm: {text: q, default: " + c.value + "}\n"))
		if err != nil {
			t.Errorf("default: %s: %v", c.value, err)
			continue
		}
		if got := f.Tasks["work"].Confirm.Default; got != c.want {
			t.Errorf("default: %s = %v, want %v", c.value, got, c.want)
		}
	}
}

func TestConfirmRefusesNonsense(t *testing.T) {
	for name, c := range map[string]struct{ yaml, want string }{
		"empty question":  {"confirm: ''", "is a question"},
		"a list":          {"confirm: [a, b]", "is a question"},
		"no text":         {"confirm: {show: ls}", "non-empty `text:`"},
		"blank text":      {"confirm: {text: '   '}", "non-empty `text:`"},
		"empty show":      {"confirm: {text: q, show: ''}", "leave it out rather than empty"},
		"unknown field":   {"confirm: {text: q, answers: [yes, no]}", "unknown field"},
		"maybe":           {"confirm: {text: q, default: maybe}", "is yes or no"},
		"a number for it": {"confirm: 42", "is a question"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte("tasks:\n  work:\n    " + c.yaml + "\n"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: got %v, want an error mentioning %q", c.yaml, err, c.want)
			}
		})
	}
}
