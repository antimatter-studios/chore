// Tests for the command line: what tsk consumes itself, what it hands to the
// task, and what it prints when there is nothing to run.
//
// Most of them drive Main end to end against a Taskfile written into
// t.TempDir(), because every property worth having here is about what actually
// reaches the runner — a parser-only test would pass just as happily if the
// flags were wired to nothing. Main calls os.Chdir for -C, so each test enters
// its directory with t.Chdir (whose cleanup puts the old one back) and NONE of
// them call t.Parallel: the process working directory is shared state.
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// ─── helpers ────────────────────────────────────────────────────────────────

// writeTree materialises a file tree under a fresh temp dir. Keys are slash
// separated paths relative to that dir. Every test builds its own tree so
// nothing depends on the repository's own Taskfiles or on where `go test` ran.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

type result struct {
	code   int
	stdout string
	stderr string
}

// runMain runs the command line from dir, capturing both streams separately —
// which stream a message lands on is part of the contract (usage goes to stderr,
// --help to stdout) and merging them would hide a regression in it.
func runMain(t *testing.T, dir string, args ...string) result {
	t.Helper()
	t.Chdir(dir)
	var out, errOut bytes.Buffer
	code := Main(args, &out, &errOut)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func (r result) String() string {
	return "exit " + itoa(r.code) + "\n--- stdout ---\n" + r.stdout + "--- stderr ---\n" + r.stderr
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func checkCode(t *testing.T, r result, want int) {
	t.Helper()
	if r.code != want {
		t.Errorf("exit code = %d, want %d\n%v", r.code, want, r)
	}
}

func checkContains(t *testing.T, r result, label, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%s does not contain %q\n%v", label, w, r)
		}
	}
}

func checkNotContains(t *testing.T, r result, label, got string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(got, w) {
			t.Errorf("%s unexpectedly contains %q\n%v", label, w, r)
		}
	}
}

// samePath compares two paths through their symlinks. t.TempDir() hands back a
// path under /var on macOS, and Main's own os.Chdir (for -C) leaves $PWD stale
// so os.Getwd reports the /private/var it resolves to — a plain string compare
// would fail for a reason that has nothing to do with the code under test.
func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	return resolve(a) == resolve(b)
}

// ─── flag parsing ───────────────────────────────────────────────────────────

