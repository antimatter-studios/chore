package tmpl

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

const profilesJSON = `{
  "work":     {"type": "claude", "directory": "~/.claude-w", "port": 8080, "on": true},
  "personal": {"type": "claude", "directory": "~/.claude-p", "ratio": 0.50, "note": null},
  "list":     ["a", "b"]
}`

const profilesYAML = `
work:
  type: claude
  directory: ~/.claude-w
  port: 8080
  on: true
personal:
  type: claude
  directory: ~/.claude-p
  ratio: 0.50
  note: null
list: [a, b]
`

// write puts content at dir/name and returns the path.
func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fileScope resolves one file variable into a scope whose TASKFILE_DIR is dir.
func fileScope(t *testing.T, dir string, v chorefile.Var) (*Scope, error) {
	t.Helper()
	base := New(nil).Push(map[string]string{"TASKFILE_DIR": dir})
	vals, err := base.ResolveValues(context.Background(), map[string]chorefile.Var{"P": v}, nil)
	if err != nil {
		return nil, err
	}
	return base.PushValues(vals), nil
}

// Both formats give the same tree, and a template walks it: nested fields,
// numbers as written, booleans, and a missing top-level variable still empty.
func TestFileVariableTemplatesWalkTheTree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "p.json", profilesJSON)
	write(t, dir, "p.yaml", profilesYAML)

	for _, v := range []chorefile.Var{
		{File: "p.json", FileFormat: "json"},
		{File: "{{.TASKFILE_DIR}}/p.yaml", FileFormat: "yaml"},
	} {
		s, err := fileScope(t, dir, v)
		if err != nil {
			t.Fatalf("%s: %v", v.FileFormat, err)
		}
		cases := map[string]string{
			"{{.P.work.directory}}":                            "~/.claude-w",
			"{{.P.work.port}}":                                 "8080",
			"{{.P.personal.ratio}}":                            "0.50",
			"{{if .P.work.on}}yes{{end}}":                      "yes",
			"{{index .P.list 1}}":                              "b",
			"{{range .P.list}}[{{.}}]{{end}}":                  "[a][b]",
			"{{.NOT_SET}}|{{.NOT_SET | default \"d\"}}":        "|d",
			"{{.TASKFILE_DIR}}":                                dir,
			"{{with index .P \"absent\"}}x{{else}}none{{end}}": "none",
			"{{.P.list}}":                                      `["a","b"]`,
			"{{.P.work}}":                                      `{"directory":"~/.claude-w","on":true,"port":8080,"type":"claude"}`,
		}
		for text, want := range cases {
			got, err := s.Render(text)
			if err != nil || got != want {
				t.Errorf("%s: Render(%s) = %q, %v; want %q", v.FileFormat, text, got, err, want)
			}
		}
	}
}

// The environment and `for:` see the file in the order it was written.
func TestFileVariableStringFormKeepsWrittenOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "p.json", profilesJSON)
	s, err := fileScope(t, dir, chorefile.Var{File: "p.json", FileFormat: "json"})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.Lookup("P")
	if v.Kind != Map || !reflect.DeepEqual(v.Keys, []string{"work", "personal", "list"}) {
		t.Errorf("for: would walk %v (kind %v), want work, personal, list", v.Keys, v.Kind)
	}
	if v.Items[2] != `["a","b"]` {
		t.Errorf("a nested value's for: item = %q, want its compact JSON", v.Items[2])
	}
	want := `{"work":{"type":"claude","directory":"~/.claude-w","port":8080,"on":true},` +
		`"personal":{"type":"claude","directory":"~/.claude-p","ratio":0.50,"note":null},"list":["a","b"]}`
	if got, _ := s.Get("P"); got != want {
		t.Errorf("$P = %s\nwant  %s", got, want)
	}
	if got := s.All()["P"]; got != want {
		t.Errorf("All()[P] = %s, want the same compact JSON", got)
	}
}

