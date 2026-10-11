package chorefile

import (
	"strings"
	"testing"
)

func TestPreconditionForms(t *testing.T) {
	f, err := Decode([]byte(`version: '3'
tasks:
  release:
    preconditions:
      - test -f app
      - sh: git diff --quiet HEAD
        msg: Commit first
      - task: on-main
        vars: {BRANCH: main}
        msg: Switch branches
`))
	if err != nil {
		t.Fatal(err)
	}
	p := f.Tasks["release"].Preconditions
	if len(p) != 3 || p[0].Sh != "test -f app" || p[1].Msg != "Commit first" || p[2].Task != "on-main" || p[2].Vars["BRANCH"].Value != "main" {
		t.Fatalf("decoded checks = %#v", p)
	}
}

func TestInvalidPreconditionsRefusedAtParseTime(t *testing.T) {
	for _, entry := range []string{
		"{sh: 'true', task: check}", "{sh: ''}", "{task: ''}",
		"{msg: explain}", "{sh: 'true', vars: {X: y}}",
		"{defer: 'true'}", "{cmds: ['true']}", "{cmd: 'true'}",
		"['true']", "null", "42", "true", "''",
	} {
		t.Run(entry, func(t *testing.T) {
			_, err := Decode([]byte("tasks:\n  work:\n    preconditions:\n      - " + entry + "\n"))
			if err == nil || !strings.Contains(err.Error(), "precondition") {
				t.Fatalf("invalid entry %s: got %v", entry, err)
			}
		})
	}
}