// TestParseFlagsBeforeTheTaskName pins the flags tsk owns, in both spellings and
// both value forms, and the two ways the command line can be wrong.
func TestParseFlagsBeforeTheTaskName(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    options
		rest    []string
		wantErr string
	}{
		{name: "nothing at all", args: nil},

		{name: "short dir", args: []string{"-C", "/srv", "up"}, want: options{dir: "/srv"}, rest: []string{"up"}},
		{name: "long dir", args: []string{"--dir", "/srv", "up"}, want: options{dir: "/srv"}, rest: []string{"up"}},
		// `--flag=value` and `--flag value` must mean the same thing: both forms
		// are in every developer's fingers, and a runner that accepts only one
		// fails at the moment someone is already annoyed.
		{name: "long dir with equals", args: []string{"--dir=/srv", "up"}, want: options{dir: "/srv"}, rest: []string{"up"}},
		{name: "short dir with equals", args: []string{"-C=/srv", "up"}, want: options{dir: "/srv"}, rest: []string{"up"}},

		{name: "short file", args: []string{"-f", "Other.yml", "up"}, want: options{file: "Other.yml"}, rest: []string{"up"}},
		{name: "long file", args: []string{"--file", "Other.yml", "up"}, want: options{file: "Other.yml"}, rest: []string{"up"}},
		{name: "file with equals", args: []string{"--file=Other.yml", "up"}, want: options{file: "Other.yml"}, rest: []string{"up"}},
		// --taskfile is what go-task calls the same flag; accepting it costs a
		// word and saves a habit.
		{name: "taskfile alias", args: []string{"--taskfile", "Other.yml"}, want: options{file: "Other.yml"}},

		{name: "short list", args: []string{"-l"}, want: options{list: true}},
		{name: "long list", args: []string{"--list"}, want: options{list: true}},
		{name: "dry", args: []string{"--dry", "up"}, want: options{dry: true}, rest: []string{"up"}},
		{name: "dry-run alias", args: []string{"--dry-run", "up"}, want: options{dry: true}, rest: []string{"up"}},
		{name: "force", args: []string{"--force", "up"}, want: options{force: true}, rest: []string{"up"}},
		{name: "short verbose", args: []string{"-v", "up"}, want: options{verbose: true}, rest: []string{"up"}},
		{name: "long verbose", args: []string{"--verbose", "up"}, want: options{verbose: true}, rest: []string{"up"}},
		{name: "help", args: []string{"-h"}, want: options{help: true}},

		{
			name: "several flags then a task with arguments",
			args: []string{"-v", "--force", "--dir=/srv", "-f", "Other.yml", "up", "mail4.test"},
			want: options{dir: "/srv", file: "Other.yml", force: true, verbose: true},
			rest: []string{"up", "mail4.test"},
		},

		// Everything after -- is the task's, wherever -- appears.
		{name: "dashdash before any task", args: []string{"--", "-v", "x"}, want: options{cliArgs: []string{"-v", "x"}}},
		{name: "dashdash after the task", args: []string{"deploy", "--", "-v", "x"}, want: options{cliArgs: []string{"-v", "x"}}, rest: []string{"deploy"}},
		{name: "dashdash with nothing after it", args: []string{"deploy", "--"}, want: options{cliArgs: []string{}}, rest: []string{"deploy"}},

		{name: "unknown flag", args: []string{"--bogus"}, wantErr: `unknown flag "--bogus"`},
		{name: "unknown flag before a task", args: []string{"--bogus", "up"}, wantErr: `unknown flag "--bogus"`},
		{name: "flag with no value", args: []string{"--file"}, wantErr: "needs a value"},
		{name: "dir with no value", args: []string{"-C"}, wantErr: "needs a value"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, rest, err := parseFlags(c.args)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("parseFlags(%q) = %+v, want error containing %q", c.args, got, c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("parseFlags(%q) error = %v, want it to contain %q", c.args, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags(%q): %v", c.args, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseFlags(%q) options = %+v, want %+v", c.args, got, c.want)
			}
			if !reflect.DeepEqual(rest, c.rest) {
				t.Errorf("parseFlags(%q) rest = %q, want %q", c.args, rest, c.rest)
			}
		})
	}
}

// TestFlagsAfterTheTaskNameBelongToTheTask is the reason the parser is written by
// hand instead of using flag.FlagSet.
//
// `tsk logs -f api` has to mean "run logs with -f and api". A stdlib parser, or
// any parser that keeps scanning past the task name, reads -f as tsk's own
// --file, swallows "api" as the filename, and then runs nothing — which is
// exactly the failure that makes a task unable to take an argument.
func TestFlagsAfterTheTaskNameBelongToTheTask(t *testing.T) {
	t.Run("parser leaves them in rest", func(t *testing.T) {
		opts, rest, err := parseFlags([]string{"logs", "-f", "api"})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if opts.file != "" {
			t.Errorf("-f after the task name was consumed as --file=%q; it belongs to the task", opts.file)
		}
		if want := []string{"logs", "-f", "api"}; !reflect.DeepEqual(rest, want) {
			t.Errorf("rest = %q, want %q", rest, want)
		}
	})

	t.Run("a flag tsk does not know is not an error either", func(t *testing.T) {
		// --since is nobody's flag but the task's. Before the task name it would
		// be a usage error; after it, it is data.
		_, rest, err := parseFlags([]string{"logs", "--since", "5m"})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if want := []string{"logs", "--since", "5m"}; !reflect.DeepEqual(rest, want) {
			t.Errorf("rest = %q, want %q", rest, want)
		}
	})

	t.Run("the task really receives them", func(t *testing.T) {
		// Positional parameters bind under the names the task declares, verbatim.
		root := writeTree(t, map[string]string{
			"Taskfile.yml": `version: '3'
tasks:
  logs:
    args: [FOLLOW, SERVICE]
    cmds:
      - 'echo follow=[{{.FOLLOW}}] service=[{{.SERVICE}}]'
`,
		})
		got := runMain(t, root, "logs", "-f", "api")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "follow=[-f] service=[api]")
	})
}

