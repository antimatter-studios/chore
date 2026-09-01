package run

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// The tests in this file drive the real scheduler against a real shell. Every
// task writes a marker file into a temp directory the test owns, and the
// assertions read those files back — that is the only evidence a user ever gets
// that a task ran, ran once, or ran in the right order, so it is the evidence
// worth asserting on. Projects are built in Go rather than parsed from YAML so a
// loader change cannot make a scheduler test fail for a reason that has nothing
// to do with scheduling.

// syncBuf is an io.Writer safe for concurrent use. deps run in parallel and
// every shell they start shares the Runner's Out and Err, so a plain
// bytes.Buffer here would be a real data race, and -race would rightly fail.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fixture is a Runner over a one-file project rooted at a fresh temp directory.
type fixture struct {
	t    *testing.T
	dir  string
	file *chorefile.File
	r    *Runner
	out  *syncBuf
	err  *syncBuf
}

// newFixture wires a Project the way the loader would: the file knows its
// directory, every task knows its name and its file, and RootDir is the temp
// directory — which is also where every task runs, so a relative `> marker.txt`
// in a command lands somewhere the test can read it.
func newFixture(t *testing.T, file *chorefile.File, tasks map[string]*chorefile.Task) *fixture {
	t.Helper()
	dir := t.TempDir()
	if file == nil {
		file = &chorefile.File{}
	}
	file.Path = filepath.Join(dir, "Taskfile.yml")
	file.Dir = dir
	file.Tasks = tasks
	for name, task := range tasks {
		task.Name = name
		task.File = file
	}
	out, errOut := &syncBuf{}, &syncBuf{}
	project := &chorefile.Project{Root: file, Tasks: tasks, RootDir: dir}
	return &fixture{t: t, dir: dir, file: file, r: New(project, out, errOut), out: out, err: errOut}
}

func (f *fixture) run(name string, args []string, callVars map[string]string) error {
	f.t.Helper()
	return f.r.Run(context.Background(), name, args, callVars)
}

// mustRun fails the test on error, dumping both streams: when a shell command
// misbehaves the reason is almost always in the output, not in the error.
func (f *fixture) mustRun(name string, args []string, callVars map[string]string) {
	f.t.Helper()
	if err := f.run(name, args, callVars); err != nil {
		f.t.Fatalf("run %s: %v\nstdout:\n%s\nstderr:\n%s", name, err, f.out, f.err)
	}
}

func (f *fixture) mustFail(name string, args []string, callVars map[string]string) error {
	f.t.Helper()
	err := f.run(name, args, callVars)
	if err == nil {
		f.t.Fatalf("run %s: want an error, got nil\nstdout:\n%s\nstderr:\n%s", name, f.out, f.err)
	}
	return err
}

// write creates a file inside the project, for dotenv fixtures and the like.
func (f *fixture) write(rel, body string) {
	f.t.Helper()
	path := filepath.Join(f.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) mkdir(rel string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.dir, filepath.FromSlash(rel)), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

// read returns a marker file's contents, failing if the task never wrote it.
func (f *fixture) read(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(rel)))
	if err != nil {
		f.t.Fatalf("reading marker %s: %v\nstdout:\n%s\nstderr:\n%s", rel, err, f.out, f.err)
	}
	return string(b)
}

func (f *fixture) exists(rel string) bool {
	f.t.Helper()
	_, err := os.Stat(filepath.Join(f.dir, filepath.FromSlash(rel)))
	return err == nil
}

// cmds turns shell snippets into plain command steps.
func cmds(scripts ...string) []chorefile.Cmd {
	out := make([]chorefile.Cmd, 0, len(scripts))
	for _, s := range scripts {
		out = append(out, chorefile.Cmd{Cmd: s})
	}
	return out
}

// vars builds a `vars:` block from alternating key/value literals.
func vars(kv ...string) map[string]chorefile.Var {
	if len(kv)%2 != 0 {
		panic("vars: odd number of arguments")
	}
	out := map[string]chorefile.Var{}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = chorefile.Var{Value: kv[i+1]}
	}
	return out
}

func depsOn(names ...string) []chorefile.Dep {
	out := make([]chorefile.Dep, 0, len(names))
	for _, n := range names {
		out = append(out, chorefile.Dep{Task: n})
	}
	return out
}

func mustContain(t *testing.T, got, want, what string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s = %q, want it to contain %q", what, got, want)
	}
}

// ---------- 1. argument binding ----------

// TestArgsBindUnderTheNameDeclared pins a discrepancy between the code and its
// own documentation. SPEC "Fixed semantics" #1 and the CLI usage text both show
// `args: [config]` feeding `{{.CONFIG}}`, but bindArgs stores the value under
// the name the task declared, verbatim, and Go templates are case sensitive. So
// `args: [config]` populates `.config` and leaves `.CONFIG` empty — a Taskfile
// copied from the usage text would run with no config at all, which is exactly
// the silent-default failure this program exists to remove.
// A boolean parameter reads correctly in both consumers: {{if .FLAG}} must not
// fire on the string "false", and [ -n "$FLAG" ] must agree with it. So an unset
// boolean is EMPTY, not "false" — and the task's own vars must not put the raw
// literal back on top.
func TestBooleanParameterNormalisation(t *testing.T) {
	tasks := func() map[string]*chorefile.Task {
		return map[string]*chorefile.Task{
			"logs": {
				Args: chorefile.Args{{Name: "follow", Type: chorefile.TypeBool}},
				Vars: map[string]chorefile.Var{"follow": {Value: "false"}},
				Cmds: cmds(`printf 'tmpl=[%s] shell=[%s]' '{{if .FOLLOW}}on{{end}}' "${FOLLOW}" > out.txt`),
			},
		}
	}

	f := newFixture(t, nil, tasks())
	f.mustRun("logs", nil, nil)
	if got := f.read("out.txt"); got != "tmpl=[] shell=[]" {
		t.Errorf("unset: out.txt = %q, want both readings empty", got)
	}

	f2 := newFixture(t, nil, tasks())
	f2.mustRun("logs", nil, map[string]string{"FOLLOW": "true"})
	if got := f2.read("out.txt"); got != "tmpl=[on] shell=[true]" {
		t.Errorf("set: out.txt = %q, want both readings true", got)
	}
}

// TestInteractiveTaskGetsStdin: `interactive: true` hands the task chore's own
// terminal.
//
// Without it a task's stdin is /dev/null, because exec.Cmd with a nil Stdin
// wires the child to it. A script that prompts — `read -rs token` for a
// credential rotation — then gets EOF immediately and carries on with an empty
// answer it never received. Measured on a real Taskfile: `chore claude:login`
// printed its banner, showed nothing while a full-screen `claude setup-token`
// ran, and only flushed when the user pressed Ctrl-C.
func TestInteractiveTaskGetsStdin(t *testing.T) {
	tasks := func() map[string]*chorefile.Task {
		return map[string]*chorefile.Task{
			"ask": {
				Interactive: true,
				Cmds:        cmds(`read -r answer; printf 'got=[%s]' "$answer" > out.txt`),
			},
			"deaf": {
				Cmds: cmds(`read -r answer; printf 'got=[%s]' "$answer" > out.txt`),
			},
		}
	}

	t.Run("the task reads what the user types", func(t *testing.T) {
		f := newFixture(t, nil, tasks())
		f.r.Stdin = strings.NewReader("a-pasted-token\n")
		f.mustRun("ask", nil, nil)
		if got := f.read("out.txt"); got != "got=[a-pasted-token]" {
			t.Errorf("out.txt = %q, want the typed value", got)
		}
	})

	t.Run("without the flag stdin is empty, which is the bug", func(t *testing.T) {
		f := newFixture(t, nil, tasks())
		f.r.Stdin = strings.NewReader("a-pasted-token\n")
		f.mustRun("deaf", nil, nil)
		if got := f.read("out.txt"); got != "got=[]" {
			t.Errorf("out.txt = %q, want an empty read: a task that did not ask "+
				"for the terminal must not consume chore's stdin", got)
		}
	})
}

// A captured value is chore reading a command, not a human answering one. If a
// `sh:` var inherited the terminal it would swallow the keystrokes meant for the
// task, and the prompt that follows would read whatever was left.
func TestCaptureNeverConsumesStdin(t *testing.T) {
	f := newFixture(t, &chorefile.File{
		Vars: map[string]chorefile.Var{"CAPTURED": {Sh: "head -c 4"}},
	}, map[string]*chorefile.Task{
		"probe": {
			Interactive: true,
			Cmds:        cmds(`read -r answer; printf 'captured=[{{.CAPTURED}}] task=[%s]' "$answer" > out.txt`),
		},
	})
	f.r.Stdin = strings.NewReader("the-whole-line\n")
	f.mustRun("probe", nil, nil)
	if got := f.read("out.txt"); got != "captured=[] task=[the-whole-line]" {
		t.Errorf("out.txt = %q, want the capture empty and the task holding the whole line", got)
	}
}

