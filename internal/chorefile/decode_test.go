package chorefile

import (
	"reflect"
	"strings"
	"testing"
)

// bq is a single backquote. Go has no escape for one inside a raw string
// literal, so every expected value containing a nested Go template has to be
// assembled with concatenation. Doing it here keeps those expectations readable
// and, more importantly, unambiguous: the test says exactly which bytes it
// wants, rather than relying on the reader to count quotes.
const bq = "`"

// checkErr asserts an expected error: err must be non-nil and its text must
// contain every substring in want. Substrings rather than exact text, because
// the value of these messages is that they name the offending key and the line,
// not their wording.
func checkErr(t *testing.T, err error, want []string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error containing %q, got nil", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not contain %q", err, w)
		}
	}
}

// --- Var ---------------------------------------------------------------

// decodeVar wraps one YAML value in the smallest legal Taskfile and decodes it.
//
// Every case here goes through Decode rather than calling UnmarshalYAML
// directly, because how strictly a custom unmarshaler behaves depends on the
// decoder that reached it — testing the type in isolation would test a path no
// Taskfile ever takes.
func decodeVar(t *testing.T, value string) (Var, error) {
	t.Helper()
	f, err := Decode([]byte("version: '3'\nvars:\n  V: " + value + "\n"))
	if err != nil {
		return Var{}, err
	}
	return f.Vars["V"], nil
}