// TestUnknownFlagIsAUsageError: a flag tsk does not know, before the task name,
// must stop the run rather than be passed on — the alternative is a typo'd
// --forse silently not forcing anything.
func TestUnknownFlagIsAUsageError(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Taskfile.yml": "version: '3'\ntasks:\n  build:\n    cmds: ['echo build']\n",
	})
	got := runMain(t, root, "--forse", "build")

	checkCode(t, got, 2) // 2 is "you typed it wrong", distinct from 1, "it failed"
	checkContains(t, got, "stderr", got.stderr, `unknown flag "--forse"`, "usage:", "-f, --file FILE")
	checkNotContains(t, got, "stdout", got.stdout, "build")
}

// TestHelpGoesToStdout: --help is an answer, not a complaint, so it must be
// pipeable (`tsk --help | less`) and exit 0.
func TestHelpGoesToStdout(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Taskfile.yml": "version: '3'\ntasks:\n  build:\n    cmds: ['echo build']\n",
	})
	got := runMain(t, root, "--help")

	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "usage:", "tsk [flags] <task> [args...]")
	if got.stderr != "" {
		t.Errorf("--help wrote to stderr: %q", got.stderr)
	}
}

// ─── word classification ────────────────────────────────────────────────────

// TestSplitArgs draws the line between a positional argument and a variable
// assignment. It matters because the words on the right of a real command line
// are hostnames, paths and URLs, and any of them may contain an `=`.
func TestSplitArgs(t *testing.T) {
	cases := []struct {
		name     string
		words    []string
		wantArgs []string
		wantVars map[string]string
	}{
		{
			name:     "nothing",
			words:    nil,
			wantArgs: nil,
			wantVars: nil, // nil, not an empty map: "no variables were given"
		},
		{
			name:     "NAME=value is a variable",
			words:    []string{"CONFIG=mail4.test"},
			wantVars: map[string]string{"CONFIG": "mail4.test"},
		},
		{
			// A domain is the single most likely argument in this project and it
			// contains a dot, not an equals — it must stay positional.
			name:     "a bare word is an argument",
			words:    []string{"mail4.test"},
			wantArgs: []string{"mail4.test"},
		},
		{
			// `=` alone does not make an assignment: the left side has to look
			// like a variable name, or `tsk deploy path/to=x` would silently set
			// a variable nobody can reference and drop the argument.
			name:     "a path with an equals stays an argument",
			words:    []string{"path/to=x"},
			wantArgs: []string{"path/to=x"},
		},
		{
			// Only the FIRST `=` splits, so a URL, a query string or a base64
			// value survives intact.
			name:     "the value keeps every later equals",
			words:    []string{"URL=https://host/p?a=b&c=d="},
			wantVars: map[string]string{"URL": "https://host/p?a=b&c=d="},
		},
		{
			name:     "an empty value is still an assignment",
			words:    []string{"CONFIG="},
			wantVars: map[string]string{"CONFIG": ""},
		},
		{
			name:     "arguments and variables interleave",
			words:    []string{"mail4.test", "CONFIG=b", "second.arg", "TAG=v1"},
			wantArgs: []string{"mail4.test", "second.arg"},
			wantVars: map[string]string{"CONFIG": "b", "TAG": "v1"},
		},
		{
			name:     "a leading digit is not a variable name",
			words:    []string{"3rd=x"},
			wantArgs: []string{"3rd=x"},
		},
		{
			name:     "an empty name is not a variable name",
			words:    []string{"=x"},
			wantArgs: []string{"=x"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args, vars, err := splitArgs(c.words, nil)
			if err != nil {
				t.Fatalf("splitArgs(%q): %v", c.words, err)
			}
			if !reflect.DeepEqual(args, c.wantArgs) {
				t.Errorf("splitArgs(%q) args = %q, want %q", c.words, args, c.wantArgs)
			}
			if !reflect.DeepEqual(vars, c.wantVars) {
				t.Errorf("splitArgs(%q) vars = %v, want %v", c.words, vars, c.wantVars)
			}
		})
	}
}