// chore identifies itself in the environment so a Taskfile can distinguish the two
// runners — needed where a guard exists to catch a Task-specific trap.
func TestRunnerIdentifiesItself(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"probe": {Cmds: cmds(`printf '%s' "${CHORE}" > out.txt`)},
	})
	f.mustRun("probe", nil, nil)
	if got := f.read("out.txt"); got != "1" {
		t.Errorf("CHORE = %q, want 1", got)
	}
}

// A flag needs no default to be optional — absence is its value. Requiring one
// would mean typing `--follow=false` to say nothing.
func TestBooleanParameterIsNeverRequired(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"logs": {
			Args: chorefile.Args{{Name: "follow", Type: chorefile.TypeBool}},
			Cmds: cmds(`printf 'follow=[%s]' '{{.FOLLOW}}' > out.txt`),
		},
	})
	f.mustRun("logs", nil, nil)
	if got := f.read("out.txt"); got != "follow=[]" {
		t.Errorf("out.txt = %q, want an absent flag to read empty", got)
	}
}

// An int parameter rejects a value it cannot mean, at the point of binding,
// rather than letting a shell command fail later with something less obvious.
func TestIntParameterTypeIsChecked(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"logs": {
			Args: chorefile.Args{{Name: "lines", Type: chorefile.TypeInt}},
			Cmds: cmds(`printf ran > ran.txt`),
		},
	})
	err := f.mustFail("logs", []string{"abc"}, nil)
	mustContain(t, err.Error(), "must be a whole number", "error")

	// The same check must apply to a value supplied by name, which arrives by a
	// different route and once skipped it entirely.
	named := f.mustFail("logs", nil, map[string]string{"LINES": "abc"})
	mustContain(t, named.Error(), "must be a whole number", "error")
	if f.exists("ran.txt") {
		t.Error("the task ran with a non-numeric value for an int parameter")
	}
	f.mustRun("logs", []string{"50"}, nil)
}

// An explicitly empty default marks a parameter optional. Without this, there is
// no way to say "may be omitted, and empty is meaningful" — and it is why
// `args:` needs no required/optional marker: a default's presence is the marker.
func TestEmptyDefaultMakesAParameterOptional(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"list": {
			Args: chorefile.Args{{Name: "filter"}},
			Vars: map[string]chorefile.Var{"filter": {Value: ""}},
			Cmds: cmds(`printf 'filter=[%s]' '{{.FILTER}}' > out.txt`),
		},
		"needed": {
			Args: chorefile.Args{{Name: "config"}},
			Cmds: cmds(`printf ran > ran.txt`),
		},
	})

	f.mustRun("list", nil, nil)
	if got := f.read("out.txt"); got != "filter=[]" {
		t.Errorf("out.txt = %q, want an empty filter to be allowed", got)
	}

	// A parameter with no declared default is still required.
	err := f.mustFail("needed", nil, nil)
	mustContain(t, err.Error(), "needs argument(s) config", "error")
}

// The same parameter supplied positionally AND by name is a contradiction, so it
// is refused rather than resolved by precedence — silently preferring one is how
// a command acts on a config the caller did not mean.
func TestConflictingArgumentAndNamedValue(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"up": {Args: chorefile.Args{{Name: "config"}}, Cmds: cmds(`printf ran > ran.txt`)},
	})

	err := f.mustFail("up", []string{"mail4.test"}, map[string]string{"CONFIG": "restmail.test"})
	mustContain(t, err.Error(), "given twice", "error")
	if f.exists("ran.txt") {
		t.Error("the task ran despite contradictory arguments")
	}

	// The same value twice is not a contradiction.
	f2 := newFixture(t, nil, map[string]*chorefile.Task{
		"up": {Args: chorefile.Args{{Name: "config"}}, Cmds: cmds(`printf ran > ran.txt`)},
	})
	f2.mustRun("up", []string{"mail4.test"}, map[string]string{"CONFIG": "mail4.test"})
}

// A parameter's DEFAULT must reach the dotenv path, which is the thing usually
// keyed on it. Task vars as a whole are resolved after dotenv (they may read its
// values), so parameter defaults specifically are resolved earlier — otherwise
// `chore up` with no argument renders `config//config.env` and fails, while
// `chore up mail4.test` works, which is a baffling way to greet a new user.
func TestParameterDefaultReachesTheDotenvPath(t *testing.T) {
	f := newFixture(t, &chorefile.File{
		Dotenv: []string{"config/{{.CONFIG}}/config.env"},
	}, map[string]*chorefile.Task{
		"up": {
			Args: chorefile.Args{{Name: "config"}},
			Vars: map[string]chorefile.Var{"config": {Value: "alpha"}},
			Cmds: cmds(`printf '%s' "$STACK" > out.txt`),
		},
	})
	f.write("config/alpha/config.env", "STACK=alpha-stack\n")
	f.write("config/bravo/config.env", "STACK=bravo-stack\n")

	f.mustRun("up", nil, nil)
	if got := f.read("out.txt"); got != "alpha-stack" {
		t.Errorf("with no argument: STACK = %q, want the default config's value", got)
	}

	f.mustRun("up", []string{"bravo"}, nil)
	if got := f.read("out.txt"); got != "bravo-stack" {
		t.Errorf("with an argument: STACK = %q, want the named config's value", got)
	}
}

// An argument answers to the name as written AND its uppercase form. Taskfile
// convention is uppercase variables, so `args: [config]` used as {{.CONFIG}} —
// the form this program's own usage text shows — must work rather than silently
// interpolating nothing.
func TestArgsBindUnderTheNameDeclared(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"up": {
			Args: chorefile.Args{{Name: "config"}},
			Cmds: cmds(`printf 'lower=%s upper=%s' '{{.config}}' '{{.CONFIG}}' > out.txt`),
		},
	})
	f.mustRun("up", []string{"mail4.test"}, nil)

	if got, want := f.read("out.txt"), "lower=mail4.test upper=mail4.test"; got != want {
		t.Errorf("out.txt = %q, want %q (an argument binds under both cases)", got, want)
	}
}

// TestArgsTooManyIsAnError: a caller who passes more than the task accepts meant
// something the task will not do, so guessing is worse than refusing.
func TestArgsTooManyIsAnError(t *testing.T) {
	t.Run("task with parameters", func(t *testing.T) {
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"up": {Args: chorefile.Args{{Name: "config"}}, Cmds: cmds("printf ran > ran.txt")},
		})
		err := f.mustFail("up", []string{"a", "b"}, nil)
		mustContain(t, err.Error(), "up", "error")
		mustContain(t, err.Error(), "takes 1 argument(s) (config), got 2", "error")
		if f.exists("ran.txt") {
			t.Error("the task ran despite the binding error")
		}
	})

	t.Run("task with no parameters", func(t *testing.T) {
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"build": {Cmds: cmds("printf ran > ran.txt")},
		})
		err := f.mustFail("build", []string{"stray"}, nil)
		mustContain(t, err.Error(), "build", "error")
		mustContain(t, err.Error(), "takes no arguments, got 1 (stray)", "error")
	})
}

// TestArgsFewerThanParametersFallThrough: an unbound parameter is not an error,
// it simply is not in the argument layer, so the name resolves further down the
// scope. That is the whole mechanism behind an optional argument with a default.
func TestArgsFewerThanParametersFallThrough(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {
			Args: chorefile.Args{{Name: "ENV"}, {Name: "TAG"}},
			Vars: vars("TAG", "latest"),
			Cmds: cmds(`printf '%s@%s' '{{.ENV}}' '{{.TAG}}' > out.txt`),
		},
	})
	f.mustRun("deploy", []string{"staging"}, nil)

	if got, want := f.read("out.txt"), "staging@latest"; got != want {
		t.Errorf("out.txt = %q, want %q", got, want)
	}
}

// ---------- 2. variable precedence ----------

