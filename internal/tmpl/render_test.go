package tmpl

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/rest-mail/go-tsk/internal/taskfile"
)

func TestRender(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		vars map[string]string
		text string
		want string
	}{
		{
			name: "empty text",
			text: "",
			want: "",
		},
		{
			name: "no actions is returned verbatim",
			vars: map[string]string{"A": "1"},
			text: "docker compose up -d --wait }} not an action",
			want: "docker compose up -d --wait }} not an action",
		},
		{
			name: "simple substitution",
			vars: map[string]string{"CONFIG": "mail4.test"},
			text: "config/{{.CONFIG}}/config.env",
			want: "config/mail4.test/config.env",
		},
		{
			name: "several substitutions",
			vars: map[string]string{"NS": "restmail", "SVC": "smtp"},
			text: "{{.NS}}-{{.SVC}}-1",
			want: "restmail-smtp-1",
		},

		// A reference to a variable nobody defined is empty, never an error:
		// Taskfiles are written assuming this.
		{
			name: "undefined variable renders empty",
			text: "[{{.NOPE}}]",
			want: "[]",
		},
		{
			name: "undefined variable in the middle of a path",
			text: "{{.ROOT_DIR}}/bin/tsk",
			want: "/bin/tsk",
		},

		// `default`: the one function.
		{
			name: "default fills in a missing key",
			text: `{{.CONFIG | default "restmail.test"}}`,
			want: "restmail.test",
		},
		{
			name: "default fills in an empty string",
			vars: map[string]string{"CONFIG": ""},
			text: `{{.CONFIG | default "restmail.test"}}`,
			want: "restmail.test",
		},
		{
			name: "default yields to a real value",
			vars: map[string]string{"CONFIG": "mail4.test"},
			text: `{{.CONFIG | default "restmail.test"}}`,
			want: "mail4.test",
		},
		{
			// sprig would swallow these. A variable set to "0" was set on
			// purpose; replacing it with the default is a silent wrong answer.
			name: "default keeps the string 0",
			vars: map[string]string{"REPLICAS": "0"},
			text: `{{.REPLICAS | default "1"}}`,
			want: "0",
		},
		{
			name: "default keeps the string false",
			vars: map[string]string{"TLS": "false"},
			text: `{{.TLS | default "true"}}`,
			want: "false",
		},
		{
			name: "default keeps whitespace",
			vars: map[string]string{"SEP": " "},
			text: `[{{.SEP | default "-"}}]`,
			want: "[ ]",
		},
		{
			name: "default in positional form",
			vars: map[string]string{"CONFIG": "mail4.test"},
			text: `{{default "restmail.test" .CONFIG}}`,
			want: "mail4.test",
		},
		{
			name: "default falling back to another variable",
			vars: map[string]string{"FALLBACK": "second"},
			text: `{{.PRIMARY | default .FALLBACK | default "third"}}`,
			want: "second",
		},
		{
			name: "chained defaults all missing",
			text: `{{.PRIMARY | default .FALLBACK | default "third"}}`,
			want: "third",
		},

		// Nested escaping. Real Taskfiles hand another program's Go template
		// through untouched: `docker ps --format '{{.Names}}'`.
		{
			name: "backquoted raw string renders literally",
			text: "docker ps --format '{{`{{.Names}}`}}'",
			want: "docker ps --format '{{.Names}}'",
		},
		{
			name: "raw string is not substituted even when the name exists",
			vars: map[string]string{"Names": "SHOULD-NOT-APPEAR"},
			text: "{{`{{.Names}}`}}",
			want: "{{.Names}}",
		},
		{
			name: "raw string beside a real substitution",
			vars: map[string]string{"NS": "restmail"},
			text: "docker ps --filter name={{.NS}} --format '{{`{{.Names}}\t{{.Status}}`}}'",
			want: "docker ps --filter name=restmail --format '{{.Names}}\t{{.Status}}'",
		},

		// Native control flow.
		{
			name: "if true branch",
			vars: map[string]string{"DETACH": "yes"},
			text: "up{{if .DETACH}} -d{{end}}",
			want: "up -d",
		},
		{
			name: "if false branch on empty value",
			vars: map[string]string{"DETACH": ""},
			text: "up{{if .DETACH}} -d{{end}}",
			want: "up",
		},
		{
			name: "if else with a missing key",
			text: `{{if .VERBOSE}}--verbose{{else}}--quiet{{end}}`,
			want: "--quiet",
		},
		{
			name: "if else if",
			vars: map[string]string{"LEVEL": "warn"},
			text: `{{if eq .LEVEL "debug"}}-vv{{else if eq .LEVEL "warn"}}-v{{else}}{{end}}`,
			want: "-v",
		},
		{
			// range over the scope itself: text/template visits map keys in
			// sorted order, so this is deterministic.
			name: "range over the flattened scope",
			vars: map[string]string{"B": "2", "A": "1", "C": "3"},
			text: "{{range $k, $v := .}}{{$k}}={{$v}};{{end}}",
			want: "A=1;B=2;C=3;",
		},
		{
			name: "range with else on an empty scope",
			text: "{{range $k, $v := .}}{{$k}}{{else}}none{{end}}",
			want: "none",
		},
		{
			name: "with block",
			vars: map[string]string{"DIR": "/srv"},
			text: "{{with .DIR}}cd {{.}}{{end}}",
			want: "cd /srv",
		},

		// Builtins stay available; only sprig is absent.
		{
			name: "index builtin",
			vars: map[string]string{"A": "1"},
			text: `{{index . "A"}}`,
			want: "1",
		},
		{
			name: "printf builtin",
			vars: map[string]string{"H": "mail", "D": "test"},
			text: `{{printf "%s.%s" .H .D}}`,
			want: "mail.test",
		},
		{
			name: "len and eq builtins",
			vars: map[string]string{"S": "abc"},
			text: `{{if eq (len .S) 3}}three{{end}}`,
			want: "three",
		},
		{
			name: "multiline template",
			vars: map[string]string{"NS": "restmail"},
			text: "set -e\ndocker network inspect {{.NS}}\n",
			want: "set -e\ndocker network inspect restmail\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := New(nil).Push(tc.vars)
			got, err := s.Render(tc.text)
			if err != nil {
				t.Fatalf("Render(%q) error: %v", tc.text, err)
			}
			if got != tc.want {
				t.Errorf("Render(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestRenderErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		wantWord string // a substring the message must carry
	}{
		{name: "unterminated action", text: "{{.A", wantWord: "parsing template"},
		{name: "unclosed if", text: "{{if .A}}yes", wantWord: "parsing template"},
		{name: "unknown function", text: "{{ nosuchfunc .A }}", wantWord: "not defined"},
		{
			// Proof there is no sprig: a Taskfile using it must fail loudly, not
			// render something plausible.
			name:     "sprig function is absent",
			text:     `{{ upper .A }}`,
			wantWord: "not defined",
		},
		{name: "bad pipeline", text: "{{ | .A }}", wantWord: "parsing template"},
		{
			// Parses, then fails at execution: scope values are strings, so a
			// nested field cannot exist. Different message, different path.
			name:     "field of a string value",
			text:     "{{.A.B}}",
			wantWord: "rendering template",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(nil).Render(tc.text)
			if err == nil {
				t.Fatalf("Render(%q) succeeded, want an error", tc.text)
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error %q does not mention %q", err, tc.wantWord)
			}
			if !strings.Contains(err.Error(), strings.TrimSpace(tc.text[:3])) {
				t.Errorf("error %q does not quote the offending text %q", err, tc.text)
			}
		})
	}
}

func TestRenderDoesNotMutateScope(t *testing.T) {
	t.Parallel()

	s := New([]string{"A=1"}).Push(map[string]string{"B": "2"})
	before := s.All()

	first, err := s.Render(`{{.A}}-{{.B}}-{{.MISSING | default "d"}}`)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	second, err := s.Render(`{{.A}}-{{.B}}-{{.MISSING | default "d"}}`)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if first != "1-2-d" {
		t.Errorf("Render = %q, want 1-2-d", first)
	}
	if first != second {
		t.Errorf("Render is not deterministic: %q then %q", first, second)
	}
	if after := s.All(); !maps.Equal(before, after) {
		t.Errorf("scope changed: %v → %v", before, after)
	}
}

func TestIsEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		v    any
		want bool
	}{
		{name: "nil", v: nil, want: true},
		{name: "empty string", v: "", want: true},
		{name: "space", v: " ", want: false},
		{name: "zero string", v: "0", want: false},
		{name: "false string", v: "false", want: false},
		{name: "int zero is not empty", v: 0, want: false},
		{name: "bool false is not empty", v: false, want: false},
		{name: "empty slice", v: []string{}, want: true},
		{name: "empty map", v: map[string]string{}, want: true},
		{name: "nil pointer", v: (*string)(nil), want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isEmpty(tc.v); got != tc.want {
				t.Errorf("isEmpty(%#v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

func TestReferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want []string
	}{
		{name: "no actions", text: "docker ps", want: nil},
		{name: "plain field", text: "{{.CONFIG}}", want: []string{"CONFIG"}},
		{name: "deduplicated", text: "{{.A}}{{.A}}{{.B}}", want: []string{"A", "B"}},
		{name: "inside default", text: `{{.A | default .B}}`, want: []string{"A", "B"}},
		{name: "inside if else", text: "{{if .A}}{{.B}}{{else}}{{.C}}{{end}}", want: []string{"A", "B", "C"}},
		{name: "inside range", text: "{{range $k, $v := .M}}{{$k}}{{end}}", want: []string{"M"}},
		{name: "inside with", text: "{{with .W}}{{.}}{{end}}", want: []string{"W"}},
		{name: "nested field takes the root", text: "{{.A.B.C}}", want: []string{"A"}},
		{name: "chained field on a parenthesised pipe", text: "{{(.A).B}}", want: []string{"A"}},
		{name: "inside a template action", text: `{{template "sub" .A}}`, want: []string{"A"}},
		{
			// The whole reason references() parses instead of scanning: this is
			// another program's template, not a reference of ours.
			name: "raw string is not a reference",
			text: "{{`{{.Names}}`}}",
			want: nil,
		},
		{
			name: "dot in literal text is not a reference",
			text: "config/.env {{.CONFIG}}",
			want: []string{"CONFIG"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := references(tc.text)
			if err != nil {
				t.Fatalf("references(%q): %v", tc.text, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("references(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// fakeCapturer stands in for internal/shell.Shell: it records the scripts it was
// asked to run and answers from a canned table, so these tests never touch a
// shell, a process, or the network.
type fakeCapturer struct {
	out     map[string]string // rendered script → stdout
	err     error             // if set, every call fails with it
	scripts []string          // every script seen, in order
	ctx     context.Context   // the context of the last call
}

func (f *fakeCapturer) Capture(ctx context.Context, script string) (string, error) {
	f.scripts = append(f.scripts, script)
	f.ctx = ctx
	if f.err != nil {
		return "", f.err
	}
	return f.out[script], nil
}

func TestResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		base      map[string]string // variables already in scope
		vars      map[string]taskfile.Var
		canned    map[string]string // script → stdout
		want      map[string]string
		wantCalls []string // scripts the shell should have been given
	}{
		{
			name: "no vars",
			vars: nil,
			want: map[string]string{},
		},
		{
			name: "literal",
			vars: map[string]taskfile.Var{"CONFIG": {Value: "mail4.test"}},
			want: map[string]string{"CONFIG": "mail4.test"},
		},
		{
			name: "literal reads the enclosing scope",
			base: map[string]string{"ROOT_DIR": "/srv/rest-mail"},
			vars: map[string]taskfile.Var{"BIN": {Value: "{{.ROOT_DIR}}/bin"}},
			want: map[string]string{"BIN": "/srv/rest-mail/bin"},
		},
		{
			name: "literal with default",
			vars: map[string]taskfile.Var{"CONFIG": {Value: `{{.CONFIG | default "restmail.test"}}`}},
			want: map[string]string{"CONFIG": "restmail.test"},
		},
		{
			name:      "sh var is captured and trimmed",
			vars:      map[string]taskfile.Var{"REV": {Sh: "git rev-parse HEAD"}},
			canned:    map[string]string{"git rev-parse HEAD": "  4f1c2ab\n\n"},
			want:      map[string]string{"REV": "4f1c2ab"},
			wantCalls: []string{"git rev-parse HEAD"},
		},
		{
			name:      "sh script is itself rendered",
			base:      map[string]string{"DOMAIN": "mail4.test"},
			vars:      map[string]taskfile.Var{"IP": {Sh: "dig +short {{.DOMAIN}}"}},
			canned:    map[string]string{"dig +short mail4.test": "10.99.0.5\n"},
			want:      map[string]string{"IP": "10.99.0.5"},
			wantCalls: []string{"dig +short mail4.test"},
		},
		{
			name: "sh script rendered from a sibling var",
			vars: map[string]taskfile.Var{
				"CONFIG": {Value: "mail4.test"},
				"IP":     {Sh: "dig +short {{.CONFIG}}"},
			},
			canned:    map[string]string{"dig +short mail4.test": "10.99.0.5\n"},
			want:      map[string]string{"CONFIG": "mail4.test", "IP": "10.99.0.5"},
			wantCalls: []string{"dig +short mail4.test"},
		},
		{
			name: "sh wins when both forms are set",
			vars: map[string]taskfile.Var{"V": {Value: "literal", Sh: "echo shell"}},
			canned: map[string]string{
				"echo shell": "shell\n",
			},
			want:      map[string]string{"V": "shell"},
			wantCalls: []string{"echo shell"},
		},
		{
			name:      "sh producing nothing yields empty",
			vars:      map[string]taskfile.Var{"V": {Sh: "true"}},
			canned:    map[string]string{},
			want:      map[string]string{"V": ""},
			wantCalls: []string{"true"},
		},
		{
			name: "var references an earlier var",
			vars: map[string]taskfile.Var{
				"BASE": {Value: "restmail"},
				"FULL": {Value: "{{.BASE}}-smtp"},
			},
			want: map[string]string{"BASE": "restmail", "FULL": "restmail-smtp"},
		},
		{
			// Sorted-first name depends on a sorted-last one, so this only works
			// if resolution really is iterative rather than one ordered pass.
			name: "var references a var that sorts after it",
			vars: map[string]taskfile.Var{
				"A": {Value: "{{.Z}}!"},
				"Z": {Value: "z"},
			},
			want: map[string]string{"A": "z!", "Z": "z"},
		},
		{
			name: "three deep chain resolved backwards",
			vars: map[string]taskfile.Var{
				"A": {Value: "{{.B}}1"},
				"B": {Value: "{{.C}}2"},
				"C": {Value: "3"},
			},
			want: map[string]string{"A": "321", "B": "32", "C": "3"},
		},
		{
			name: "sh var depends on a literal that sorts after it",
			vars: map[string]taskfile.Var{
				"A_IMAGE": {Sh: "echo {{.Z_TAG}}"},
				"Z_TAG":   {Value: "2026.07.26"},
			},
			canned:    map[string]string{"echo 2026.07.26": "restmail:2026.07.26"},
			want:      map[string]string{"A_IMAGE": "restmail:2026.07.26", "Z_TAG": "2026.07.26"},
			wantCalls: []string{"echo 2026.07.26"},
		},
		{
			// Extending an inherited value is not a cycle: .PATH reads the layer
			// below, which is already resolved.
			name: "self reference reads the enclosing scope",
			base: map[string]string{"PATH": "/usr/bin"},
			vars: map[string]taskfile.Var{"PATH": {Value: "{{.PATH}}:/opt/bin"}},
			want: map[string]string{"PATH": "/usr/bin:/opt/bin"},
		},
		{
			name: "self reference with no inherited value",
			vars: map[string]taskfile.Var{"EXTRA": {Value: "{{.EXTRA}}-suffix"}},
			want: map[string]string{"EXTRA": "-suffix"},
		},
		{
			// A regex-based dependency scan would call this a cycle. Both are
			// just literals bound for another program.
			name: "raw strings never create a dependency",
			vars: map[string]taskfile.Var{
				"A": {Value: "{{`{{.B}}`}}"},
				"B": {Value: "{{`{{.A}}`}}"},
			},
			want: map[string]string{"A": "{{.B}}", "B": "{{.A}}"},
		},
		{
			name: "docker format var",
			vars: map[string]taskfile.Var{
				"FORMAT": {Value: "table {{`{{.Names}}\t{{.Status}}`}}"},
			},
			want: map[string]string{"FORMAT": "table {{.Names}}\t{{.Status}}"},
		},
		{
			name: "unknown reference resolves empty",
			vars: map[string]taskfile.Var{"V": {Value: "[{{.NOPE}}]"}},
			want: map[string]string{"V": "[]"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			base := New(nil).Push(tc.base)
			before := base.All()
			cap := &fakeCapturer{out: tc.canned}

			got, err := base.Resolve(t.Context(), tc.vars, cap)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("Resolve = %v, want %v", got, tc.want)
			}
			if !slices.Equal(cap.scripts, tc.wantCalls) {
				t.Errorf("shell saw %q, want %q", cap.scripts, tc.wantCalls)
			}
			// Resolve returns values; it never writes them back into the scope
			// it was called on — that is the caller's decision.
			if after := base.All(); !maps.Equal(before, after) {
				t.Errorf("Resolve mutated the scope: %v → %v", before, after)
			}
		})
	}
}

func TestResolveErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("exit status 127: dig: command not found")

	tests := []struct {
		name      string
		vars      map[string]taskfile.Var
		cap       Capturer
		noShell   bool // pass a nil Capturer, as the runner would with no shell
		wantWords []string
	}{
		{
			name: "two var cycle names both",
			vars: map[string]taskfile.Var{
				"A": {Value: "{{.B}}"},
				"B": {Value: "{{.A}}"},
			},
			wantWords: []string{"cycle", "A references B", "B references A"},
		},
		{
			name: "three var cycle names all three",
			vars: map[string]taskfile.Var{
				"A": {Value: "{{.B}}"},
				"B": {Value: "{{.C}}"},
				"C": {Value: "{{.A}}"},
			},
			wantWords: []string{"cycle", "A references B", "B references C", "C references A"},
		},
		{
			name: "cycle through an sh script",
			vars: map[string]taskfile.Var{
				"A": {Sh: "echo {{.B}}"},
				"B": {Value: "{{.A}}"},
			},
			wantWords: []string{"cycle", "A references B", "B references A"},
		},
		{
			name: "cycle reported alongside resolvable vars",
			vars: map[string]taskfile.Var{
				"OK": {Value: "fine"},
				"A":  {Value: "{{.B}}"},
				"B":  {Value: "{{.A}}"},
			},
			wantWords: []string{"cycle", "A references B", "B references A"},
		},
		{
			name:      "capturer error names the var",
			vars:      map[string]taskfile.Var{"IP": {Sh: "dig +short mail4.test"}},
			cap:       &fakeCapturer{err: boom},
			wantWords: []string{"var IP", "dig +short mail4.test", "command not found"},
		},
		{
			name:      "missing shell names the var",
			vars:      map[string]taskfile.Var{"REV": {Sh: "git rev-parse HEAD"}},
			noShell:   true,
			wantWords: []string{"var REV", "git rev-parse HEAD", "shell"},
		},
		{
			name:      "unparseable value names the var",
			vars:      map[string]taskfile.Var{"BAD": {Value: "{{.A"}},
			wantWords: []string{"var BAD", "parsing template"},
		},
		{
			name:      "unparseable sh script names the var",
			vars:      map[string]taskfile.Var{"BAD": {Sh: "echo {{ nosuchfunc }}"}},
			wantWords: []string{"var BAD", "not defined"},
		},
		{
			name:      "unrenderable value names the var",
			vars:      map[string]taskfile.Var{"BAD": {Value: "{{.A.B}}"}},
			wantWords: []string{"var BAD", "rendering template"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := tc.cap
			if c == nil && !tc.noShell {
				c = &fakeCapturer{}
			}

			got, err := New(nil).Resolve(t.Context(), tc.vars, c)
			if err == nil {
				t.Fatalf("Resolve succeeded with %v, want an error", got)
			}
			if got != nil {
				t.Errorf("Resolve returned %v alongside an error, want nil", got)
			}
			for _, w := range tc.wantWords {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestResolvePassesContextToTheShell(t *testing.T) {
	t.Parallel()

	type key struct{}
	ctx := context.WithValue(t.Context(), key{}, "carried")
	cap := &fakeCapturer{out: map[string]string{"whoami": "root"}}

	if _, err := New(nil).Resolve(ctx, map[string]taskfile.Var{"U": {Sh: "whoami"}}, cap); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := cap.ctx.Value(key{}); got != "carried" {
		t.Errorf("shell got context value %v, want carried — cancellation would not reach it", got)
	}
}

func TestResolveIsDeterministic(t *testing.T) {
	t.Parallel()

	// Map iteration order changes run to run; the result must not.
	vars := map[string]taskfile.Var{
		"A": {Value: "{{.B}}-{{.C}}"},
		"B": {Value: "{{.D}}b"},
		"C": {Value: "{{.D}}c"},
		"D": {Value: "d"},
	}
	want := map[string]string{"A": "db-dc", "B": "db", "C": "dc", "D": "d"}

	for range 20 {
		got, err := New(nil).Resolve(t.Context(), vars, &fakeCapturer{})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if !maps.Equal(got, want) {
			t.Fatalf("Resolve = %v, want %v", got, want)
		}
	}
}