// A --flag naming a declared parameter is consumed as that parameter; anything
// else stays a positional word so it still reaches the task. Without that rule,
// adding named parameters would break `tsk logs -f api`.
func TestSplitArgsNamedParameters(t *testing.T) {
	params := map[string]param{"config": {name: "config"}}

	for _, c := range []struct {
		name      string
		words     []string
		wantArgs  []string
		wantVars  map[string]string
		wantError bool
	}{
		{
			name:     "--config value",
			words:    []string{"--config", "mail4.test"},
			wantVars: map[string]string{"config": "mail4.test", "CONFIG": "mail4.test"},
		},
		{
			name:     "--config=value",
			words:    []string{"--config=mail4.test"},
			wantVars: map[string]string{"config": "mail4.test", "CONFIG": "mail4.test"},
		},
		{
			name:     "case-insensitive match on the declared name",
			words:    []string{"--CONFIG", "mail4.test"},
			wantVars: map[string]string{"config": "mail4.test", "CONFIG": "mail4.test"},
		},
		{
			name:     "an undeclared flag passes through to the task",
			words:    []string{"-f", "api"},
			wantArgs: []string{"-f", "api"},
		},
		{
			name:     "an undeclared long flag passes through too",
			words:    []string{"--follow", "api"},
			wantArgs: []string{"--follow", "api"},
		},
		{
			name:     "NAME=value for a declared parameter sets both spellings",
			words:    []string{"CONFIG=mail4.test"},
			wantVars: map[string]string{"CONFIG": "mail4.test", "config": "mail4.test"},
		},
		{
			name:      "a declared flag with no value is an error",
			words:     []string{"--config"},
			wantError: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			args, vars, err := splitArgs(c.words, params)
			if c.wantError {
				if err == nil {
					t.Fatal("splitArgs = nil error, want one")
				}
				return
			}
			if err != nil {
				t.Fatalf("splitArgs: %v", err)
			}
			if !reflect.DeepEqual(args, c.wantArgs) {
				t.Errorf("args = %q, want %q", args, c.wantArgs)
			}
			if !reflect.DeepEqual(vars, c.wantVars) {
				t.Errorf("vars = %v, want %v", vars, c.wantVars)
			}
		})
	}
}

// A boolean parameter — one whose default is true/false — is complete on its
// own, so `--follow` must not swallow the word after it. Getting this wrong
// turns `tsk logs --follow api` into a request to follow a service called "api"
// with no filter, or an error, depending on which way it guesses.
func TestSplitArgsBooleanFlags(t *testing.T) {
	params := map[string]param{
		"follow": {name: "follow", boolean: true},
		"filter": {name: "filter"},
	}

	for _, c := range []struct {
		name     string
		words    []string
		wantArgs []string
		wantVars map[string]string
	}{
		{
			name:     "presence alone is the value, and the next word survives",
			words:    []string{"--follow", "api"},
			wantArgs: []string{"api"},
			wantVars: map[string]string{"follow": "true", "FOLLOW": "true"},
		},
		{
			name:     "explicit false becomes empty, so {{if}} and [ -n ] agree",
			words:    []string{"--follow=false"},
			wantVars: map[string]string{"follow": "", "FOLLOW": ""},
		},
		{
			name:     "explicit true",
			words:    []string{"--follow=yes"},
			wantVars: map[string]string{"follow": "true", "FOLLOW": "true"},
		},
		{
			name:     "a value parameter still takes the next word",
			words:    []string{"--filter", "api"},
			wantVars: map[string]string{"filter": "api", "FILTER": "api"},
		},
		{
			name:     "NAME=value normalises for a boolean too",
			words:    []string{"FOLLOW=0"},
			wantVars: map[string]string{"FOLLOW": "", "follow": ""},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			args, vars, err := splitArgs(c.words, params)
			if err != nil {
				t.Fatalf("splitArgs: %v", err)
			}
			if !reflect.DeepEqual(args, c.wantArgs) {
				t.Errorf("args = %q, want %q", args, c.wantArgs)
			}
			if !reflect.DeepEqual(vars, c.wantVars) {
				t.Errorf("vars = %v, want %v", vars, c.wantVars)
			}
		})
	}
}

func TestIsVarName(t *testing.T) {
	cases := map[string]bool{
		"CONFIG":     true,
		"config":     true,
		"_private":   true,
		"MAIL3_PORT": true,
		"a1":         true,
		"":           false,
		"1BAD":       false, // a name cannot start with a digit
		"path/to":    false,
		"mail4.test": false, // the dot is why a domain is never an assignment
		"WITH SPACE": false,
		"WITH-DASH":  false,
		"a$b":        false,
	}
	for in, want := range cases {
		if got := isVarName(in); got != want {
			t.Errorf("isVarName(%q) = %v, want %v", in, got, want)
		}
	}
}

// ─── the headline behaviour ─────────────────────────────────────────────────