// TestVariablePrecedence walks the order SPEC "Fixed semantics" #2 fixes:
//
//	positional args → call vars → task vars → include vars → file vars → dotenv → process env
//
// One subtest per adjacent pair, so a failure names the exact boundary that
// moved instead of leaving a pile of variables to bisect. The include-vars layer
// has no subtest here: the loader merges an include's vars into the included
// file's own vars before the Runner ever sees them, so from run's point of view
// there is no separate layer to order.
func TestVariablePrecedence(t *testing.T) {
	const probe = `printf '%s' '{{.V}}' > out.txt`

	t.Run("a declared parameter given twice is refused, not ranked", func(t *testing.T) {
		// SPEC lists positional args above call vars, and that ordering still holds
		// for everything else. But for a DECLARED parameter the two forms are the
		// same knob, so supplying both is a contradiction from the command line
		// (`chore up mail4.test CONFIG=other`) and is rejected rather than silently
		// resolved — the caller named two configs and would get one.
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"probe": {Args: chorefile.Args{{Name: "V"}}, Cmds: cmds(probe)},
		})
		err := f.mustFail("probe", []string{"from-arg"}, map[string]string{"V": "from-call"})
		mustContain(t, err.Error(), "given twice", "error")
	})

	t.Run("call var beats task var", func(t *testing.T) {
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"probe": {Vars: vars("V", "from-task"), Cmds: cmds(probe)},
		})
		f.mustRun("probe", nil, map[string]string{"V": "from-call"})
		if got := f.read("out.txt"); got != "from-call" {
			t.Errorf("V = %q, want from-call", got)
		}
	})

	t.Run("task var beats file var", func(t *testing.T) {
		f := newFixture(t, &chorefile.File{Vars: vars("V", "from-file")}, map[string]*chorefile.Task{
			"probe": {Vars: vars("V", "from-task"), Cmds: cmds(probe)},
		})
		f.mustRun("probe", nil, nil)
		if got := f.read("out.txt"); got != "from-task" {
			t.Errorf("V = %q, want from-task", got)
		}
	})

	t.Run("file var beats dotenv", func(t *testing.T) {
		f := newFixture(t, &chorefile.File{
			Dotenv: []string{"app.env"},
			Vars:   vars("V", "from-file"),
		}, map[string]*chorefile.Task{
			"probe": {Cmds: cmds(probe)},
		})
		f.write("app.env", "V=from-dotenv\n")
		f.mustRun("probe", nil, nil)
		if got := f.read("out.txt"); got != "from-file" {
			t.Errorf("V = %q, want from-file", got)
		}
	})

	t.Run("dotenv beats process environment", func(t *testing.T) {
		// The process environment is the base layer, so a dotenv file is how a
		// config overrides whatever the developer happens to have exported.
		t.Setenv("V", "from-process-env")
		f := newFixture(t, &chorefile.File{Dotenv: []string{"app.env"}}, map[string]*chorefile.Task{
			"probe": {Cmds: cmds(probe)},
		})
		f.write("app.env", "V=from-dotenv\n")
		f.mustRun("probe", nil, nil)
		if got := f.read("out.txt"); got != "from-dotenv" {
			t.Errorf("V = %q, want from-dotenv", got)
		}
	})

	t.Run("process environment is visible when nothing overrides it", func(t *testing.T) {
		t.Setenv("TSK_TEST_ONLY_IN_ENV", "from-process-env")
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"probe": {Cmds: cmds(`printf '%s' '{{.TSK_TEST_ONLY_IN_ENV}}' > out.txt`)},
		})
		f.mustRun("probe", nil, nil)
		if got := f.read("out.txt"); got != "from-process-env" {
			t.Errorf("value = %q, want from-process-env", got)
		}
	})
}

// ---------- 3. dotenv is resolved after arguments ----------

// TestDotenvResolvesAfterArguments is the defect the whole program exists to
// fix. Task resolves `dotenv:` while parsing the Taskfile, before command-line
// variables exist, so `task up CONFIG=mail4.test` loaded the DEFAULT config's
// environment and then operated on the wrong stack without a word. Here two
// configs exist side by side and the invocation must select one, both through a
// positional argument and through the NAME=value form.
func TestDotenvResolvesAfterArguments(t *testing.T) {
	newStacks := func(t *testing.T) *fixture {
		t.Helper()
		f := newFixture(t, &chorefile.File{
			Dotenv: []string{"config/{{.CONFIG}}/config.env"},
		}, map[string]*chorefile.Task{
			// Both spellings are asserted: {{.STACK}} proves the value reached
			// the template scope, "$STACK" proves it reached the script's
			// environment, and a real Taskfile relies on both.
			"up": {
				Args: chorefile.Args{{Name: "CONFIG"}},
				Cmds: cmds(`printf '%s %s' '{{.STACK}}' "$STACK" > stack.txt`),
			},
		})
		f.write("config/alpha/config.env", "STACK=alpha\n")
		f.write("config/beta/config.env", "STACK=beta\n")
		return f
	}

	t.Run("positional argument selects the config", func(t *testing.T) {
		f := newStacks(t)
		f.mustRun("up", []string{"beta"}, nil)
		if got := f.read("stack.txt"); got != "beta beta" {
			t.Errorf("stack.txt = %q, want %q — the argument did not reach dotenv resolution", got, "beta beta")
		}
	})

	t.Run("the other config is equally reachable", func(t *testing.T) {
		// Asserting only one value would pass against an implementation that
		// hard-wired it; running both proves the argument is what chose.
		f := newStacks(t)
		f.mustRun("up", []string{"alpha"}, nil)
		if got := f.read("stack.txt"); got != "alpha alpha" {
			t.Errorf("stack.txt = %q, want %q", got, "alpha alpha")
		}
	})

	t.Run("NAME=value call var selects the config", func(t *testing.T) {
		// `chore up CONFIG=beta` — the exact invocation Task accepted and got wrong.
		f := newStacks(t)
		f.mustRun("up", nil, map[string]string{"CONFIG": "beta"})
		if got := f.read("stack.txt"); got != "beta beta" {
			t.Errorf("stack.txt = %q, want %q", got, "beta beta")
		}
	})

	t.Run("a config that does not exist fails instead of defaulting", func(t *testing.T) {
		// The failure mode being prevented: no environment at all, every value
		// silently empty, commands matching nothing.
		f := newStacks(t)
		err := f.mustFail("up", []string{"nope"}, nil)
		mustContain(t, err.Error(), "no environment loaded", "error")
		mustContain(t, err.Error(), filepath.Join("config", "nope", "config.env"), "error")
		if f.exists("stack.txt") {
			t.Error("the task ran with no environment loaded")
		}
	})
}

// ---------- 4. deps ----------

// TestDepsRunConcurrently: sequential execution of two 300ms sleeps could not
// finish in under 600ms, so the ceiling separates the two implementations while
// still leaving room for process startup on a loaded machine.
func TestDepsRunConcurrently(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"top":    {Deps: depsOn("slow-a", "slow-b")},
		"slow-a": {Cmds: cmds("sleep 0.3; printf a > a.txt")},
		"slow-b": {Cmds: cmds("sleep 0.3; printf b > b.txt")},
	})

	start := time.Now()
	f.mustRun("top", nil, nil)
	elapsed := time.Since(start)

	if !f.exists("a.txt") || !f.exists("b.txt") {
		t.Fatalf("both deps must run: a.txt=%v b.txt=%v", f.exists("a.txt"), f.exists("b.txt"))
	}
	if elapsed >= 550*time.Millisecond {
		t.Errorf("deps took %v; two 300ms sleeps in parallel should finish well under the 600ms they take in sequence", elapsed)
	}
}

// TestFailingDepCancelsTheOthers: the first error cancels the group, so a long
// dep is killed rather than left to finish work whose result is already void.
func TestFailingDepCancelsTheOthers(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"top":   {Deps: depsOn("fails", "slow"), Cmds: cmds("printf top > top.txt")},
		"fails": {Cmds: cmds("exit 1")},
		"slow":  {Cmds: cmds("sleep 0.5; printf slow > slow.txt")},
	})

	err := f.mustFail("top", nil, nil)
	mustContain(t, err.Error(), "fails", "error")

	if f.exists("slow.txt") {
		t.Error("the surviving dep completed; a failing dep must cancel the rest")
	}
	if f.exists("top.txt") {
		t.Error("the task body ran despite a failing dep")
	}
}

// ---------- 5. run: once ----------

// TestRunOnceExecutesOnce covers SPEC "Fixed semantics" #6. The sleep is load
// bearing: deps run concurrently, so the interesting window is between the first
// caller deciding the task has not run and the moment that decision is recorded.
// A dedup that only records after execution lets a second caller start a
// duplicate inside that window — which is precisely what `run: once` exists to
// prevent for a task that creates a network or a volume.
func TestRunOnceExecutesOnce(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"top":   {Deps: depsOn("a", "b")},
		"a":     {Deps: depsOn("setup"), Cmds: cmds("printf a > a.txt")},
		"b":     {Deps: depsOn("setup"), Cmds: cmds("printf b > b.txt")},
		"setup": {Run: "once", Cmds: cmds("sleep 0.2; printf x >> count.txt")},
	})

	f.mustRun("top", nil, nil)

	if got := f.read("count.txt"); got != "x" {
		t.Errorf("setup ran %d times (%q), want exactly once", len(got), got)
	}
}

