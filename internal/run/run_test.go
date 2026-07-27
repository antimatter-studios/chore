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

	"github.com/rest-mail/go-tsk/internal/taskfile"
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
	file *taskfile.File
	r    *Runner
	out  *syncBuf
	err  *syncBuf
}

// newFixture wires a Project the way the loader would: the file knows its
// directory, every task knows its name and its file, and RootDir is the temp
// directory — which is also where every task runs, so a relative `> marker.txt`
// in a command lands somewhere the test can read it.
func newFixture(t *testing.T, file *taskfile.File, tasks map[string]*taskfile.Task) *fixture {
	t.Helper()
	dir := t.TempDir()
	if file == nil {
		file = &taskfile.File{}
	}
	file.Path = filepath.Join(dir, "Taskfile.yml")
	file.Dir = dir
	file.Tasks = tasks
	for name, task := range tasks {
		task.Name = name
		task.File = file
	}
	out, errOut := &syncBuf{}, &syncBuf{}
	project := &taskfile.Project{Root: file, Tasks: tasks, RootDir: dir}
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
func cmds(scripts ...string) []taskfile.Cmd {
	out := make([]taskfile.Cmd, 0, len(scripts))
	for _, s := range scripts {
		out = append(out, taskfile.Cmd{Cmd: s})
	}
	return out
}

// vars builds a `vars:` block from alternating key/value literals.
func vars(kv ...string) map[string]taskfile.Var {
	if len(kv)%2 != 0 {
		panic("vars: odd number of arguments")
	}
	out := map[string]taskfile.Var{}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = taskfile.Var{Value: kv[i+1]}
	}
	return out
}