// A key the file does not have is an error naming it, not an empty string.
func TestFileVariableMissingKeyIsAnError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "p.json", profilesJSON)
	s, err := fileScope(t, dir, chorefile.Var{File: "p.json", FileFormat: "json"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Render("ssh {{.P.work.hots}}")
	if err == nil || !strings.Contains(err.Error(), "hots") {
		t.Errorf("Render of a misspelt key = %v; want an error naming it", err)
	}
}

// Without a file variable in scope nothing changes: strings only, and a missing
// key is still empty.
func TestNoFileVariableKeepsStringTemplates(t *testing.T) {
	t.Parallel()
	s := New(nil).Push(map[string]string{"A": "x"})
	got, err := s.Render("{{.A}}{{.MISSING}}")
	if err != nil || got != "x" {
		t.Errorf("Render = %q, %v; want %q", got, err, "x")
	}
}

func TestFileVariableErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "yaml.json", "a: 1\n")
	write(t, dir, "dup.json", `{"a": 1, "a": 2}`)
	write(t, dir, "dup.yaml", "a: 1\na: 2\n")
	write(t, dir, "scalar.json", `"just a string"`)
	write(t, dir, "trailing.json", `{"a": 1} {"b": 2}`)
	write(t, dir, "empty.yaml", "")
	write(t, dir, "two.yaml", "a: 1\n---\nb: 2\n")
	write(t, dir, "merge.yaml", "base: &b {x: 1}\nother:\n  <<: *b\n")

	cases := []struct {
		v    chorefile.Var
		want []string
	}{
		{chorefile.Var{File: "absent.json", FileFormat: "json"}, []string{"P:", "absent.json does not exist", "write it", `"?"`}},
		{chorefile.Var{File: "yaml.json", FileFormat: "json"}, []string{"yaml.json is not valid JSON"}},
		{chorefile.Var{File: "dup.json", FileFormat: "json"}, []string{`key "a" appears twice`}},
		{chorefile.Var{File: "dup.yaml", FileFormat: "yaml"}, []string{"not valid YAML"}},
		{chorefile.Var{File: "scalar.json", FileFormat: "json"}, []string{"holds a single value", "object or an array"}},
		{chorefile.Var{File: "trailing.json", FileFormat: "json"}, []string{"after the top-level value"}},
		{chorefile.Var{File: "empty.yaml", FileFormat: "yaml"}, []string{"empty"}},
		{chorefile.Var{File: "two.yaml", FileFormat: "yaml"}, []string{"more than one YAML document"}},
		{chorefile.Var{File: "merge.yaml", FileFormat: "yaml"}, []string{"merge keys"}},
		// "?" excuses a missing file, never a broken one.
		{chorefile.Var{File: "?yaml.json", FileFormat: "json"}, []string{"not valid JSON"}},
	}
	for _, tc := range cases {
		_, err := fileScope(t, dir, tc.v)
		if err == nil {
			t.Errorf("%s: no error, want %q", tc.v.File, tc.want)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q does not mention %q", tc.v.File, err, w)
			}
		}
	}
}

// "?" makes a missing file an empty object: it renders as {}, walks nothing,
// and `index` finds no key.
func TestOptionalMissingFileIsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := fileScope(t, dir, chorefile.Var{File: "?nope.yaml", FileFormat: "yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("P"); got != "{}" {
		t.Errorf("$P = %q, want {}", got)
	}
	got, err := s.Render(`{{with index .P "x"}}set{{else}}unset{{end}}`)
	if err != nil || got != "unset" {
		t.Errorf("Render = %q, %v; want unset", got, err)
	}
	v, _ := s.Lookup("P")
	if len(v.Keys) != 0 {
		t.Errorf("for: would walk %v, want nothing", v.Keys)
	}
}

// Another variable can be built from a file variable's fields, in either order
// of declaration: the reference is found and resolved first.
func TestVariableReadsAFileVariable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "p.json", profilesJSON)
	base := New(nil).Push(map[string]string{"TASKFILE_DIR": dir})
	vals, err := base.ResolveValues(context.Background(), map[string]chorefile.Var{
		"A_DIR": {Value: "{{.P.work.directory}}/x"},
		"P":     {File: "p.json", FileFormat: "json"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := vals["A_DIR"].String(); got != "~/.claude-w/x" {
		t.Errorf("A_DIR = %q, want ~/.claude-w/x", got)
	}
}