// TestRunOnceKeyIncludesVariables: the dedup key is the task name plus its
// rendered variables, so `once` means "once per distinct configuration", not
// "once per name". Two deps that ask for different stacks must both happen.
func TestRunOnceKeyIncludesVariables(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"top": {Deps: []chorefile.Dep{
			{Task: "setup", Vars: vars("N", "1")},
			{Task: "setup", Vars: vars("N", "2")},
		}},
		"setup": {Run: "once", Cmds: cmds(`printf '%s' '{{.N}}' >> count.txt`)},
	})

	f.mustRun("top", nil, nil)

	got := f.read("count.txt")
	if len(got) != 2 || !strings.Contains(got, "1") || !strings.Contains(got, "2") {
		t.Errorf("count.txt = %q, want both 1 and 2 — different vars are different work", got)
	}
}

// ---------- 6. defer ----------

// TestDeferRunsLastAndInReverse: reverse order is what makes deferred steps
// compose. Each step tears down what the step before it built, so unwinding has
// to happen in the opposite order to setting up.
func TestDeferRunsLastAndInReverse(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {Cmds: []chorefile.Cmd{
			{Cmd: "printf 1 >> order.txt"},
			{Cmd: "printf a >> order.txt", Defer: true},
			{Cmd: "printf b >> order.txt", Defer: true},
			{Cmd: "printf 2 >> order.txt"},
		}},
	})

	f.mustRun("deploy", nil, nil)

	if got, want := f.read("order.txt"), "12ba"; got != want {
		t.Errorf("order.txt = %q, want %q (normal steps in order, deferred steps last and reversed)", got, want)
	}
}

// TestDeferRunsAfterFailure is the reason `defer` is worth having at all: a task
// that brings a topology up can promise it comes back down even when the middle
// of the task explodes. The steps after the failure must still be skipped.
func TestDeferRunsAfterFailure(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {Cmds: []chorefile.Cmd{
			{Cmd: "printf up >> order.txt"},
			{Cmd: "printf down >> order.txt", Defer: true},
			{Cmd: "exit 42"},
			{Cmd: "printf unreachable >> order.txt"},
		}},
	})

	err := f.mustFail("deploy", nil, nil)

	if got, want := f.read("order.txt"), "updown"; got != want {
		t.Errorf("order.txt = %q, want %q", got, want)
	}
	if code := ExitCode(err); code != 42 {
		t.Errorf("ExitCode = %d, want 42", code)
	}
}

// TestDeferredFailureDoesNotMaskTheRealOne: if teardown also fails, the reported
// error must stay the one that explains why the task failed. Reporting the
// teardown error instead would send a reader to investigate the wrong thing.
func TestDeferredFailureDoesNotMaskTheRealOne(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {Cmds: []chorefile.Cmd{
			{Cmd: "exit 7", Defer: true},
			{Cmd: "exit 42"},
		}},
	})

	err := f.mustFail("deploy", nil, nil)

	if code := ExitCode(err); code != 42 {
		t.Errorf("ExitCode = %d, want 42 — the original failure, not the deferred one", code)
	}
	// The teardown failure is still worth knowing about, so it is reported.
	mustContain(t, f.err.String(), "deferred step failed", "stderr")
}

// TestDeferredFailureAloneFailsTheTask: when nothing else went wrong, a failed
// teardown is the failure, and swallowing it would leave the topology half up
// while the run reported success.
func TestDeferredFailureAloneFailsTheTask(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {Cmds: []chorefile.Cmd{
			{Cmd: "printf ok > ok.txt"},
			{Cmd: "exit 7", Defer: true},
		}},
	})

	err := f.mustFail("deploy", nil, nil)

	if code := ExitCode(err); code != 7 {
		t.Errorf("ExitCode = %d, want 7", code)
	}
	if !f.exists("ok.txt") {
		t.Error("the normal step should still have run")
	}
}

// TestDeferDeclaredAfterTheFailureIsNotRegistered pins the registration rule,
// which is Go's and Task's: a deferred step is armed when execution reaches it,
// not when the task is parsed. A failure stops the walk through `cmds:`, so a
// `defer:` written below the command that failed never runs.
//
// This is the one thing to know when writing teardown: put the `defer:`
// immediately AFTER the step that creates the thing, never at the end of the
// list, or the case it exists for is exactly the case it misses.
func TestDeferDeclaredAfterTheFailureIsNotRegistered(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {Cmds: []chorefile.Cmd{
			{Cmd: "exit 1"},
			{Cmd: "printf late >> order.txt", Defer: true},
		}},
	})

	f.mustFail("deploy", nil, nil)

	if f.exists("order.txt") {
		t.Error("a defer declared below the failing command ran; registration is supposed to be positional")
	}
}

// ---------- 7. ignore_error ----------

func TestIgnoreError(t *testing.T) {
	t.Run("per command", func(t *testing.T) {
		// The exemption is scoped to the one step that declares it: the step
		// after it still runs, and a later real failure is still a failure.
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"check": {Cmds: []chorefile.Cmd{
				{Cmd: "exit 3", IgnoreError: true},
				{Cmd: "printf after > after.txt"},
			}},
		})
		f.mustRun("check", nil, nil)

		if got := f.read("after.txt"); got != "after" {
			t.Errorf("after.txt = %q, want %q", got, "after")
		}
		mustContain(t, f.err.String(), "failed, ignored", "stderr")
	})

	t.Run("per command does not cover the other steps", func(t *testing.T) {
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"check": {Cmds: []chorefile.Cmd{
				{Cmd: "exit 3", IgnoreError: true},
				{Cmd: "exit 9"},
			}},
		})
		err := f.mustFail("check", nil, nil)
		if code := ExitCode(err); code != 9 {
			t.Errorf("ExitCode = %d, want 9", code)
		}
	})

	t.Run("per task", func(t *testing.T) {
		// A task-level exemption covers every step, so the task runs to the end
		// and reports success — the shape of a best-effort cleanup task.
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"clean": {IgnoreError: true, Cmds: cmds("exit 1", "printf second > second.txt", "exit 2")},
		})
		f.mustRun("clean", nil, nil)

		if !f.exists("second.txt") {
			t.Error("execution stopped at the first failure despite ignore_error on the task")
		}
	})
}

// ---------- 8. requires ----------

// TestRequiresFailsBeforeAnythingRuns: `requires:` is a precondition, so it has
// to be checked before deps are scheduled. Checking it later would mean the
// missing variable is discovered after half the work has already happened.
func TestRequiresFailsBeforeAnythingRuns(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {
			Requires: []string{"TSK_TEST_TARGET"},
			Deps:     depsOn("side-effect"),
			Cmds:     cmds("printf body > body.txt"),
		},
		"side-effect": {Cmds: cmds("printf dep > dep.txt")},
	})

	err := f.mustFail("deploy", nil, nil)

	mustContain(t, err.Error(), "deploy", "error")
	mustContain(t, err.Error(), "required variable(s) not set: TSK_TEST_TARGET", "error")
	if f.exists("dep.txt") {
		t.Error("a dependency ran before the requires check")
	}
	if f.exists("body.txt") {
		t.Error("the task body ran despite an unmet requirement")
	}
}

// TestRequiresIsSatisfiedByAnArgument proves the check reads the assembled
// scope, not just the task's own vars — otherwise `requires` and `args` could
// not be used together, which is the obvious way to write a mandatory parameter.
func TestRequiresIsSatisfiedByAnArgument(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {
			Args:     chorefile.Args{{Name: "TARGET"}},
			Requires: []string{"TARGET"},
			Cmds:     cmds(`printf '%s' '{{.TARGET}}' > out.txt`),
		},
	})
	f.mustRun("deploy", []string{"prod"}, nil)

	if got := f.read("out.txt"); got != "prod" {
		t.Errorf("out.txt = %q, want prod", got)
	}
}

// ---------- 9. exit codes ----------

// TestExitCodePropagates covers SPEC "Fixed semantics" #7: chore exits with the
// code its command did, so a caller's `if chore x; then` and its retry logic keep
// working.
func TestExitCodePropagates(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"boom": {Cmds: cmds("exit 42")},
	})
	err := f.mustFail("boom", nil, nil)

	if code := ExitCode(err); code != 42 {
		t.Errorf("ExitCode = %d, want 42", code)
	}
	if ExitCode(nil) != 0 {
		t.Error("ExitCode(nil) must be 0")
	}
	// Anything that is not a script failure is still a failure, just without a
	// code of its own.
	if got := ExitCode(errors.New("could not start")); got != 1 {
		t.Errorf("ExitCode(non-exit error) = %d, want 1", got)
	}
}

// TestExitCodeSurvivesNesting: the code has to travel back through a `- task:`
// step, otherwise nesting a task would quietly flatten every failure to 1.
func TestExitCodeSurvivesNesting(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"outer": {Cmds: []chorefile.Cmd{{Task: "inner"}}},
		"inner": {Cmds: cmds("exit 17")},
	})
	err := f.mustFail("outer", nil, nil)

	if code := ExitCode(err); code != 17 {
		t.Errorf("ExitCode = %d, want 17", code)
	}
}

// ---------- 10. dry run ----------