func depsOn(names ...string) []taskfile.Dep {
	out := make([]taskfile.Dep, 0, len(names))
	for _, n := range names {
		out = append(out, taskfile.Dep{Task: n})
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
// `args: [config!]` insists on an explicit value: a default no longer satisfies
// the parameter. That is the one thing omitting a default cannot express — "there
// IS a sensible default, but you must still say which one you mean."
func TestRequiredMarkerIgnoresTheDefault(t *testing.T) {
	newTask := func() map[string]*taskfile.Task {
		return map[string]*taskfile.Task{
			"up": {
				Args: []string{"config!"},
				Vars: map[string]taskfile.Var{"config": {Value: "restmail.test"}},
				Cmds: cmds(`printf '%s' '{{.CONFIG}}' > out.txt`),
			},
		}
	}

	// A default exists, but the marker means it cannot stand in for a choice.
	f := newFixture(t, nil, newTask())
	err := f.mustFail("up", nil, nil)
	mustContain(t, err.Error(), "must be given explicitly", "error")
	if f.exists("out.txt") {
		t.Error("the task ran without the value it insists on")
	}

	// Supplied positionally — the marker is not part of the name.
	f2 := newFixture(t, nil, newTask())
	f2.mustRun("up", []string{"mail4.test"}, nil)
	if got := f2.read("out.txt"); got != "mail4.test" {
		t.Errorf("out.txt = %q, want mail4.test", got)
	}

	// Supplied by name.
	f3 := newFixture(t, nil, newTask())
	f3.mustRun("up", nil, map[string]string{"CONFIG": "mail4.test"})
	if got := f3.read("out.txt"); got != "mail4.test" {
		t.Errorf("out.txt = %q, want mail4.test", got)
	}
}

// An explicitly empty default marks a parameter optional. Without this, there is
// no way to say "may be omitted, and empty is meaningful" — and it is why
// `args:` needs no required/optional marker: a default's presence is the marker.
func TestEmptyDefaultMakesAParameterOptional(t *testing.T) {
	f := newFixture(t, nil, map[string]*taskfile.Task{
		"list": {
			Args: []string{"filter"},
			Vars: map[string]taskfile.Var{"filter": {Value: ""}},
			Cmds: cmds(`printf 'filter=[%s]' '{{.FILTER}}' > out.txt`),
		},
		"needed": {
			Args: []string{"config"},
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
		"up": {Args: []string{"config"}, Cmds: cmds(`printf ran > ran.txt`)},
	})

	err := f.mustFail("up", []string{"mail4.test"}, map[string]string{"CONFIG": "restmail.test"})
	mustContain(t, err.Error(), "given twice", "error")
	if f.exists("ran.txt") {
		t.Error("the task ran despite contradictory arguments")
	}

	// The same value twice is not a contradiction.
	f2 := newFixture(t, nil, map[string]*taskfile.Task{
		"up": {Args: []string{"config"}, Cmds: cmds(`printf ran > ran.txt`)},
	})
	f2.mustRun("up", []string{"mail4.test"}, map[string]string{"CONFIG": "mail4.test"})
}

// A parameter's DEFAULT must reach the dotenv path, which is the thing usually
// keyed on it. Task vars as a whole are resolved after dotenv (they may read its
// values), so parameter defaults specifically are resolved earlier — otherwise
// `tsk up` with no argument renders `config//config.env` and fails, while
// `tsk up mail4.test` works, which is a baffling way to greet a new user.
func TestParameterDefaultReachesTheDotenvPath(t *testing.T) {
	f := newFixture(t, &taskfile.File{
		Dotenv: []string{"config/{{.CONFIG}}/config.env"},
	}, map[string]*taskfile.Task{
		"up": {
			Args: []string{"config"},
			Vars: map[string]taskfile.Var{"config": {Value: "alpha"}},
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
		"up": {
			Args: []string{"config"},
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
		f := newFixture(t, nil, map[string]*taskfile.Task{
			"up": {Args: []string{"config"}, Cmds: cmds("printf ran > ran.txt")},
		})
		err := f.mustFail("up", []string{"a", "b"}, nil)
		mustContain(t, err.Error(), "up", "error")
		mustContain(t, err.Error(), "takes 1 argument(s) (config), got 2", "error")
		if f.exists("ran.txt") {
			t.Error("the task ran despite the binding error")
		}
	})

	t.Run("task with no parameters", func(t *testing.T) {
		f := newFixture(t, nil, map[string]*taskfile.Task{
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
		"deploy": {
			Args: []string{"ENV", "TAG"},
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
		// (`tsk up mail4.test CONFIG=other`) and is rejected rather than silently
		// resolved — the caller named two configs and would get one.
		f := newFixture(t, nil, map[string]*taskfile.Task{
			"probe": {Args: []string{"V"}, Cmds: cmds(probe)},
		})
		err := f.mustFail("probe", []string{"from-arg"}, map[string]string{"V": "from-call"})
		mustContain(t, err.Error(), "given twice", "error")
	})

	t.Run("call var beats task var", func(t *testing.T) {
		f := newFixture(t, nil, map[string]*taskfile.Task{
			"probe": {Vars: vars("V", "from-task"), Cmds: cmds(probe)},
		})
		f.mustRun("probe", nil, map[string]string{"V": "from-call"})
		if got := f.read("out.txt"); got != "from-call" {
			t.Errorf("V = %q, want from-call", got)
		}
	})

	t.Run("task var beats file var", func(t *testing.T) {
		f := newFixture(t, &taskfile.File{Vars: vars("V", "from-file")}, map[string]*taskfile.Task{
			"probe": {Vars: vars("V", "from-task"), Cmds: cmds(probe)},
		})
		f.mustRun("probe", nil, nil)
		if got := f.read("out.txt"); got != "from-task" {
			t.Errorf("V = %q, want from-task", got)
		}
	})

	t.Run("file var beats dotenv", func(t *testing.T) {
		f := newFixture(t, &taskfile.File{
			Dotenv: []string{"app.env"},
			Vars:   vars("V", "from-file"),
		}, map[string]*taskfile.Task{
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
		f := newFixture(t, &taskfile.File{Dotenv: []string{"app.env"}}, map[string]*taskfile.Task{
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
		f := newFixture(t, nil, map[string]*taskfile.Task{
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
		f := newFixture(t, &taskfile.File{
			Dotenv: []string{"config/{{.CONFIG}}/config.env"},
		}, map[string]*taskfile.Task{
			// Both spellings are asserted: {{.STACK}} proves the value reached
			// the template scope, "$STACK" proves it reached the script's
			// environment, and a real Taskfile relies on both.
			"up": {
				Args: []string{"CONFIG"},
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
		// `tsk up CONFIG=beta` — the exact invocation Task accepted and got wrong.
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
		"top": {Deps: []taskfile.Dep{
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
		"deploy": {Cmds: []taskfile.Cmd{
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
		"deploy": {Cmds: []taskfile.Cmd{
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
		"deploy": {Cmds: []taskfile.Cmd{
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
		"deploy": {Cmds: []taskfile.Cmd{
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
		"deploy": {Cmds: []taskfile.Cmd{
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
		f := newFixture(t, nil, map[string]*taskfile.Task{
			"check": {Cmds: []taskfile.Cmd{
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
		f := newFixture(t, nil, map[string]*taskfile.Task{
			"check": {Cmds: []taskfile.Cmd{
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
		f := newFixture(t, nil, map[string]*taskfile.Task{
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
		"deploy": {
			Args:     []string{"TARGET"},
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

// TestExitCodePropagates covers SPEC "Fixed semantics" #7: tsk exits with the
// code its command did, so a caller's `if tsk x; then` and its retry logic keep
// working.
func TestExitCodePropagates(t *testing.T) {
	f := newFixture(t, nil, map[string]*taskfile.Task{
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
		"outer": {Cmds: []taskfile.Cmd{{Task: "inner"}}},
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
	f := newFixture(t, nil, map[string]*taskfile.Task{
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
		return newFixture(t, nil, map[string]*taskfile.Task{
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
		f := newFixture(t, nil, map[string]*taskfile.Task{
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
		f := newFixture(t, nil, map[string]*taskfile.Task{
			"in-sub": {
				Dir:  "stacks/{{.NAME}}",
				Args: []string{"NAME"},
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
		f := newFixture(t, nil, map[string]*taskfile.Task{
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