// TestDotenvSeesCommandLineVariables is the regression guard for the defect this
// program exists to remove (SPEC "Why" #2, "Fixed semantics" #3).
//
// `dotenv:` is templated on {{.CONFIG}} and two configs exist on disk. Task
// resolves dotenv while PARSING the Taskfile, before command-line variables
// exist, so `task <x> CONFIG=b` loads config a's environment and then acts on
// the wrong stack without a word. Here the variable must reach the dotenv path.
func TestDotenvSeesCommandLineVariables(t *testing.T) {
	// The process environment is the scope's bottom layer, so an inherited
	// CONFIG would change what `default` falls back to. Blank it out: these
	// tests are about what the command line says, not what the shell had.
	t.Setenv("CONFIG", "")

	tree := func(taskfile string) map[string]string {
		return map[string]string{
			"Taskfile.yml":        taskfile,
			"config/a/config.env": "TSK_TEST_HOST=alpha.example\n",
			"config/b/config.env": "TSK_TEST_HOST=bravo.example\n",
		}
	}

	// Two Taskfiles that a real project would actually write. Both must obey the
	// command line; rest-mail uses the first form.
	forms := map[string]string{
		"file var defaults from the caller": `version: '3'
vars:
  CONFIG: '{{.CONFIG | default "a"}}'
dotenv:
  - 'config/{{.CONFIG}}/config.env'
tasks:
  show:
    cmds:
      - 'echo config={{.CONFIG}} host={{.TSK_TEST_HOST}}'
`,
		"default written into the dotenv path": `version: '3'
dotenv:
  - 'config/{{.CONFIG | default "a"}}/config.env'
tasks:
  show:
    cmds:
      - 'echo host={{.TSK_TEST_HOST}}'
`,
	}

	for name, taskfile := range forms {
		t.Run(name, func(t *testing.T) {
			t.Run("no variable given uses the default config", func(t *testing.T) {
				root := writeTree(t, tree(taskfile))
				got := runMain(t, root, "show")
				checkCode(t, got, 0)
				checkContains(t, got, "stdout", got.stdout, "host=alpha.example")
			})

			t.Run("CONFIG=b acts on config b", func(t *testing.T) {
				root := writeTree(t, tree(taskfile))
				got := runMain(t, root, "show", "CONFIG=b")
				checkCode(t, got, 0)
				checkContains(t, got, "stdout", got.stdout, "host=bravo.example")
				// The whole point: not a trace of the default's environment.
				checkNotContains(t, got, "stdout", got.stdout, "alpha.example")
			})

			t.Run("a config that does not exist fails loudly", func(t *testing.T) {
				// SPEC "Fixed semantics" #4. The alternative — every variable
				// silently defaulting — is how container names resolve to
				// "-suffix" and commands quietly match nothing.
				root := writeTree(t, tree(taskfile))
				got := runMain(t, root, "show", "CONFIG=nope")
				if got.code == 0 {
					t.Errorf("a missing config exited 0; it must never silently fall back\n%v", got)
				}
				checkContains(t, got, "stderr", got.stderr, "no environment loaded", "config/nope/config.env")
			})
		})
	}
}

// TestDotenvWithALiteralFileVarIsAKnownDeviation documents behaviour that does
// NOT match SPEC "Fixed semantics" #2 (`call vars` outrank `file vars`).
//
// internal/run builds the scope used to render the dotenv path as
// base→args→callVars→fileVars, so a file var written as a plain literal wins
// over the command line and the wrong environment is loaded — while the same
// variable resolves to the command-line value everywhere else, so the run acts
// on config b with config a's environment. Real Taskfiles do not hit it because
// they write the self-defaulting form (`{{.CONFIG | default "a"}}`), which is why
// this survived a 153-task acceptance run.
//
// The assertion below pins today's behaviour so the bug cannot spread quietly.
// When internal/run pushes fileVars UNDER callVars, this test fails: that is the
// signal to flip it to want "bravo.example" and delete this comment.
// The command line outranks a literal file var for the dotenv path too, not just
// for the task's own variables. Before this was fixed, `show CONFIG=b` ran with
// CONFIG=b while loading config a's environment — the wrong-stack failure this
// program exists to remove, one layer below where anyone would look for it.
func TestDotenvPathHonoursTheCommandLineOverALiteralFileVar(t *testing.T) {
	t.Setenv("CONFIG", "")
	root := writeTree(t, map[string]string{
		"Taskfile.yml": `version: '3'
vars:
  CONFIG: a
dotenv:
  - 'config/{{.CONFIG}}/config.env'
tasks:
  show:
    cmds:
      - 'echo config={{.CONFIG}} host={{.TSK_TEST_HOST}}'
`,
		"config/a/config.env": "TSK_TEST_HOST=alpha.example\n",
		"config/b/config.env": "TSK_TEST_HOST=bravo.example\n",
	})

	got := runMain(t, root, "show", "CONFIG=b")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "config=b")
	checkContains(t, got, "stdout", got.stdout, "host=bravo.example")
}