// TestDryRunPrintsWithoutExecuting: --dry is only useful if it is trustworthy,
// so the assertion that matters is the marker file that must NOT exist.
func TestDryRunPrintsWithoutExecuting(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"up":   {Deps: depsOn("prep"), Cmds: cmds(`printf '{{.WHAT}}' > marker.txt`), Vars: vars("WHAT", "danger")},
		"prep": {Cmds: cmds("printf prep > prep.txt")},
	})
	f.r.DryRun = true

	f.mustRun("up", nil, nil)

	// The printed command is the rendered one: a dry run that showed the
	// unrendered template would hide the variable resolution being checked.
	mustContain(t, f.out.String(), "printf 'danger' > marker.txt", "stdout")
	mustContain(t, f.out.String(), "printf prep > prep.txt", "stdout")
	if f.exists("marker.txt") || f.exists("prep.txt") {
		t.Error("a dry run executed something")
	}
}

// ---------- 11. unknown task ----------

func TestUnknownTask(t *testing.T) {
	newProject := func(t *testing.T) *fixture {
		t.Helper()
		return newFixture(t, nil, map[string]*chorefile.Task{
			"build":     {Cmds: cmds("true")},
			"test:unit": {Cmds: cmds("true")},
			"test:e2e":  {Cmds: cmds("true")},
		})
	}

	t.Run("names the task", func(t *testing.T) {
		f := newProject(t)
		err := f.mustFail("nonexistent", nil, nil)
		mustContain(t, err.Error(), `no task "nonexistent"`, "error")
	})

	t.Run("suggests near matches", func(t *testing.T) {
		// A namespaced project has 150+ tasks, and typing the namespace without
		// the task is the commonest way to miss.
		f := newProject(t)
		err := f.mustFail("test", nil, nil)
		mustContain(t, err.Error(), "did you mean", "error")
		mustContain(t, err.Error(), "test:e2e", "error")
		mustContain(t, err.Error(), "test:unit", "error")
	})

	t.Run("stays quiet when nothing is close", func(t *testing.T) {
		f := newProject(t)
		err := f.mustFail("zzz", nil, nil)
		if strings.Contains(err.Error(), "did you mean") {
			t.Errorf("unrelated suggestions offered: %v", err)
		}
	})
}

// ---------- 12. dir ----------

func TestTaskDir(t *testing.T) {
	t.Run("relative to the taskfile", func(t *testing.T) {
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"in-sub": {Dir: "sub", Cmds: cmds("printf here > here.txt")},
		})
		f.mkdir("sub")
		f.mustRun("in-sub", nil, nil)

		if !f.exists("sub/here.txt") {
			t.Error("the task did not run in sub/")
		}
		if f.exists("here.txt") {
			t.Error("the task ran in the taskfile's directory, ignoring dir:")
		}
	})

	t.Run("rendered from a variable", func(t *testing.T) {
		// dir: is templated, which is how one task serves several instances.
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"in-sub": {
				Dir:  "stacks/{{.NAME}}",
				Args: chorefile.Args{{Name: "NAME"}},
				Cmds: cmds("printf here > here.txt"),
			},
		})
		f.mkdir("stacks/beta")
		f.mustRun("in-sub", []string{"beta"}, nil)

		if !f.exists("stacks/beta/here.txt") {
			t.Error("the task did not run in stacks/beta/")
		}
	})

	t.Run("absolute", func(t *testing.T) {
		f := newFixture(t, nil, map[string]*chorefile.Task{
			"elsewhere": {Cmds: cmds("printf here > here.txt")},
		})
		outside := t.TempDir()
		f.r.Project.Tasks["elsewhere"].Dir = outside

		f.mustRun("elsewhere", nil, nil)

		if _, err := os.Stat(filepath.Join(outside, "here.txt")); err != nil {
			t.Errorf("an absolute dir: was not used: %v", err)
		}
	})
}

// TestEnv covers `env:`, which was accepted by the schema, listed as supported in
// the README and SPEC, and never reached the shell at all — so rest-mail's
// `docker build … >$OUTPUT 2>&1`, whose OUTPUT is a file-level `env:` with `sh:`,
// died with "ambiguous redirect" on every task that builds an image.
func TestEnv(t *testing.T) {
	// Both spellings, both levels, and the value has to arrive as a shell variable
	// AND as a template variable: this project uses `>$OUTPUT` in commands and
	// `{{.OUTPUT}}` nowhere, but go-task exposes env: to templates and a file
	// written for one runner is read by the other.
	t.Run("file-level, literal and sh:", func(t *testing.T) {
		f := newFixture(t, &chorefile.File{Env: map[string]chorefile.Var{
			"PLAIN": {Value: "literal"},
			"VIASH": {Sh: "echo computed"},
		}}, map[string]*chorefile.Task{
			"show": {Cmds: []chorefile.Cmd{{Cmd: `echo "shell=[$PLAIN/$VIASH] tmpl=[{{.PLAIN}}/{{.VIASH}}]" > out.txt`}}},
		})
		f.mustRun("show", nil, nil)
		if got := f.read("out.txt"); got != "shell=[literal/computed] tmpl=[literal/computed]\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("task-level overrides file-level", func(t *testing.T) {
		f := newFixture(t, &chorefile.File{Env: map[string]chorefile.Var{"WHO": {Value: "file"}}},
			map[string]*chorefile.Task{
				"show": {
					Env:  map[string]chorefile.Var{"WHO": {Value: "task"}},
					Cmds: []chorefile.Cmd{{Cmd: `echo "$WHO" > out.txt`}},
				},
			})
		f.mustRun("show", nil, nil)
		if got := f.read("out.txt"); got != "task\n" {
			t.Errorf("task env did not override file env: %q", got)
		}
	})

	// An empty redirect target is exactly the failure this fixes: bash reports
	// "ambiguous redirect" and the task fails, which is how the bug surfaced.
	t.Run("usable as a redirect target", func(t *testing.T) {
		f := newFixture(t, &chorefile.File{Env: map[string]chorefile.Var{
			"OUTPUT": {Sh: `[ -n "$VERBOSE" ] && echo /dev/stdout || echo /dev/null`},
		}}, map[string]*chorefile.Task{
			"build": {Cmds: []chorefile.Cmd{
				{Cmd: `echo noise >$OUTPUT 2>&1`},
				{Cmd: `echo ran > out.txt`},
			}},
		})
		f.mustRun("build", nil, nil)
		if got := f.read("out.txt"); got != "ran\n" {
			t.Errorf("task did not complete: %q", got)
		}
	})

	// A caller has to be able to win: the whole point of OUTPUT is flipping it
	// per-invocation, and `chore up VERBOSE=1` must reach the sh: that reads it.
	t.Run("a call var beats file env", func(t *testing.T) {
		f := newFixture(t, &chorefile.File{Env: map[string]chorefile.Var{"WHO": {Value: "file"}}},
			map[string]*chorefile.Task{
				"show": {Cmds: []chorefile.Cmd{{Cmd: `echo "$WHO" > out.txt`}}},
			})
		f.mustRun("show", nil, map[string]string{"WHO": "caller"})
		if got := f.read("out.txt"); got != "caller\n" {
			t.Errorf("caller did not override file env: %q", got)
		}
	})

	// env: is read by the sh: of another env entry, which is what a toggle looks
	// like: VERBOSE from the environment decides what OUTPUT becomes.
	t.Run("sh: sees the process environment", func(t *testing.T) {
		t.Setenv("VERBOSE", "1")
		f := newFixture(t, &chorefile.File{Env: map[string]chorefile.Var{
			"OUTPUT": {Sh: `[ -n "$VERBOSE" ] && echo verbose || echo quiet`},
		}}, map[string]*chorefile.Task{
			"show": {Cmds: []chorefile.Cmd{{Cmd: `echo "$OUTPUT" > out.txt`}}},
		})
		f.mustRun("show", nil, nil)
		if got := f.read("out.txt"); got != "verbose\n" {
			t.Errorf("sh: did not see VERBOSE: %q", got)
		}
	})
}