func TestDecodeVar(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    Var
		wantErr []string
	}{
		{
			name:  "plain string",
			value: "rest-mail",
			want:  Var{Value: "rest-mail"},
		},
		{
			// Quoting is the author's only way to keep padding, so it must not be
			// trimmed on the way in.
			name:  "quoted string keeps its spaces",
			value: `'  spaced  '`,
			want:  Var{Value: "  spaced  "},
		},
		{
			// Real Taskfiles write `PORT: 25`, not `PORT: '25'`. YAML types that
			// as !!int; the templating layer only ever wants text, so rejecting it
			// would fail on valid input everyone writes.
			name:  "unquoted int becomes its string form",
			value: "25",
			want:  Var{Value: "25"},
		},
		{
			// The written form survives: 1.50 must not come back as "1.5", because
			// the value may be a version or a tag where the digit matters.
			name:  "unquoted float keeps its written form",
			value: "1.50",
			want:  Var{Value: "1.50"},
		},
		{
			name:  "unquoted bool becomes its string form",
			value: "true",
			want:  Var{Value: "true"},
		},
		{
			// `FOO:` with nothing after it is a declared-but-empty variable, which
			// is how a Taskfile documents a value the caller is expected to supply.
			name:  "null is the empty string",
			value: "",
			want:  Var{},
		},
		{
			name:  "sh mapping in flow form",
			value: "{sh: git rev-parse HEAD}",
			want:  Var{Sh: "git rev-parse HEAD"},
		},
		{
			name:  "sh mapping in block form",
			value: "\n    sh: docker compose ps -q",
			want:  Var{Sh: "docker compose ps -q"},
		},
		{
			// Value and Sh are alternatives: a dynamic var must not also arrive
			// with a literal, or the resolution layer has to guess.
			name:  "sh mapping leaves Value empty",
			value: "{sh: echo hi}",
			want:  Var{Sh: "echo hi"},
		},
		{
			// A misspelled key next to a valid `sh` is the dangerous case: the var
			// still decodes, so without a check the typo is invisible forever.
			name:    "unknown key alongside sh",
			value:   "{sh: date, shh: quiet}",
			wantErr: []string{"unknown field", `"shh"`, "line 3"},
		},
		{
			name:    "unknown key on its own",
			value:   "{value: 25}",
			wantErr: []string{"unknown field", `"value"`},
		},
		{
			name:    "mapping without sh",
			value:   "{}",
			wantErr: []string{"must set `sh`"},
		},
		{
			name:    "sequence is not a variable",
			value:   "[a, b]",
			wantErr: []string{"a variable must be a value or a mapping with `sh`"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeVar(t, tc.value)
			if len(tc.wantErr) > 0 {
				checkErr(t, err, tc.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got != tc.want {
				t.Errorf("Var = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// --- Cmd ---------------------------------------------------------------

// decodeCmd decodes a single `cmds:` list item.
func decodeCmd(t *testing.T, item string) (Cmd, error) {
	t.Helper()
	f, err := Decode([]byte("version: '3'\ntasks:\n  t:\n    cmds:\n      - " + item + "\n"))
	if err != nil {
		return Cmd{}, err
	}
	if n := len(f.Tasks["t"].Cmds); n != 1 {
		t.Fatalf("got %d cmds, want exactly 1", n)
	}
	return f.Tasks["t"].Cmds[0], nil
}

func TestDecodeCmd(t *testing.T) {
	cases := []struct {
		name    string
		item    string
		want    Cmd
		wantErr []string
	}{
		{
			// The overwhelmingly common form: 124 of rest-mail's cmds are strings.
			name: "plain string",
			item: "go build ./...",
			want: Cmd{Cmd: "go build ./..."},
		},
		{
			name: "explicit cmd mapping",
			item: "{cmd: go test ./...}",
			want: Cmd{Cmd: "go test ./..."},
		},
		{
			name: "cmd with ignore_error",
			item: "{cmd: docker rm -f box, ignore_error: true}",
			want: Cmd{Cmd: "docker rm -f box", IgnoreError: true},
		},
		{
			name: "task reference without vars",
			item: "{task: postgres:up}",
			want: Cmd{Task: "postgres:up"},
		},
		{
			name: "task reference with vars",
			item: "{task: postgres:up, vars: {PORT: 5432, IMAGE: 'postgres:17'}}",
			want: Cmd{
				Task: "postgres:up",
				Vars: map[string]Var{
					"PORT":  {Value: "5432"},
					"IMAGE": {Value: "postgres:17"},
				},
			},
		},
		{
			name: "task reference with silent and ignore_error",
			item: "{task: postgres:up, silent: true, ignore_error: true}",
			want: Cmd{Task: "postgres:up", Silent: true, IgnoreError: true},
		},
		{
			// `defer:` nests a whole command, so the shorthand string form has to
			// unwrap to a normal shell step that merely runs at the end.
			name: "defer of a shell command",
			item: "{defer: docker rm -f box}",
			want: Cmd{Cmd: "docker rm -f box", Defer: true},
		},
		{
			// The form that makes bringing a topology up safe: the teardown task
			// runs whether or not the body succeeded.
			name: "defer of a task reference",
			item: "{defer: {task: e2e:down}}",
			want: Cmd{Task: "e2e:down", Defer: true},
		},
		{
			// Everything the inner command said survives the unwrapping; only
			// Defer is added.
			name: "defer preserves the inner command's fields",
			item: "{defer: {task: e2e:down, vars: {NAME: box}, silent: true, ignore_error: true}}",
			want: Cmd{
				Task:        "e2e:down",
				Vars:        map[string]Var{"NAME": {Value: "box"}},
				Silent:      true,
				IgnoreError: true,
				Defer:       true,
			},
		},
		{
			// Both set means the author expected one of them to win; refusing is
			// the only answer that cannot silently run the wrong thing.
			name:    "cmd and task together",
			item:    "{cmd: echo hi, task: other}",
			wantErr: []string{"either `cmd` or `task`, not both"},
		},
		{
			name:    "neither cmd nor task",
			item:    "{silent: true}",
			wantErr: []string{"a command needs `cmd`, `task` or `defer`"},
		},
		{
			name:    "empty mapping",
			item:    "{}",
			wantErr: []string{"a command needs `cmd`, `task` or `defer`"},
		},
		{
			// A deferred command is a step of its own, never a modifier on a
			// sibling `cmd:`/`task:` in the same mapping.
			name:    "defer combined with cmd",
			item:    "{defer: docker rm -f box, cmd: echo hi}",
			wantErr: []string{"`defer` is a command of its own"},
		},
		{
			name:    "defer combined with task",
			item:    "{defer: {task: e2e:down}, task: other}",
			wantErr: []string{"`defer` is a command of its own"},
		},
		{
			name:    "unknown key",
			item:    "{cmd: echo hi, slient: true}",
			wantErr: []string{"unknown field", `"slient"`},
		},
		{
			name:    "sequence is not a command",
			item:    "[echo, hi]",
			wantErr: []string{"a command must be a string or a mapping"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeCmd(t, tc.item)
			if len(tc.wantErr) > 0 {
				checkErr(t, err, tc.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Cmd = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// --- Dep ---------------------------------------------------------------

// decodeDep decodes a single `deps:` list item.
func decodeDep(t *testing.T, item string) (Dep, error) {
	t.Helper()
	f, err := Decode([]byte("version: '3'\ntasks:\n  t:\n    deps:\n      - " + item + "\n"))
	if err != nil {
		return Dep{}, err
	}
	if n := len(f.Tasks["t"].Deps); n != 1 {
		t.Fatalf("got %d deps, want exactly 1", n)
	}
	return f.Tasks["t"].Deps[0], nil
}

func TestDecodeDep(t *testing.T) {
	cases := []struct {
		name    string
		item    string
		want    Dep
		wantErr []string
	}{
		{
			name: "plain string",
			item: "build",
			want: Dep{Task: "build"},
		},
		{
			name: "namespaced name",
			item: "postgres:up",
			want: Dep{Task: "postgres:up"},
		},
		{
			// The same task twice with different vars is two concurrent runs, so
			// call vars have to survive decoding intact.
			name: "task with vars",
			item: "{task: sign, vars: {KEY: dev, ROUNDS: 3}}",
			want: Dep{
				Task: "sign",
				Vars: map[string]Var{"KEY": {Value: "dev"}, "ROUNDS": {Value: "3"}},
			},
		},
		{
			name: "task with silent",
			item: "{task: build, silent: true}",
			want: Dep{Task: "build", Silent: true},
		},
		{
			// A mapping that only sets modifiers names nothing to run; accepting it
			// would add a dependency on the empty task name.
			name:    "mapping without task",
			item:    "{silent: true}",
			wantErr: []string{"a dependency needs `task`"},
		},
		{
			name:    "empty mapping",
			item:    "{}",
			wantErr: []string{"a dependency needs `task`"},
		},
		{
			name:    "empty string",
			item:    `''`,
			wantErr: []string{"empty dependency name"},
		},
		{
			name:    "unknown key",
			item:    "{task: build, slient: true}",
			wantErr: []string{"unknown field", `"slient"`},
		},
		{
			name:    "sequence is not a dependency",
			item:    "[a, b]",
			wantErr: []string{"a dependency must be a string or a mapping"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeDep(t, tc.item)
			if len(tc.wantErr) > 0 {
				checkErr(t, err, tc.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Dep = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// --- strictness --------------------------------------------------------

// TestDecodeRejectsUnknownFields is the reason Decode exists rather than a bare
// yaml.Unmarshal: Task ignores keys it does not recognise, which turns `slient:`
// into a task that quietly prints everything, and `sourcs:` into a build that
// never skips. Every level of the document has to be strict, including the ones
// reached through a custom unmarshaler.
func TestDecodeRejectsUnknownFields(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr []string
	}{
		{
			name:    "unknown top-level field",
			yaml:    "version: '3'\noutput: prefixed\n",
			wantErr: []string{"output", `unknown field "output" in a file`},
		},
		{
			// v2 syntax, and the single most likely thing to be pasted in from
			// another project.
			name:    "unknown top-level field from Task v2",
			yaml:    "version: '2'\nexpansions: 3\n",
			wantErr: []string{"expansions"},
		},
		{
			name:    "unknown field inside a task",
			yaml:    "version: '3'\ntasks:\n  build:\n    sourcs: ['*.go']\n",
			wantErr: []string{"sourcs", `unknown field "sourcs" in a task`},
		},
		{
			name:    "misspelled silent inside a task",
			yaml:    "version: '3'\ntasks:\n  build:\n    slient: true\n",
			wantErr: []string{"slient"},
		},
		{
			name:    "unknown field inside an include",
			yaml:    "version: '3'\nincludes:\n  pg:\n    taskfile: pg\n    internal: true\n",
			wantErr: []string{"internal", `unknown field "internal" in an include`},
		},
		{
			// Unsupported by design (see the spec's "explicitly not supported"
			// list); it must fail loudly rather than be dropped.
			name:    "for-expansion is rejected, not ignored",
			yaml:    "version: '3'\ntasks:\n  build:\n    cmds:\n      - for: [a, b]\n        cmd: echo {{.ITEM}}\n",
			wantErr: []string{"unknown field", `"for"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Decode([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("want an error, got %+v", f)
			}
			checkErr(t, err, tc.wantErr)
		})
	}
}

// TestDecodeEmptyTaskBody: `build:` with nothing under it is legal — a
// placeholder, or a name that only exists to be an alias target. YAML hands
// that back as a nil *Task, and every consumer of Project.Tasks dereferences
// without checking, so Decode has to substitute an empty task.
func TestDecodeEmptyTaskBody(t *testing.T) {
	f, err := Decode([]byte("version: '3'\ntasks:\n  build:\n  test:\n    cmds: [go test ./...]\n  lint: null\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	for _, name := range []string{"build", "lint"} {
		task, ok := f.Tasks[name]
		if !ok {
			t.Fatalf("task %q missing from %v", name, f.Tasks)
		}
		if task == nil {
			t.Fatalf("task %q is a nil *Task; callers dereference it unguarded", name)
		}
		if !reflect.DeepEqual(*task, Task{}) {
			t.Errorf("task %q = %+v, want a zero Task", name, *task)
		}
	}
	if got := len(f.Tasks["test"].Cmds); got != 1 {
		t.Errorf("the neighbouring real task lost its cmds: %d", got)
	}
}

// --- template preservation ---------------------------------------------

// TestDecodePreservesTemplates is load-bearing. rest-mail's `status` task passes
// a Go template through to `docker --format`, and writes it as {{`…`}} so chore's
// own templating layer emits the inner braces verbatim. If decoding normalises,
// re-quotes or otherwise rewrites the scalar by even one byte, the format string
// docker receives is different and the command silently produces nothing useful.
//
// The expected values are assembled in Go so there is no question which bytes
// are being asserted.
func TestDecodePreservesTemplates(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "single-quoted nested template",
			yaml: "version: '3'\ntasks:\n  ps:\n    cmds:\n      - docker ps --format '{{" + bq + "{{.Names}}\\t{{.Status}}" + bq + "}}'\n",
			// \t stays a literal backslash-t: a single-quoted YAML scalar has no
			// escapes, and docker's own formatter is what interprets it.
			want: `docker ps --format '{{` + bq + `{{.Names}}\t{{.Status}}` + bq + `}}'`,
		},
		{
			name: "block scalar keeps template, quoting and trailing newline",
			yaml: "version: '3'\ntasks:\n  ps:\n    cmds:\n      - |\n" +
				"        set -euo pipefail\n" +
				"        docker ps -a --filter \"name=^{{.PROJECT}}-\" \\\n" +
				"          --format '{{" + bq + "{{.Names}}\\t{{.State}}" + bq + "}}'\n",
			want: "set -euo pipefail\n" +
				`docker ps -a --filter "name=^{{.PROJECT}}-" \` + "\n" +
				`  --format '{{` + bq + `{{.Names}}\t{{.State}}` + bq + `}}'` + "\n",
		},
		{
			name: "plain unquoted template is untouched",
			yaml: "version: '3'\ntasks:\n  ps:\n    cmds:\n      - echo {{.CONFIG}} {{default \"none\" .TAG}}\n",
			want: `echo {{.CONFIG}} {{default "none" .TAG}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Decode([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			got := f.Tasks["ps"].Cmds[0].Cmd
			if got != tc.want {
				t.Errorf("cmd mangled:\n got %q\nwant %q", got, tc.want)
			}
		})
	}

	// The same guarantee has to hold for a var, because {{`…`}} appears in
	// `vars:` too — a format string is often defined once and reused.
	t.Run("var value", func(t *testing.T) {
		want := `{{` + bq + `{{.Names}}` + bq + `}}`
		f, err := Decode([]byte("version: '3'\nvars:\n  FORMAT: '" + want + "'\n"))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if got := f.Vars["FORMAT"].Value; got != want {
			t.Errorf("var mangled:\n got %q\nwant %q", got, want)
		}
	})
}

// --- end to end --------------------------------------------------------

// TestDecodeRealisticTaskfile decodes one file using every feature the schema
// covers and asserts the whole result at once. The per-field tests above can all
// pass while the pieces fail to compose; this is the case that would notice.
func TestDecodeRealisticTaskfile(t *testing.T) {
	const src = `version: '3'
silent: true

dotenv:
  - config/base.env
  - '?config/secrets.env'

includes:
  pg:
    taskfile: pg/Taskfile.yml
    dir: work/pg
    optional: true
    vars:
      IMAGE: 'postgres:17'
      PORT: 5432
  extra:
    taskfile: extra

vars:
  PROJECT: rest-mail
  REPLICAS: 3
  HEAD:
    sh: git rev-parse --short HEAD

env:
  DOCKER_BUILDKIT: 1

tasks:
  build:
    desc: build the binary
    aliases: [b]
    sources: ['cmd/**/*.go']
    generates: [bin/chore]
    cmds:
      - go build -o bin/chore .

  up:
    desc: bring the stack up
    args: [config]
    dir: deploy
    run: once
    deps:
      - build
      - task: pg:up
        vars:
          PORT: 5433
    cmds:
      - |
        set -euo pipefail
        docker compose up -d
      - task: wait
        vars: {TIMEOUT: 30s}
      - defer: {task: e2e:down}

  wait:
    internal: true
    silent: true
    status:
      - test -f .ready
    cmds:
      - cmd: sleep 1
        ignore_error: true
`

	want := &File{
		Version: "3",
		Silent:  true,
		Dotenv:  []string{"config/base.env", "?config/secrets.env"},
		Includes: map[string]*Include{
			"pg": {
				Taskfile: "pg/Taskfile.yml",
				Dir:      "work/pg",
				Optional: true,
				Vars: map[string]Var{
					"IMAGE": {Value: "postgres:17"},
					"PORT":  {Value: "5432"},
				},
			},
			"extra": {Taskfile: "extra"},
		},
		Vars: map[string]Var{
			"PROJECT":  {Value: "rest-mail"},
			"REPLICAS": {Value: "3"},
			"HEAD":     {Sh: "git rev-parse --short HEAD"},
		},
		Env: map[string]Var{
			"DOCKER_BUILDKIT": {Value: "1"},
		},
		Tasks: map[string]*Task{
			"build": {
				Desc:      "build the binary",
				Aliases:   []string{"b"},
				Sources:   []string{"cmd/**/*.go"},
				Generates: []string{"bin/chore"},
				Cmds:      []Cmd{{Cmd: "go build -o bin/chore ."}},
			},
			"up": {
				Desc: "bring the stack up",
				Args: Args{{Name: "config"}},
				Dir:  "deploy",
				Run:  "once",
				Deps: []Dep{
					{Task: "build"},
					{Task: "pg:up", Vars: map[string]Var{"PORT": {Value: "5433"}}},
				},
				Cmds: []Cmd{
					// A block scalar keeps its interior newlines and the final one:
					// the whole thing is handed to one shell invocation, so
					// `set -euo pipefail` governs the line below it.
					{Cmd: "set -euo pipefail\ndocker compose up -d\n"},
					{Task: "wait", Vars: map[string]Var{"TIMEOUT": {Value: "30s"}}},
					{Task: "e2e:down", Defer: true},
				},
			},
			"wait": {
				Internal: true,
				Silent:   true,
				Status:   []string{"test -f .ready"},
				Cmds:     []Cmd{{Cmd: "sleep 1", IgnoreError: true}},
			},
		},
	}

	got, err := Decode([]byte(src))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		// Report the first differing task before dumping everything, because a
		// whole-File diff of pointers is unreadable.
		for name, wantTask := range want.Tasks {
			gotTask, ok := got.Tasks[name]
			if !ok {
				t.Errorf("task %q missing", name)
				continue
			}
			if !reflect.DeepEqual(gotTask, wantTask) {
				t.Errorf("task %q:\n got %+v\nwant %+v", name, gotTask, wantTask)
			}
		}
		t.Fatalf("File:\n got %+v\nwant %+v", got, want)
	}

	// RunOnce is what the runner actually calls; `run: once` is worthless if it
	// does not reach it.
	if !got.Tasks["up"].RunOnce() {
		t.Error("up.RunOnce() = false, want true")
	}
	if got.Tasks["build"].RunOnce() {
		t.Error("build.RunOnce() = true, want false for the default `run`")
	}
	// The loader fills these in; Decode must not invent them.
	if got.Path != "" || got.Dir != "" {
		t.Errorf("Decode set Path=%q Dir=%q, want them left to the loader", got.Path, got.Dir)
	}
}

// TestDecodeEmptyInput records what an empty or blank Taskfile does: yaml
// reports EOF, so the message a user sees is "taskfile: EOF" with no path and no
// hint. Worth knowing before blaming the loader.
func TestDecodeEmptyInput(t *testing.T) {
	for _, src := range []string{"", "\n", "# just a comment\n"} {
		if _, err := Decode([]byte(src)); err == nil {
			t.Errorf("Decode(%q) = nil error, want one", src)
		}
	}
}

// A bare "-" in cmds: or deps: is rejected rather than dropped.
//
// yaml.v3 zero-fills a null element into a struct slice BEFORE any element
// unmarshaler runs, so the entry used to vanish with no error at all: a task
// would quietly run one step fewer than it was written to. Named slice types on
// the schema exist solely to catch this.
func TestDecodeNullListElementIsRejected(t *testing.T) {
	for _, tt := range []struct {
		name, yaml, want string
	}{
		{
			name: "null dep",
			yaml: "version: '3'\ntasks:\n  t:\n    deps:\n      -\n      - build\n",
			want: "deps entry 1 is empty",
		},
		{
			name: "null cmd in the middle",
			yaml: "version: '3'\ntasks:\n  t:\n    cmds:\n      - echo one\n      -\n      - echo two\n",
			want: "cmds entry 2 is empty",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Decode = nil error, want the empty entry to be rejected")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

// A parameter name that cannot be referenced as {{.Name}} is rejected at decode
// time. The empty case is not hypothetical: `- !config` is YAML tag syntax and
// decodes to "", so without this the task would silently receive nothing under a
// name nobody can write.
func TestDecodeRejectsUnusableParameterNames(t *testing.T) {
	for _, tt := range []struct{ name, yaml, want string }{
		{
			name: "yaml tag decodes to an empty name",
			yaml: "version: '3'\ntasks:\n  up:\n    args:\n      - !config\n",
			want: "YAML tag syntax",
		},
		{
			name: "punctuation cannot be a template variable",
			yaml: "version: '3'\ntasks:\n  up:\n    args: ['con-fig']\n",
			want: "cannot be used as a variable",
		},
		{
			name: "a leading digit cannot either",
			yaml: "version: '3'\ntasks:\n  up:\n    args: ['2fast']\n",
			want: "cannot be used as a variable",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Decode = nil error, want the bad parameter name rejected")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A `short:` that cannot work is refused where it is written. Each of these is
// silent otherwise: a multi-letter short never matches, two parameters sharing a
// letter means one of them can never be reached, and `short: h` is shadowed by
// chore's own -h, which is answered before a task is invoked.
func TestDecodeRejectsUnusableShortFlags(t *testing.T) {
	for _, tt := range []struct{ name, yaml, want string }{
		{
			name: "more than one letter",
			yaml: "version: '3'\ntasks:\n  up:\n    args:\n      - {name: force, short: fo}\n",
			want: "a short flag is a single letter",
		},
		{
			name: "a digit would collide with a negative number",
			yaml: "version: '3'\ntasks:\n  up:\n    args:\n      - {name: force, short: '5'}\n",
			want: "a short flag is a single letter",
		},
		{
			name: "two parameters cannot share a letter",
			yaml: "version: '3'\ntasks:\n  up:\n    args:\n      - {name: force, short: f}\n      - {name: follow, short: f}\n",
			want: "both declare short",
		},
		{
			name: "h is chore's help flag",
			yaml: "version: '3'\ntasks:\n  up:\n    args:\n      - {name: halt, short: h}\n",
			want: "unreachable",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Decode = nil error, want the bad short rejected")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A usable short decodes, and case distinguishes two of them: -v and -V are
// different flags everywhere else, so they are here too.
func TestDecodeAcceptsShortFlags(t *testing.T) {
	f, err := Decode([]byte("version: '3'\ntasks:\n  up:\n    args:\n" +
		"      - {name: verbose, short: v, type: bool}\n" +
		"      - {name: version, short: V}\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	args := f.Tasks["up"].Args
	if got := args.Shorts(); !reflect.DeepEqual(got, []string{"-v", "-V"}) {
		t.Errorf("Shorts() = %q, want [-v -V]", got)
	}
	if !args[0].IsBool() || args[1].IsBool() {
		t.Errorf("types did not survive alongside short: %+v", args)
	}
}

// chore_min_version has to be a version, checked where it is written. A floor
// nobody can compare is worse than none: it reads as a guarantee and enforces
// nothing.
func TestDecodeRejectsAMalformedVersionFloor(t *testing.T) {
	for _, bad := range []string{"banana", "0.4", "0.4.0.1", "0.4.0-rc1", "latest", ""} {
		yml := "version: '3'\nchore_min_version: '" + bad + "'\ntasks:\n  t:\n    cmds: [echo hi]\n"
		f, err := Decode([]byte(yml))
		if bad == "" {
			// absent and empty both mean "no restriction"
			if err != nil {
				t.Errorf("empty floor: Decode = %v, want it accepted as no restriction", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("chore_min_version %q was accepted; want it rejected", bad)
			continue
		}
		if !strings.Contains(err.Error(), "not a version") {
			t.Errorf("floor %q: error = %q, want it to say it is not a version", bad, err)
		}
		_ = f
	}
}

// ParseSemver must compare NUMERICALLY. As strings, "0.10.0" < "0.4.0", which
// would reject a newer chore than the floor asks for.
func TestParseSemverAndOrdering(t *testing.T) {
	for _, c := range []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"0.4.0", [3]int{0, 4, 0}, true},
		{"v0.4.0", [3]int{0, 4, 0}, true},
		{" 0.4.0 ", [3]int{0, 4, 0}, true},
		{"0.10.0", [3]int{0, 10, 0}, true},
		{"1.2.3", [3]int{1, 2, 3}, true},
		// chore's own dev stamp is deliberately NOT a version
		{"dev", [3]int{}, false},
		{"dev+a581449", [3]int{}, false},
		{"dev+a581449-dirty", [3]int{}, false},
		{"0.4", [3]int{}, false},
		{"0.4.x", [3]int{}, false},
		{"", [3]int{}, false},
	} {
		got, ok := ParseSemver(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseSemver(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}

	older, _ := ParseSemver("0.4.0")
	newer, _ := ParseSemver("0.10.0")
	if !SemverLess(older, newer) {
		t.Error("0.4.0 must sort before 0.10.0 — string comparison gets this backwards")
	}
	if SemverLess(newer, older) {
		t.Error("0.10.0 must not sort before 0.4.0")
	}
	if SemverLess(older, older) {
		t.Error("equal versions: neither is less, so an equal version satisfies a floor")
	}
}

// A well-formed list is unaffected by that check.
func TestDecodeListsWithoutNullsStillDecode(t *testing.T) {
	f, err := Decode([]byte("version: '3'\ntasks:\n  t:\n    deps:\n      - build\n    cmds:\n      - echo hi\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := f.Tasks["t"].Deps; len(got) != 1 || got[0].Task != "build" {
		t.Errorf("deps = %+v, want just build", got)
	}
	if got := f.Tasks["t"].Cmds; len(got) != 1 || got[0].Cmd != "echo hi" {
		t.Errorf("cmds = %+v, want just echo hi", got)
	}
}

// The per-task hooks and child_hooks are ordinary struct fields, so the point of
// these is the grammar they add: that the four names are accepted where a task
// setting goes, that child_hooks distinguishes "absent" from "true", and that a
// `defer:` inside a hook is refused rather than silently run as a step.
func TestDecodeTaskHooks(t *testing.T) {
	f, err := Decode([]byte(`
version: '3'
tasks:
  build:
    child_hooks: false
    before:     [ ./check.sh ]
    cmds:       [ make ]
    on_success: [ ./publish.sh ]
    on_failure: [ {task: collect:logs} ]
    after:      [ 'echo {{.EXIT_CODE}}' ]
  plain:
    cmds: [ true ]
`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	b := f.Tasks["build"]
	if len(b.Before) != 1 || b.Before[0].Cmd != "./check.sh" {
		t.Errorf("before = %+v", b.Before)
	}
	if len(b.OnSuccess) != 1 || b.OnSuccess[0].Cmd != "./publish.sh" {
		t.Errorf("on_success = %+v", b.OnSuccess)
	}
	if len(b.OnFailure) != 1 || b.OnFailure[0].Task != "collect:logs" {
		t.Errorf("on_failure = %+v", b.OnFailure)
	}
	if len(b.After) != 1 || b.After[0].Cmd != "echo {{.EXIT_CODE}}" {
		t.Errorf("after = %+v", b.After)
	}
	if !b.HasHooks() {
		t.Error("HasHooks() = false for a task that declares four")
	}
	if !b.SuppressesChildHooks() {
		t.Error("child_hooks: false must suppress")
	}
	// Absent is not the same as false: a task that says nothing must not silence
	// its children.
	p := f.Tasks["plain"]
	if p.ChildHooks != nil {
		t.Errorf("ChildHooks = %v, want nil when undeclared", *p.ChildHooks)
	}
	if p.SuppressesChildHooks() {
		t.Error("an undeclared child_hooks must not suppress")
	}
	if p.HasHooks() {
		t.Error("HasHooks() = true for a task with no hooks")
	}
}

func TestDecodeRejectsDeferInsideAHook(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{
			name: "task hook",
			yaml: "version: '3'\ntasks:\n  x:\n    cmds: [true]\n    after:\n      - defer: echo nope\n",
			want: `task "x": after step 1 is a ` + "`defer:`",
		},
		{
			name: "lifecycle hook",
			yaml: "version: '3'\nlifecycle:\n  after_all:\n    - defer: echo nope\ntasks:\n  x:\n    cmds: [true]\n",
			want: "lifecycle: after_all step 1 is a " + "`defer:`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.yaml))
			if err == nil {
				t.Fatal("a `defer:` inside a hook must be refused, not run as an ordinary step")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