// ─── listing ────────────────────────────────────────────────────────────────

type listed struct{ group, name, desc string }

var (
	groupLine = regexp.MustCompile(`^ {2}\[(.+)\]$`)
	taskLine  = regexp.MustCompile(`^ {4}(\S+)(?: +(.*\S))?\s*$`)
)

func parseList(t *testing.T, out string) []listed {
	t.Helper()
	var entries []listed
	group := ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case line == "" || line == "tasks:":
		case groupLine.MatchString(line):
			group = groupLine.FindStringSubmatch(line)[1]
		case taskLine.MatchString(line):
			m := taskLine.FindStringSubmatch(line)
			entries = append(entries, listed{group: group, name: m[1], desc: m[2]})
		default:
			t.Fatalf("unrecognised line in listing: %q\n--- listing ---\n%s", line, out)
		}
	}
	return entries
}

// TestListing covers the answer tsk gives when there is nothing to run, which is
// the first thing anyone sees in an unfamiliar project.
func TestListing(t *testing.T) {
	files := map[string]string{
		"Taskfile.yml": `version: '3'
includes:
  db:
    taskfile: ./db.yml
tasks:
  build:
    desc: compile everything
    aliases: [b, compile]
    cmds: ['echo built']
  scratch:
    cmds: ['echo scratch']
  _prepare:
    internal: true
    desc: wiring, not a verb
    cmds: ['echo prepare']
`,
		"db.yml": `version: '3'
tasks:
  up:
    desc: start the database
    cmds: ['echo db up']
  down:
    cmds: ['echo db down']
`,
	}

	want := []listed{
		{group: "(root)", name: "build", desc: "compile everything"},
		{group: "(root)", name: "scratch"},
		{group: "db", name: "db:down"},
		{group: "db", name: "db:up", desc: "start the database"},
	}

	t.Run("no task named lists everything", func(t *testing.T) {
		root := writeTree(t, files)
		got := runMain(t, root)
		checkCode(t, got, 0)
		if entries := parseList(t, got.stdout); !reflect.DeepEqual(entries, want) {
			t.Errorf("listing = %+v\nwant %+v\n%v", entries, want, got)
		}
		// An internal task is wiring; showing it in the menu invites someone to
		// call it. An alias appears once, under the name the Taskfile defines.
		checkNotContains(t, got, "stdout", got.stdout, "_prepare", "wiring, not a verb", "compile\n")
	})

	t.Run("--list gives exactly the same answer", func(t *testing.T) {
		// `tsk` on its own is never a mystery: it is --list, byte for byte, so
		// nobody has to learn which of the two tells the truth.
		root := writeTree(t, files)
		bare := runMain(t, root)
		listed := runMain(t, root, "--list")
		if bare.stdout != listed.stdout {
			t.Errorf("`tsk` and `tsk --list` disagree\n--- tsk ---\n%s--- tsk --list ---\n%s", bare.stdout, listed.stdout)
		}
		checkCode(t, listed, 0)
	})

	t.Run("an alias is hidden from the list but still runs", func(t *testing.T) {
		root := writeTree(t, files)
		got := runMain(t, root, "b")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "built")
	})

	t.Run("a Taskfile with no tasks says so", func(t *testing.T) {
		root := writeTree(t, map[string]string{"Taskfile.yml": "version: '3'\n"})
		got := runMain(t, root)
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "no tasks")
	})
}

// ─── finding the Taskfile ───────────────────────────────────────────────────