// TestIncludedTaskWorkingDirectory pins where an included task RUNS.
//
// go-task keeps the working directory at the ROOT taskfile's directory for a
// plain include, and only moves it when the include declares `dir:`. That is
// exactly why it also offers {{.TASKFILE_DIR}} — you ask for the file's own
// directory when you want it. chore ran every included task in the file's own
// directory instead, so in rest-mail `-v $(pwd):/app` bind-mounted tasks/ as the
// application, and the api container restarted forever on
// "open .air.toml: no such file or directory". `docker build … -f Dockerfile .`
// in the same file was pointed at the wrong context for the same reason.
func TestIncludedTaskWorkingDirectory(t *testing.T) {
	// project builds what the loader would produce for
	// `includes: {sub: {taskfile: ./nested/sub.yml[, dir: …]}}`. includeDir is the
	// include's `dir:`; empty means the plain case.
	project := func(t *testing.T, includeDir string) (*fixture, string) {
		t.Helper()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
			t.Fatal(err)
		}
		root := &chorefile.File{Path: filepath.Join(dir, "Taskfile.yml"), Dir: dir}
		sub := &chorefile.File{Path: filepath.Join(dir, "nested", "sub.yml")}
		// The loader sets Dir to the include's `dir:` when it has one, and to the
		// file's own directory otherwise.
		sub.Dir = filepath.Dir(sub.Path)
		if includeDir != "" {
			sub.Dir = filepath.Join(dir, includeDir)
			sub.WorkDir = sub.Dir
		}
		task := &chorefile.Task{Name: "sub:where", File: sub,
			Cmds: []chorefile.Cmd{{Cmd: `pwd > "$MARKER"`}}}
		sub.Tasks = map[string]*chorefile.Task{"sub:where": task}
		tasks := map[string]*chorefile.Task{"sub:where": task}
		out, errOut := &syncBuf{}, &syncBuf{}
		p := &chorefile.Project{Root: root, Tasks: tasks, RootDir: dir}
		f := &fixture{t: t, dir: dir, file: sub, r: New(p, out, errOut), out: out, err: errOut}
		return f, dir
	}

	// An absolute marker path, because the point of the test is that the task's own
	// working directory is not where we think it is.
	t.Run("plain include runs in the root directory", func(t *testing.T) {
		f, dir := project(t, "")
		marker := filepath.Join(dir, "where.txt")
		t.Setenv("MARKER", marker)
		f.mustRun("sub:where", nil, nil)
		if got, want := ranIn(t, marker), resolve(t, dir); got != want {
			t.Errorf("ran in %q, want the root %q", got, want)
		}
	})

	t.Run("an include's dir: is honoured", func(t *testing.T) {
		f, dir := project(t, "nested")
		marker := filepath.Join(dir, "where.txt")
		t.Setenv("MARKER", marker)
		f.mustRun("sub:where", nil, nil)
		if got, want := ranIn(t, marker), resolve(t, filepath.Join(dir, "nested")); got != want {
			t.Errorf("ran in %q, want the include's dir %q", got, want)
		}
	})
}

// ranIn reads a marker holding a `pwd`, resolved through symlinks.
func ranIn(t *testing.T, marker string) string {
	t.Helper()
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	return resolve(t, strings.TrimSpace(string(b)))
}

