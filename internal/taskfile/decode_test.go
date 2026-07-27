package taskfile

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
			wantErr: []string{"output", "taskfile.File"},
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
			wantErr: []string{"sourcs", "taskfile.Task"},
		},
		{
			name:    "misspelled silent inside a task",
			yaml:    "version: '3'\ntasks:\n  build:\n    slient: true\n",
			wantErr: []string{"slient"},
		},
		{
			name:    "unknown field inside an include",
			yaml:    "version: '3'\nincludes:\n  pg:\n    taskfile: pg\n    internal: true\n",
			wantErr: []string{"internal", "taskfile.Include"},
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
// a Go template through to `docker --format`, and writes it as {{`…`}} so tsk's
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
    generates: [bin/tsk]
    cmds:
      - go build -o bin/tsk ./cmd/tsk

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
				Generates: []string{"bin/tsk"},
				Cmds:      []Cmd{{Cmd: "go build -o bin/tsk ./cmd/tsk"}},
			},
			"up": {
				Desc: "bring the stack up",
				Args: []string{"config"},
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