// TestTaskfileDiscovery: where tsk looks, and what it says when it finds
// nothing.
func TestTaskfileDiscovery(t *testing.T) {
	files := map[string]string{
		"Taskfile.yml": `version: '3'
tasks:
  where:
    cmds:
      - 'echo root={{.ROOT_DIR}}'
`,
		"sub/deeper/keep": "",
	}

	t.Run("in the current directory", func(t *testing.T) {
		root := writeTree(t, files)
		got := runMain(t, root, "where")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "root=")
	})

	t.Run("searching upward from a subdirectory", func(t *testing.T) {
		// Running `tsk build` three directories down is the normal case, and the
		// task must still run against the project root, not the cwd.
		root := writeTree(t, files)
		got := runMain(t, filepath.Join(root, "sub", "deeper"), "where")
		checkCode(t, got, 0)
		reported := reportedRoot(t, got)
		if !samePath(t, reported, root) {
			t.Errorf("ROOT_DIR = %q, want the project root %q", reported, root)
		}
	})

	t.Run("-C changes directory first", func(t *testing.T) {
		root := writeTree(t, files)
		elsewhere := t.TempDir()
		got := runMain(t, elsewhere, "-C", root, "where")
		checkCode(t, got, 0)
		if reported := reportedRoot(t, got); !samePath(t, reported, root) {
			t.Errorf("ROOT_DIR = %q, want %q", reported, root)
		}
	})

	t.Run("-C to a directory that is not there", func(t *testing.T) {
		got := runMain(t, t.TempDir(), "-C", filepath.Join(t.TempDir(), "nope"), "where")
		checkCode(t, got, 1)
		checkContains(t, got, "stderr", got.stderr, "tsk:")
	})

	t.Run("-f names the file explicitly", func(t *testing.T) {
		// An explicitly named file wins over discovery, and its own directory —
		// not the cwd — is the project root.
		root := writeTree(t, map[string]string{
			"Other.yml": `version: '3'
tasks:
  where:
    cmds:
      - 'echo root={{.ROOT_DIR}}'
`,
		})
		got := runMain(t, t.TempDir(), "-f", filepath.Join(root, "Other.yml"), "where")
		checkCode(t, got, 0)
		if reported := reportedRoot(t, got); !samePath(t, reported, root) {
			t.Errorf("ROOT_DIR = %q, want %q", reported, root)
		}
	})

	t.Run("-f naming a file that is not there", func(t *testing.T) {
		root := writeTree(t, files)
		got := runMain(t, root, "-f", "Missing.yml", "where")
		checkCode(t, got, 1)
		checkContains(t, got, "stderr", got.stderr, "Missing.yml")
	})

	t.Run("nothing found anywhere", func(t *testing.T) {
		dir := t.TempDir()
		requireNoTaskfileAbove(t, dir)
		got := runMain(t, dir, "where")
		checkCode(t, got, 1)
		// The message has to name the thing that was looked for; "not found" on
		// its own sends people hunting for a config problem they do not have.
		checkContains(t, got, "stderr", got.stderr, "no Taskfile.yml here or in any parent directory")
	})
}

func reportedRoot(t *testing.T, r result) string {
	t.Helper()
	for _, line := range strings.Split(r.stdout, "\n") {
		if after, ok := strings.CutPrefix(line, "root="); ok && after != "" {
			return after
		}
	}
	t.Fatalf("no root= line in output\n%v", r)
	return ""
}