// resolve follows symlinks before comparing paths. On macOS t.TempDir() hands back
// /var/folders/… while `pwd` in the shell reports the physical
// /private/var/folders/… — the same directory, and comparing the strings fails.
func resolve(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTaskReferenceIsRelativeToItsFile pins how a name written INSIDE a taskfile
// resolves. Every case here was checked against go-task first.
//
// `- task: deps` in tasks/webmail.yml means webmail's own deps. chore resolved
// these globally, so `chore instance:up` stopped with `no task "deps"` after
// bringing up nine containers, while go-task ran the same file.
func TestTaskReferenceIsRelativeToItsFile(t *testing.T) {
	// build makes a project with a root file and one included file under the
	// namespace "sub", both able to hold tasks, plus a marker each task writes.
	build := func(t *testing.T, rootTasks, subTasks map[string]string, caller string, ref string) (*fixture, error) {
		t.Helper()
		dir := t.TempDir()
		root := &chorefile.File{Path: filepath.Join(dir, "Taskfile.yml"), Dir: dir}
		sub := &chorefile.File{Path: filepath.Join(dir, "sub.yml"), Dir: dir, Namespace: "sub"}
		tasks := map[string]*chorefile.Task{}
		add := func(f *chorefile.File, name, marker string) {
			full := name
			if f.Namespace != "" {
				full = f.Namespace + ":" + name
			}
			tasks[full] = &chorefile.Task{Name: full, File: f,
				Cmds: []chorefile.Cmd{{Cmd: "echo " + marker + " > out.txt"}}}
		}
		for name, marker := range rootTasks {
			add(root, name, marker)
		}
		for name, marker := range subTasks {
			add(sub, name, marker)
		}
		// The caller lives in the included file and references `ref`.
		tasks[caller] = &chorefile.Task{Name: caller, File: sub,
			Cmds: []chorefile.Cmd{{Task: ref}}}
		out, errOut := &syncBuf{}, &syncBuf{}
		p := &chorefile.Project{Root: root, Tasks: tasks, RootDir: dir}
		f := &fixture{t: t, dir: dir, file: sub, r: New(p, out, errOut), out: out, err: errOut}
		return f, f.run(caller, nil, nil)
	}

	t.Run("a sibling wins over a root task of the same name", func(t *testing.T) {
		f, err := build(t,
			map[string]string{"deps": "ROOT"},
			map[string]string{"deps": "SIBLING"},
			"sub:caller", "deps")
		if err != nil {
			t.Fatalf("run: %v\nstderr:\n%s", err, f.err)
		}
		if got := strings.TrimSpace(f.read("out.txt")); got != "SIBLING" {
			t.Errorf("ran the %s task; a reference is relative to its own file", got)
		}
	})

	t.Run("a reference keeps a colon it already has", func(t *testing.T) {
		// tasks/monitoring.yml defines a task named "prometheus:up" and refers to it
		// as `- task: prometheus:up`, which is monitoring:prometheus:up.
		f, err := build(t, nil,
			map[string]string{"prometheus:up": "NESTED"},
			"sub:up", "prometheus:up")
		if err != nil {
			t.Fatalf("run: %v\nstderr:\n%s", err, f.err)
		}
		if got := strings.TrimSpace(f.read("out.txt")); got != "NESTED" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a leading colon escapes to the root", func(t *testing.T) {
		f, err := build(t,
			map[string]string{"build": "ROOT"},
			map[string]string{"build": "SIBLING"},
			"sub:caller", ":build")
		if err != nil {
			t.Fatalf("run: %v\nstderr:\n%s", err, f.err)
		}
		if got := strings.TrimSpace(f.read("out.txt")); got != "ROOT" {
			t.Errorf("`:build` ran %q; a leading colon means the root namespace", got)
		}
	})

	// No falling back to the root when the sibling is missing: go-task reports
	// `Task "sub:rootonly" does not exist`, and quietly running a different task
	// than go-task would is worse than an error.
	t.Run("a missing sibling is an error, named as written", func(t *testing.T) {
		_, err := build(t,
			map[string]string{"rootonly": "ROOT"},
			nil,
			"sub:caller", "rootonly")
		if err == nil {
			t.Fatal("referencing a root task by bare name from an include should fail")
		}
		if !strings.Contains(err.Error(), "sub:rootonly") {
			t.Errorf("error %q does not name the task it looked for (sub:rootonly)", err)
		}
	})

	t.Run("deps resolve the same way", func(t *testing.T) {
		dir := t.TempDir()
		root := &chorefile.File{Path: filepath.Join(dir, "Taskfile.yml"), Dir: dir}
		sub := &chorefile.File{Path: filepath.Join(dir, "sub.yml"), Dir: dir, Namespace: "sub"}
		tasks := map[string]*chorefile.Task{
			"helper": {Name: "helper", File: root, Cmds: []chorefile.Cmd{{Cmd: "echo ROOT > out.txt"}}},
			"sub:helper": {Name: "sub:helper", File: sub,
				Cmds: []chorefile.Cmd{{Cmd: "echo SIBLING > out.txt"}}},
			"sub:caller": {Name: "sub:caller", File: sub, Deps: chorefile.Deps{{Task: "helper"}}},
		}
		out, errOut := &syncBuf{}, &syncBuf{}
		p := &chorefile.Project{Root: root, Tasks: tasks, RootDir: dir}
		f := &fixture{t: t, dir: dir, file: sub, r: New(p, out, errOut), out: out, err: errOut}
		f.mustRun("sub:caller", nil, nil)
		if got := strings.TrimSpace(f.read("out.txt")); got != "SIBLING" {
			t.Errorf("a dep resolved to %q, not the sibling", got)
		}
	})
}

// TestTaskDotenvOverride: a task that drives ANOTHER project must be able to
// decline this one's environment. rest-mail's root file requires a config's
// config.env — correct for every task that operates on a config, wrong for
// `instance:up --type reference`, whose whole job is to hand off to a peer
// repository that owns its own configs. Without this, that task fails with
// "no environment loaded" before it can delegate anything.
func TestTaskDotenvOverride(t *testing.T) {
	t.Run("dotenv: [] declines the file's", func(t *testing.T) {
		f := newFixture(t, &chorefile.File{Dotenv: []string{"missing.env"}},
			map[string]*chorefile.Task{
				"delegate": {Dotenv: []string{}, Cmds: []chorefile.Cmd{{Cmd: "echo ran > out.txt"}}},
			})
		f.mustRun("delegate", nil, nil)
		if got := f.read("out.txt"); got != "ran\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a task's own dotenv replaces the file's", func(t *testing.T) {
		f := newFixture(t, &chorefile.File{Dotenv: []string{"missing.env"}},
			map[string]*chorefile.Task{
				"own": {Dotenv: []string{"mine.env"}, Cmds: []chorefile.Cmd{{Cmd: `echo "$WHO" > out.txt`}}},
			})
		f.write("mine.env", "WHO=mine\n")
		f.mustRun("own", nil, nil)
		if got := f.read("out.txt"); got != "mine\n" {
			t.Errorf("got %q, want the task's own dotenv", got)
		}
	})

	// Not declaring one still inherits, which is what nearly every task wants —
	// and a genuinely missing file is still an error, not a silent default.
	t.Run("not declaring one inherits, and a miss still fails", func(t *testing.T) {
		f := newFixture(t, &chorefile.File{Dotenv: []string{"missing.env"}},
			map[string]*chorefile.Task{
				"inherits": {Cmds: []chorefile.Cmd{{Cmd: "echo ran > out.txt"}}},
			})
		if err := f.run("inherits", nil, nil); err == nil {
			t.Fatal("a missing inherited dotenv should fail")
		}
	})
}

// TestIncludeScoping covers what an included file may see: the mapping is
// rendered where it was WRITTEN, `inherit:` opts into the parent's variables, and
// without it nothing bleeds through.
func TestIncludeScoping(t *testing.T) {
	// build wires what the loader produces for an include of kid.yml, with or
	// without `inherit:`. The name HORSE is deliberately silly: nothing in chore
	// knows any particular variable name, and a test using WORKSPACE would hide a
	// special case if one ever crept in.
	build := func(t *testing.T, inherit bool) *fixture {
		t.Helper()
		dir := t.TempDir()
		root := &chorefile.File{
			Path: filepath.Join(dir, "chores.yml"), Dir: dir,
			Vars: map[string]chorefile.Var{
				"HORSE":  {Value: ".workspace"},
				"GLOBAL": {Value: "global-config"},
				"OWN":    {Value: "parent-own"},
			},
		}
		kid := &chorefile.File{
			Path: filepath.Join(dir, "kid.yml"), Dir: dir, Namespace: "kid",
			Parent: root, Inherit: inherit,
			Vars:        map[string]chorefile.Var{"OWN": {Value: "kid-own"}},
			IncludeVars: map[string]chorefile.Var{"MAPPED": {Value: "via-{{.HORSE}}"}},
		}
		task := &chorefile.Task{Name: "kid:show", File: kid, Cmds: []chorefile.Cmd{
			{Cmd: `echo "[{{.HORSE}}][{{.GLOBAL}}][{{.OWN}}][{{.MAPPED}}]" > out.txt`},
		}}
		kid.Tasks = map[string]*chorefile.Task{"kid:show": task}
		out, errOut := &syncBuf{}, &syncBuf{}
		p := &chorefile.Project{Root: root, Tasks: kid.Tasks, RootDir: dir}
		return &fixture{t: t, dir: dir, file: kid, r: New(p, out, errOut), out: out, err: errOut}
	}

	t.Run("default sees only what was mapped", func(t *testing.T) {
		f := build(t, false)
		f.mustRun("kid:show", nil, nil)
		// The mapping resolved against the PARENT's HORSE even though the child
		// cannot see HORSE itself — that is the distinction being tested.
		if got, want := strings.TrimSpace(f.read("out.txt")), "[][][kid-own][via-.workspace]"; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("inherit brings the parent's variables, below the file's own", func(t *testing.T) {
		f := build(t, true)
		f.mustRun("kid:show", nil, nil)
		if got, want := strings.TrimSpace(f.read("out.txt")), "[.workspace][global-config][kid-own][via-.workspace]"; got != want {
			t.Errorf("got %s, want %s — OWN must stay the child's", got, want)
		}
	})
}

// TestRootDotenvReachesIncludes pins the behaviour a change of mine broke.
//
// Scoping dotenv per file reads better than it works: rest-mail's root maps
// `POSTGRES_IP: '{{.MAIL3_POSTGRES_IP}}'` into an include, and that name comes from
// the ROOT's config.env — so a child's mapping depends on the parent's dotenv.
// Loading only the task's own file left the IP empty and `docker run --ip ""`
// failed with exit 125, taking the stack down.
//
// A task that must not require the root's environment says so with `dotenv: []`
// (see TestTaskDotenvOverride), which is how a hand-off to a peer project works.
func TestRootDotenvReachesIncludes(t *testing.T) {
	dir := t.TempDir()
	root := &chorefile.File{
		Path: filepath.Join(dir, "chores.yml"), Dir: dir,
		Dotenv: []string{"root.env"},
	}
	kid := &chorefile.File{
		Path: filepath.Join(dir, "kid.yml"), Dir: dir, Namespace: "kid", Parent: root,
		IncludeVars: map[string]chorefile.Var{"MAPPED": {Value: "{{.FROM_ROOT_ENV}}"}},
	}
	kid.Tasks = map[string]*chorefile.Task{
		"kid:go": {Name: "kid:go", File: kid,
			Cmds: []chorefile.Cmd{{Cmd: `echo "[{{.MAPPED}}]" > out.txt`}}},
	}
	out, errOut := &syncBuf{}, &syncBuf{}
	f := &fixture{t: t, dir: dir, file: kid,
		r:   New(&chorefile.Project{Root: root, Tasks: kid.Tasks, RootDir: dir}, out, errOut),
		out: out, err: errOut}
	f.write("root.env", "FROM_ROOT_ENV=10.99.0.43\n")

	f.mustRun("kid:go", nil, nil)
	if got := strings.TrimSpace(f.read("out.txt")); got != "[10.99.0.43]" {
		t.Errorf("got %s — a mapping must see the root's dotenv", got)
	}
}

// TestCancelKillsWhatTheScriptStarted is the regression test for the defect that
// made Ctrl-C leave processes behind: a task's script runs in its OWN process
// group, so the terminal's SIGINT — which reaches only the FOREGROUND process
// group — never touched it. chore died from the default action and the Cancel
// hook that kills the group never fired, because nothing cancelled the context.
// `chore app:run` exited while `flutter run` carried on holding the terminal.
//
// Note what this does and does not prove. Cancelling has ALWAYS killed the group
// — that half was built and correct. What was missing was anything to cancel the
// context, which lives in internal/cli and is tested there. This test pins the
// mechanism the fix depends on, so a later change to Setpgid or the Cancel hook
// cannot quietly take the teeth out of the signal handler.
func TestCancelKillsWhatTheScriptStarted(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		// The `&` and `wait` put the sleeper in a child of the shell, so a kill
		// aimed only at the shell would leave it running — which is the bug.
		"serve": {Cmds: []chorefile.Cmd{
			{Cmd: "(sleep 3; printf survived > survived.txt) & wait"},
		}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)

	if err := f.r.Run(ctx, "serve", nil, nil); err == nil {
		t.Fatal("run returned nil, want the cancellation error")
	}

	// Outlive the sleeper: if the group survived cancellation it writes the file
	// while we wait here, and a passing test would only mean we did not look.
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(filepath.Join(f.dir, "survived.txt")); err == nil {
		t.Error("survived.txt exists: cancelling killed the shell but not the process it started")
	}
}

// TestDeferredStepsStillRunAfterCancel guards the other half. exec.CommandContext
// refuses to START a process on a cancelled context, so passing the run's context
// straight through to teardown silently skipped every deferred step at the one
// moment they matter most — Ctrl-C on a task that brought a topology up.
func TestDeferredStepsStillRunAfterCancel(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"deploy": {Cmds: []chorefile.Cmd{
			{Cmd: "printf down > teardown.txt", Defer: true},
			{Cmd: "sleep 5"},
		}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)

	if err := f.r.Run(ctx, "deploy", nil, nil); err == nil {
		t.Fatal("run returned nil, want the cancellation error")
	}
	if got := f.read("teardown.txt"); got != "down" {
		t.Errorf("teardown.txt = %q, want %q — deferred teardown was skipped on cancel", got, "down")
	}
}

// TestCleanupContextOnlyReplacesACancelledOne: teardown must not get a fresh
// deadline while the run is healthy, or a `defer:` step in a long task would be
// given a budget the task itself never had.
func TestCleanupContextOnlyReplacesACancelledOne(t *testing.T) {
	healthy, cancelHealthy := context.WithCancel(context.Background())
	defer cancelHealthy()
	if got, done := cleanupContext(healthy); got != healthy {
		done()
		t.Error("a healthy context was replaced; teardown should inherit the run's own")
	} else {
		done()
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	got, done := cleanupContext(cancelled)
	defer done()
	if got.Err() != nil {
		t.Error("teardown context is already cancelled: no teardown could start")
	}
	if _, ok := got.Deadline(); !ok {
		t.Error("teardown context has no deadline: a wedged teardown would hang the run")
	}
}

// ---------- templated sources/generates ----------

// fingerprints returns every fingerprint file the run recorded, concatenated.
// The filenames carry a digest suffix, so the test globs rather than naming one
// — and it reads the JSON rather than only re-running the task, because "ran
// twice" and "recorded the wrong thing" are different failures with one symptom.
func (f *fixture) fingerprints() string {
	f.t.Helper()
	paths, err := filepath.Glob(filepath.Join(f.dir, ".chore", "fingerprints", "*.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var b strings.Builder
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			f.t.Fatal(err)
		}
		b.WriteString(filepath.Base(p))
		b.WriteString("\n")
		b.Write(body)
	}
	return b.String()
}

// A `sources:` pattern containing a template must be expanded before it is
// hashed, on the SAVE side as well as on the check side. Saving the raw pattern
// records the checksum of an empty file set, which can never equal the checksum
// of the rendered one — so the task rebuilds on every invocation, for ever, and
// nothing anywhere reports it.
func TestTemplatedSourcesGoUpToDate(t *testing.T) {
	f := newFixture(t, &chorefile.File{Vars: vars("EXT", "aaa")}, map[string]*chorefile.Task{
		"build": {
			Sources: []string{"src/*.{{.EXT}}"},
			Cmds:    cmds("echo ran >> log.txt"),
		},
	})
	f.write("src/a.aaa", "one")

	f.mustRun("build", nil, nil)
	f.mustRun("build", nil, nil)

	if got := strings.Count(f.read("log.txt"), "ran"); got != 1 {
		t.Errorf("build ran %d times, want 1: a templated sources: pattern never goes up to date", got)
	}
	// The direct evidence, and the reason a rerun count alone is not enough: the
	// recorded fingerprint must name the file it hashed. An empty set records no
	// sources at all, which reads as "nothing matched" rather than as a defect.
	if fp := f.fingerprints(); !strings.Contains(fp, "src/a.aaa") {
		t.Errorf("the stored fingerprint does not name the source it hashed:\n%s", fp)
	}
}

// Two tasks whose templated patterns render to DIFFERENT file sets must not
// store the same checksum. Hashing the patterns raw makes both match nothing, so
// both store the digest of the empty set — identical, and identical for a reason
// that has nothing to do with their contents.
func TestTemplatedSourcesOfDifferentTasksDiffer(t *testing.T) {
	f := newFixture(t, &chorefile.File{Vars: vars("EXT_A", "aaa", "EXT_B", "bbb")}, map[string]*chorefile.Task{
		"ta": {Sources: []string{"src/*.{{.EXT_A}}"}, Cmds: cmds("echo a > out-a.txt")},
		"tb": {Sources: []string{"src/*.{{.EXT_B}}"}, Cmds: cmds("echo b > out-b.txt")},
	})
	f.write("src/a.aaa", "one")
	f.write("src/b.bbb", "two")

	f.mustRun("ta", nil, nil)
	f.mustRun("tb", nil, nil)

	fp := f.fingerprints()
	hashes := map[string]bool{}
	for _, line := range strings.Split(fp, "\n") {
		if strings.Contains(line, `"hash"`) {
			hashes[strings.TrimSpace(line)] = true
		}
	}
	if len(hashes) != 2 {
		t.Errorf("two tasks over different source sets stored %d distinct hashes, want 2:\n%s", len(hashes), fp)
	}
}

// `generates:` is templated on the same path and has the same failure: the raw
// pattern is what gets recorded, so the next run stats a path with braces in it,
// finds nothing, and rebuilds.
func TestTemplatedGeneratesGoUpToDate(t *testing.T) {
	f := newFixture(t, &chorefile.File{Vars: vars("NAME", "lib")}, map[string]*chorefile.Task{
		"build": {
			Sources:   []string{"src/a.c"},
			Generates: []string{"out/{{.NAME}}.a"},
			Cmds:      cmds("mkdir -p out && echo built > out/{{.NAME}}.a", "echo ran >> log.txt"),
		},
	})
	f.write("src/a.c", "int main(){}")

	f.mustRun("build", nil, nil)
	f.mustRun("build", nil, nil)

	if got := strings.Count(f.read("log.txt"), "ran"); got != 1 {
		t.Errorf("build ran %d times, want 1: a templated generates: pattern never goes up to date", got)
	}
	if fp := f.fingerprints(); !strings.Contains(fp, "out/lib.a") || strings.Contains(fp, "{{") {
		t.Errorf("the stored fingerprint recorded an unrendered generates pattern:\n%s", fp)
	}
}

// ---------- call vars and parameter spelling ----------

// A value typed on the command line binds under the declared spelling AND its
// uppercase form (internal/cli's setParam). A value passed by a `- task:` or a
// `deps:` reference did not, so `vars: {out: …}` against `args: [out]` reached
// {{.out}} and not {{.OUT}} — and where anything lower in the scope defines OUT,
// {{.OUT}} kept THAT value instead. The build lands in the wrong directory and
// nothing is reported.
func TestCallVarsBindUnderBothSpellings(t *testing.T) {
	// A file var under the other spelling is what makes this silent rather than
	// merely empty: the mirror in scope() only fills a spelling that is EMPTY.
	file := &chorefile.File{Vars: vars("OUT", "/wrong/dir")}
	f := newFixture(t, file, map[string]*chorefile.Task{
		"staticlib": {
			Args: chorefile.Args{{Name: "out"}},
			Cmds: cmds(`echo "{{.OUT}}|{{.out}}" > seen.txt`),
		},
		"viaCmd":  {Cmds: []chorefile.Cmd{{Task: "staticlib", Vars: vars("out", "/right/path")}}},
		"viaDeps": {Deps: []chorefile.Dep{{Task: "staticlib", Vars: vars("out", "/right/path")}}, Cmds: cmds("true")},
	})

	for _, caller := range []string{"viaCmd", "viaDeps"} {
		f.mustRun(caller, nil, nil)
		if got := strings.TrimSpace(f.read("seen.txt")); got != "/right/path|/right/path" {
			t.Errorf("%s: {{.OUT}}|{{.out}} = %q, want %q", caller, got, "/right/path|/right/path")
		}
	}
}

// The same fold the command line applies: a spelling that differs only in case
// from the declaration names that parameter. `chore staticlib Out=x` binds; a
// `- task:` passing `vars: {Out: x}` was refused as a missing argument.
func TestCallVarsAreMatchedCaseInsensitively(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"staticlib": {
			Args: chorefile.Args{{Name: "out"}},
			Cmds: cmds(`echo "{{.OUT}}|{{.out}}" > seen.txt`),
		},
		"build": {Cmds: []chorefile.Cmd{{Task: "staticlib", Vars: vars("Out", "/right/path")}}},
	})
	f.mustRun("build", nil, nil)
	if got := strings.TrimSpace(f.read("seen.txt")); got != "/right/path|/right/path" {
		t.Errorf("{{.OUT}}|{{.out}} = %q, want %q", got, "/right/path|/right/path")
	}
}

// Folding must not silently pick a winner. One parameter given two different
// values under two spellings is the caller naming two things, and it is the
// same failure checkArgConflicts already refuses positionally.
func TestCallVarsDisagreeingOnSpellingIsAnError(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"staticlib": {
			Args: chorefile.Args{{Name: "out"}},
			Cmds: cmds(`echo "{{.OUT}}" > seen.txt`),
		},
		"build": {Cmds: []chorefile.Cmd{{Task: "staticlib", Vars: vars("out", "/a", "OUT", "/b")}}},
	})
	err := f.mustFail("build", nil, nil)
	mustContain(t, err.Error(), "given twice", "error")
	if f.exists("seen.txt") {
		t.Error("the task ran despite being given one parameter twice")
	}
}

// A bool parameter passed as a call var is normalised the same way a flag typed
// on the command line is, under both spellings — otherwise {{if .FORCE}} fires
// on the string "false".
func TestBoolCallVarNormalisedUnderBothSpellings(t *testing.T) {
	f := newFixture(t, nil, map[string]*chorefile.Task{
		"lib": {
			Args: chorefile.Args{{Name: "force", Type: chorefile.TypeBool}},
			Cmds: cmds(`echo "{{if .FORCE}}on{{else}}off{{end}}" > seen.txt`),
		},
		"off": {Cmds: []chorefile.Cmd{{Task: "lib", Vars: vars("force", "false")}}},
		"on":  {Cmds: []chorefile.Cmd{{Task: "lib", Vars: vars("force", "true")}}},
	})
	f.mustRun("off", nil, nil)
	if got := strings.TrimSpace(f.read("seen.txt")); got != "off" {
		t.Errorf("force=false read as %q, want %q", got, "off")
	}
	f.mustRun("on", nil, nil)
	if got := strings.TrimSpace(f.read("seen.txt")); got != "on" {
		t.Errorf("force=true read as %q, want %q", got, "on")
	}
}