// requireNoTaskfileAbove skips rather than fails if some ancestor of the temp
// dir holds a Taskfile: the upward search would then succeed, and the test would
// be asserting something about the machine rather than about the code.
func requireNoTaskfileAbove(t *testing.T, dir string) {
	t.Helper()
	for d := dir; ; {
		for _, name := range []string{"Taskfile.yml", "Taskfile.yaml", "taskfile.yml"} {
			if _, err := os.Stat(filepath.Join(d, name)); err == nil {
				t.Skipf("%s exists, so the upward search cannot fail from %s", filepath.Join(d, name), dir)
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return
		}
		d = parent
	}
}

// ─── exit codes ─────────────────────────────────────────────────────────────

// TestExitCodes: tsk is run from scripts and from CI, where the exit code is the
// only thing read. A command's own status has to survive to the caller
// (SPEC "Fixed semantics" #7).
func TestExitCodes(t *testing.T) {
	files := map[string]string{
		"Taskfile.yml": `version: '3'
tasks:
  ok:
    cmds: ['true']
  boom:
    cmds: ['exit 42']
  build:
    cmds: ['echo built']
`,
	}

	t.Run("success is zero", func(t *testing.T) {
		got := runMain(t, writeTree(t, files), "ok")
		checkCode(t, got, 0)
	})

	t.Run("a failing command exits with its own code", func(t *testing.T) {
		// Not 1: a script that branches on `tsk deploy; case $? in 42) …` has to
		// see what the command actually returned.
		got := runMain(t, writeTree(t, files), "boom")
		checkCode(t, got, 42)
		checkContains(t, got, "stderr", got.stderr, "exit status 42")
	})

	t.Run("an unknown task names itself and suggests", func(t *testing.T) {
		got := runMain(t, writeTree(t, files), "buil")
		if got.code == 0 {
			t.Errorf("an unknown task exited 0\n%v", got)
		}
		checkContains(t, got, "stderr", got.stderr, `no task "buil"`, "did you mean", "build")
		checkNotContains(t, got, "stdout", got.stdout, "built")
	})
}

// ─── passthrough and the remaining flags ────────────────────────────────────

// TestCLIArgsPassthrough: everything after -- is the task's to forward, which is
// how a wrapper task hands options to the program it wraps.
func TestCLIArgsPassthrough(t *testing.T) {
	files := map[string]string{
		"Taskfile.yml": `version: '3'
tasks:
  echoargs:
    cmds:
      - 'echo cli=[{{.CLI_ARGS}}]'
  logs:
    args: [SERVICE]
    cmds:
      - 'echo service=[{{.SERVICE}}] cli=[{{.CLI_ARGS}}]'
`,
	}

	t.Run("words after -- become CLI_ARGS", func(t *testing.T) {
		got := runMain(t, writeTree(t, files), "echoargs", "--", "-race", "./...")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "cli=[-race ./...]")
	})

	t.Run("CLI_ARGS is a single string joined with spaces", func(t *testing.T) {
		// A quoted word does not survive as one word — CLI_ARGS is text spliced
		// into a script, not an argv. Pinned so nobody assumes otherwise.
		got := runMain(t, writeTree(t, files), "echoargs", "--", "a", "b c")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "cli=[a b c]")
	})

	t.Run("empty when there is no --", func(t *testing.T) {
		got := runMain(t, writeTree(t, files), "echoargs")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "cli=[]")
	})

	t.Run("arguments and passthrough coexist", func(t *testing.T) {
		got := runMain(t, writeTree(t, files), "logs", "api", "--", "--tail", "10")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "service=[api] cli=[--tail 10]")
	})
}

// TestFlagsReachTheRunner checks the three flags whose only job is to change how
// the runner behaves. Parsing them into a struct proves nothing; each one is
// asserted by its effect.
func TestFlagsReachTheRunner(t *testing.T) {
	files := map[string]string{
		"Taskfile.yml": `version: '3'
tasks:
  touch:
    cmds: ['touch marker.txt']
  done:
    status: ['true']
    cmds: ['echo ran-anyway']
  quiet:
    silent: true
    cmds: ['echo hello']
`,
	}

	t.Run("--dry prints without doing", func(t *testing.T) {
		root := writeTree(t, files)
		got := runMain(t, root, "--dry", "touch")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "touch marker.txt")
		if _, err := os.Stat(filepath.Join(root, "marker.txt")); err == nil {
			t.Errorf("--dry created marker.txt; it must only print")
		}
	})

	t.Run("--force overrides an up-to-date check", func(t *testing.T) {
		root := writeTree(t, files)
		skipped := runMain(t, root, "done")
		checkCode(t, skipped, 0)
		checkContains(t, skipped, "stdout", skipped.stdout, "up to date")
		checkNotContains(t, skipped, "stdout", skipped.stdout, "ran-anyway")

		forced := runMain(t, root, "--force", "done")
		checkCode(t, forced, 0)
		checkContains(t, forced, "stdout", forced.stdout, "ran-anyway")
	})

	t.Run("-v echoes commands a silent task hides", func(t *testing.T) {
		root := writeTree(t, files)
		quiet := runMain(t, root, "quiet")
		checkCode(t, quiet, 0)
		checkContains(t, quiet, "stdout", quiet.stdout, "hello")
		checkNotContains(t, quiet, "stdout", quiet.stdout, "echo hello")

		loud := runMain(t, root, "-v", "quiet")
		checkCode(t, loud, 0)
		checkContains(t, loud, "stdout", loud.stdout, "echo hello", "hello")
	})
}
