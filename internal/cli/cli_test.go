// Tests for the command line: what chore consumes itself, what it hands to the
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
	"time"

	"github.com/antimatter-studios/chore/internal/buildinfo"
	"github.com/antimatter-studios/chore/internal/chorefile"
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

// TestParseFlagsBeforeTheTaskName pins the flags chore owns, in both spellings and
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
// `chore logs -f api` has to mean "run logs with -f and api". A stdlib parser, or
// any parser that keeps scanning past the task name, reads -f as chore's own
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

	t.Run("a flag chore does not know is not an error either", func(t *testing.T) {
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

// TestUnknownFlagIsAUsageError: a flag chore does not know, before the task name,
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
// pipeable (`chore --help | less`) and exit 0.
func TestHelpGoesToStdout(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Taskfile.yml": "version: '3'\ntasks:\n  build:\n    cmds: ['echo build']\n",
	})
	got := runMain(t, root, "--help")

	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "usage:", "chore [flags] <task> [args...]")
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
			// like a variable name, or `chore deploy path/to=x` would silently set
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
// adding named parameters would break `chore logs -f api`.
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
// turns `chore logs --follow api` into a request to follow a service called "api"
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

// chores.yml is this program's file; Taskfile.yml is accepted only so a
// repository can migrate, and says so, because one file claimed by two runners
// that disagree about `args:` and about `task <t> VAR=value` is a trap.
func TestFilenamePrecedenceAndMigrationNotice(t *testing.T) {
	t.Run("chores.yml wins over Taskfile.yml", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"chores.yml":   "version: '3'\ntasks:\n  which:\n    cmds: ['echo choresfile']\n",
			"Taskfile.yml": "version: '3'\ntasks:\n  which:\n    cmds: ['echo taskfile']\n",
		})
		got := runMain(t, root, "which")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "choresfile")
		if strings.Contains(got.stderr, "rename it") {
			t.Errorf("stderr = %q, want no migration notice when chores.yml exists", got.stderr)
		}
	})

	t.Run("Taskfile.yml still works, with a notice", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"Taskfile.yml": "version: '3'\ntasks:\n  which:\n    cmds: ['echo taskfile']\n",
		})
		got := runMain(t, root, "which")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "taskfile")
		checkContains(t, got, "stderr", got.stderr, "rename it to chores.yml")
	})
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

// TestListing covers the answer chore gives when there is nothing to run, which is
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
		// `chore` on its own is never a mystery: it is --list, byte for byte, so
		// nobody has to learn which of the two tells the truth.
		root := writeTree(t, files)
		bare := runMain(t, root)
		listed := runMain(t, root, "--list")
		if bare.stdout != listed.stdout {
			t.Errorf("`chore` and `chore --list` disagree\n--- chore ---\n%s--- chore --list ---\n%s", bare.stdout, listed.stdout)
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

// TestTaskfileDiscovery: where chore looks, and what it says when it finds
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
		// Running `chore build` three directories down is the normal case, and the
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
		checkContains(t, got, "stderr", got.stderr, "chore:")
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
		checkContains(t, got, "stderr", got.stderr, "no chores.yml here or in any parent directory")
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
		for _, name := range []string{"Taskfile.yml", "Taskfile.yaml", "chorefile.yml"} {
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

// TestExitCodes: chore is run from scripts and from CI, where the exit code is the
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
		// Not 1: a script that branches on `chore deploy; case $? in 42) …` has to
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

// TestAge: --version answers "how old is this", so the age is computed at run time
// from a stamp that is fixed. Only the stamp must be stable for a rebuild to be
// byte-identical; reading the clock here is fine and is the point.
func TestAge(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{time.Minute, "1 minute ago"},
		{90 * time.Minute, "1 hour ago"},
		{25 * time.Hour, "1 day ago"},
		{72 * time.Hour, "3 days ago"},
		{70 * 24 * time.Hour, "2 months ago"},
	} {
		if got := age(c.d); got != c.want {
			t.Errorf("age(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestDatedRendersOrPassesThrough(t *testing.T) {
	if got := dated(""); got != "" {
		t.Errorf("dated(\"\") = %q, want empty so the row is dropped", got)
	}
	// Unparseable input is shown as-is rather than swallowed: better a raw string
	// than a silently missing row.
	if got := dated("not-a-date"); got != "not-a-date" {
		t.Errorf("dated(junk) = %q, want it passed through", got)
	}
	if got := dated("2026-07-28T11:21:58Z"); !strings.Contains(got, "2026-07-28") || !strings.Contains(got, "ago") {
		t.Errorf("dated() = %q, want the date and its age", got)
	}
}

// TestTaskHelp: `chore <task> --help` describes the task and RUNS NOTHING.
//
// Everything after a task name is otherwise the task's own data, so --help bound as
// a positional argument and `chore instance:up --help` started a database. A flag
// that reads as "tell me about this" must never do anything.
func TestTaskHelp(t *testing.T) {
	root := writeTree(t, map[string]string{
		"chores.yml": `version: '3'
vars:
  CONFIG: '{{.CONFIG | default "restmail.test"}}'
tasks:
  up:
    desc: Bring up an instance
    args:
      - {name: config, desc: which instance}
      - {name: follow, type: bool, desc: tail the logs}
    cmds: ['echo RAN > ran.txt']
  bare:
    desc: No parameters
    cmds: ['echo RAN > ran.txt']
`,
	})

	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag+" does not run the task", func(t *testing.T) {
			got := runMain(t, root, "up", flag)
			checkCode(t, got, 0)
			if _, err := os.Stat(filepath.Join(root, "ran.txt")); err == nil {
				t.Fatal("the task RAN; --help must never execute anything")
			}
			checkContains(t, got, "stdout", got.stdout, "Bring up an instance")
			checkContains(t, got, "stdout", got.stdout, "which instance")
			checkContains(t, got, "stdout", got.stdout, "tail the logs")
			// bool is shown as such, and a default in the FILE's vars counts:
			// reading only the task's own vars called config required.
			checkContains(t, got, "stdout", got.stdout, "bool")
			checkContains(t, got, "stdout", got.stdout, "optional")
		})
	}

	t.Run("a task with no parameters says so", func(t *testing.T) {
		got := runMain(t, root, "bare", "--help")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "takes no arguments")
	})

	// After `--`, the words belong to the task: a command that genuinely needs to
	// pass --help through must still be able to.
	t.Run("after -- it is the task's data", func(t *testing.T) {
		root2 := writeTree(t, map[string]string{
			"chores.yml": `version: '3'
tasks:
  show:
    cmds: ['echo "[{{.CLI_ARGS}}]" > out.txt']
`,
		})
		got := runMain(t, root2, "show", "--", "--help")
		checkCode(t, got, 0)
		b, err := os.ReadFile(filepath.Join(root2, "out.txt"))
		if err != nil {
			t.Fatalf("the task did not run: %v", err)
		}
		if strings.TrimSpace(string(b)) != "[--help]" {
			t.Errorf("CLI_ARGS = %s, want [--help]", strings.TrimSpace(string(b)))
		}
	})
}

// ─── unknown flags after the task name ──────────────────────────────────────

// TestUnknownFlagAfterTheTaskNameIsAUsageError: the mirror of
// TestUnknownFlagIsAUsageError, on the other side of the task name.
//
// `chore --forse build` is already a usage error. `chore build --forse` is not:
// the word falls through splitArgs' declared-parameter lookup and is appended
// as a POSITIONAL, so it binds to whatever the task declares first. When that
// first parameter is a bool, NormalizeBool turns the flag's own text into
// "true" — anything outside {"", "0", "false", "no", "off"} is true — and a
// typo silently sets an unrelated flag.
//
// Measured against a real Taskfile driving a trading platform: `chore tick
// --total-nonsense` rendered `main.py tick --dry-run`, and `chore backtest
// --robot-name x` set BOTH holdout and force, where holdout spends a one-shot
// resource. A task runner that reinterprets a mistyped flag as a different one
// cannot safely drive anything that spends money.
//
// The refusal is at BINDING, not in splitArgs: an undeclared long flag still
// falls through as a positional word (TestSplitArgsNamedParameters pins that,
// and `chore logs -f api` depends on the same rule), and is refused only when
// it would become the value of a declared parameter. So the exit code is 1, a
// task-level error like any other bad argument — 2 stays chore's own
// flag-parsing code, as `chore --forse build` uses.
func TestUnknownFlagAfterTheTaskNameIsAUsageError(t *testing.T) {
	root := writeTree(t, map[string]string{
		"chores.yml": "version: '3'\n" +
			"tasks:\n" +
			"  deploy:\n" +
			"    args:\n" +
			"      - {name: live, type: bool}\n" +
			"    vars:\n" +
			"      live: false\n" +
			"    cmds: ['echo live=[{{.LIVE}}]']\n",
	})
	got := runMain(t, root, "--dry", "deploy", "--not-a-flag")

	checkCode(t, got, 1)
	checkContains(t, got, "stderr", got.stderr, `--not-a-flag`)
	// and it must say what the task DOES take, so the typo is fixable
	checkContains(t, got, "stderr", got.stderr, `--live`)

	t.Run("the suggestion is spelled the way a caller would type it", func(t *testing.T) {
		// The declaration is `dry_run` because it has to be usable as
		// {{.DRY_RUN}}, but --dry-run is what anyone types, and since both now
		// reach it, echoing the underscore teaches the wrong spelling.
		root := writeTree(t, map[string]string{
			"chores.yml": "version: '3'\n" +
				"tasks:\n" +
				"  tick:\n" +
				"    args:\n" +
				"      - {name: dry_run, type: bool}\n" +
				"    cmds: ['echo dry=[{{.DRY_RUN}}]']\n",
		})
		got := runMain(t, root, "--dry", "tick", "--total-nonsense")
		checkCode(t, got, 1)
		checkContains(t, got, "stderr", got.stderr, "--dry-run")
		checkNotContains(t, got, "stderr", got.stderr, "--dry_run")
	})
	// and it must NOT have quietly become the value of `live`
	checkNotContains(t, got, "stdout", got.stdout, "live=[true]")
}

// A declared name is always underscored (the loader requires a usable variable
// name), so the hyphenated spelling a human types has to be folded onto it, in
// any case. The underscore spelling keeps working — Taskfiles and scripts are
// written with it.
func TestSplitArgsFoldsHyphensOntoDeclaredUnderscores(t *testing.T) {
	params := map[string]param{"train_bars": {name: "train_bars"}}

	for _, spelling := range []string{"--train-bars", "--train_bars", "--TRAIN-BARS", "--Train-Bars"} {
		t.Run(spelling, func(t *testing.T) {
			args, vars, err := splitArgs([]string{spelling, "504"}, params)
			if err != nil {
				t.Fatalf("splitArgs(%q): %v", spelling, err)
			}
			if len(args) != 0 {
				t.Errorf("args = %q, want none: the flag was not recognised", args)
			}
			want := map[string]string{"train_bars": "504", "TRAIN_BARS": "504"}
			if !reflect.DeepEqual(vars, want) {
				t.Errorf("vars = %v, want %v", vars, want)
			}
		})
	}
}

// TestChoreMinVersion: a file states the oldest chore that may run it.
//
// The need is not hypothetical. A Taskfile driving money declared every
// dangerous flag as a string compared to "true" for one reason only — chore
// < 0.4.0 bound an unknown --flag positionally and let a bool take any value, so
// `--robot-name x` set an unrelated flag and spent a one-shot resource. Stating
// the floor is what lets the file drop the workaround instead of carrying it
// forever.
func TestChoreMinVersion(t *testing.T) {
	project := func(floor string) *chorefile.Project {
		return &chorefile.Project{
			Root:  &chorefile.File{ChoreMinVersion: floor, Path: "/w/chores.yml"},
			Tasks: map[string]*chorefile.Task{},
		}
	}

	for _, c := range []struct {
		name    string
		floor   string
		running buildinfo.Info
		wantErr string
	}{
		{"older is refused", "0.4.0", buildinfo.Info{Version: "0.3.0"}, "too old"},
		{"much older is refused", "0.4.0", buildinfo.Info{Version: "0.2.2"}, "too old"},
		{"the exact version satisfies it", "0.4.0", buildinfo.Info{Version: "0.4.0"}, ""},
		{"a newer patch satisfies it", "0.4.0", buildinfo.Info{Version: "0.4.1"}, ""},
		// 0.10.0 sorts BEFORE 0.4.0 as a string; numerically it is newer
		{"a two-digit minor is newer, not older", "0.4.0", buildinfo.Info{Version: "0.10.0"}, ""},
		{"a new major satisfies it", "0.4.0", buildinfo.Info{Version: "1.0.0"}, ""},
		{"no floor means no restriction", "", buildinfo.Info{Version: "0.1.0"}, ""},
		// a dev build has no version to judge, and already banners itself
		{"a dev build is exempt", "0.4.0", buildinfo.Info{Version: "dev+abc1234", Dev: true}, ""},
		{"an unjudgeable non-dev build is refused", "0.4.0", buildinfo.Info{Version: "?"}, "refusing rather than assuming"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := checkChoreVersion(project(c.floor), c.running)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("checkChoreVersion = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkChoreVersion = nil, want an error containing %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, c.wantErr)
			}
			// it must name both versions and the file, or it is not actionable
			for _, want := range []string{c.running.Version, c.floor, "/w/chores.yml"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to name %q", err, want)
				}
			}
		})
	}

	t.Run("the strictest floor across included files wins", func(t *testing.T) {
		// A floor belongs to the file that needs it, but the tasks it contributes
		// are still run by this one binary.
		p := project("0.4.0")
		p.Tasks["fromInclude"] = &chorefile.Task{
			Name: "fromInclude",
			File: &chorefile.File{ChoreMinVersion: "0.9.0", Path: "/w/inc/chores.yml"},
		}
		err := checkChoreVersion(p, buildinfo.Info{Version: "0.5.0"})
		if err == nil {
			t.Fatal("a 0.9.0 floor in an included file was ignored")
		}
		if !strings.Contains(err.Error(), "0.9.0") || !strings.Contains(err.Error(), "/w/inc/chores.yml") {
			t.Errorf("error = %q, want it to name the include's floor and file", err)
		}
	})

	t.Run("end to end, and inspection still works", func(t *testing.T) {
		// Proves the wiring, which a direct call cannot: that Main consults the
		// floor before running, and does not block reading the file.
		root := writeTree(t, map[string]string{
			"chores.yml": "version: '3'\n" +
				"chore_min_version: 0.4.0\n" +
				"tasks:\n  t:\n    cmds: ['echo ran']\n",
		})
		saved := Version
		Version = "0.3.0"
		defer func() { Version = saved }()

		got := runMain(t, root, "--dry", "t")
		checkCode(t, got, 1)
		checkContains(t, got, "stderr", got.stderr, "too old", "0.3.0", "0.4.0")
		checkNotContains(t, got, "stdout", got.stdout, "echo ran")

		// --list is inspection: someone staring at that refusal has to be able to
		// read what the file contains.
		got = runMain(t, root, "--list")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "t")
	})
}

// {{.CHORE_EXE}} is the binary actually running, not whatever PATH answers to
// "chore". A launchd plist needs an absolute ProgramArguments path, and baking in
// the installed copy while a different binary is running the task is how a
// scheduled job silently drifts from the one being tested.
func TestChoreExeAndVersionAreAvailableToTasks(t *testing.T) {
	root := writeTree(t, map[string]string{
		"chores.yml": "version: '3'\ntasks:\n  who:\n    cmds: ['echo exe=[{{.CHORE_EXE}}] v=[{{.CHORE_VERSION}}]']\n",
	})
	got := runMain(t, root, "--dry", "who")
	checkCode(t, got, 0)

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	checkContains(t, got, "stdout", got.stdout, "exe=["+exe+"]")
	// never empty: an empty {{.CHORE_EXE}} would silently run the next word
	checkNotContains(t, got, "stdout", got.stdout, "exe=[]")
}

// TestShortFlags: `-f` meaning `--force`, opt-in per parameter with `short:`.
//
// Opt-in and not derived from the name, because a single-dash word is otherwise
// DATA — `chore logs -f api` passes -f to the task — so deriving shorts would
// silently change what every existing file does, and two parameters starting
// with the same letter would have no answer at all.
func TestShortFlags(t *testing.T) {
	root := writeTree(t, map[string]string{
		"chores.yml": "version: '3'\n" +
			"tasks:\n" +
			"  logs:\n" +
			"    args:\n" +
			"      - {name: service, short: s}\n" +
			"      - {name: follow, short: f, type: bool}\n" +
			"      - {name: all, short: a, type: bool}\n" +
			"      - {name: brief, short: b, type: bool}\n" +
			"    vars: {service: \"\", follow: false, all: false, brief: false}\n" +
			"    cmds: ['echo svc=[{{.SERVICE}}] f=[{{.FOLLOW}}] a=[{{.ALL}}] b=[{{.BRIEF}}]']\n" +
			"  plain:\n" +
			"    args: [follow, service]\n" +
			"    cmds: ['echo docker logs {{.FOLLOW}} {{.SERVICE}}']\n",
	})

	for _, c := range []struct {
		name  string
		words []string
		want  string
	}{
		{"a bool short is its own value", []string{"-f"}, "svc=[] f=[true] a=[] b=[]"},
		{"a value short takes the next word", []string{"-s", "api"}, "svc=[api] f=[] a=[] b=[]"},
		{"or an attached value", []string{"-s=api"}, "svc=[api] f=[] a=[] b=[]"},
		{"several, separately", []string{"-f", "-a", "-b"}, "svc=[] f=[true] a=[true] b=[true]"},
		{"bundled, in any order", []string{"-bfa"}, "svc=[] f=[true] a=[true] b=[true]"},
		{"mixed with a value short", []string{"-fa", "-s", "api"}, "svc=[api] f=[true] a=[true] b=[]"},
		{"mixed with the long form", []string{"-f", "--all"}, "svc=[] f=[true] a=[true] b=[]"},
		{"a bool short does not eat the next word", []string{"-f", "api"}, "svc=[api] f=[true] a=[] b=[]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := runMain(t, root, append([]string{"--dry", "logs"}, c.words...)...)
			checkCode(t, got, 0)
			checkContains(t, got, "stdout", got.stdout, c.want)
		})
	}

	t.Run("a bundle holding a value parameter is refused, not guessed", func(t *testing.T) {
		// -sfa could be `-s -f -a` or an -s whose value is "fa". Guessing either
		// way is how a flag ends up set to a filename.
		got := runMain(t, root, "--dry", "logs", "-sfa")
		checkCode(t, got, 2)
		checkContains(t, got, "stderr", got.stderr, "-s takes a value", "cannot be bundled")
	})

	t.Run("a value short with nothing after it", func(t *testing.T) {
		got := runMain(t, root, "--dry", "logs", "-s")
		checkCode(t, got, 2)
		checkContains(t, got, "stderr", got.stderr, "-s needs a value")
	})

	t.Run("an explicit non-boolean is refused here too", func(t *testing.T) {
		got := runMain(t, root, "--dry", "logs", "-f=maybe")
		checkCode(t, got, 2)
		checkContains(t, got, "stderr", got.stderr, "-f must be true or false")
	})

	t.Run("an undeclared letter is named against the ones that exist", func(t *testing.T) {
		got := runMain(t, root, "--dry", "logs", "-z")
		checkCode(t, got, 1)
		checkContains(t, got, "stderr", got.stderr, "-z is not one of its short flags", "-s, -f, -a, -b")
	})

	t.Run("a file that declares no shorts is untouched", func(t *testing.T) {
		// The documented reason single-dash words are data in the first place.
		got := runMain(t, root, "--dry", "plain", "-f", "api")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "docker logs -f api")
	})

	t.Run("help shows the short, and a bool takes no value", func(t *testing.T) {
		got := runMain(t, root, "logs", "--help")
		checkCode(t, got, 0)
		checkContains(t, got, "stdout", got.stdout, "service (-s)", "follow (-f)", "chore logs -s <value>")
		// the flag form for a bool must not suggest a value, which is now an error
		checkNotContains(t, got, "stdout", got.stdout, "--follow <value>")
	})
}

// TestABoolParameterOnlyTakesABoolean: `type: bool` was the one declared type
// nothing validated. checkArgType rejects a non-numeric int, but returned nil
// for a bool, and NormalizeBool reads everything outside
// {"", "0", "false", "no", "off"} as true — so ANY word bound to a bool set it.
//
// That is what made single-dash flags look like they worked. chore has no short
// flag syntax, so `-f` is data, and data binds by POSITION: given
// `args: [f, a, b, c]` all bool, `chore t -f -a -b -c` set all four, and so did
// `-c -b -a -f`, and `-c` alone set f. The letters were never read — coercion
// hid it by making the answer true either way.
func TestABoolParameterOnlyTakesABoolean(t *testing.T) {
	root := writeTree(t, map[string]string{
		"chores.yml": "version: '3'\n" +
			"tasks:\n" +
			"  deploy:\n" +
			"    args:\n" +
			"      - {name: live, type: bool}\n" +
			"    vars:\n" +
			"      live: false\n" +
			"    cmds: ['echo live=[{{.LIVE}}]']\n",
	})

	t.Run("a word that is not a boolean is refused, not coerced", func(t *testing.T) {
		for _, value := range []string{"typo", "maybe", "-x", "-f"} {
			got := runMain(t, root, "--dry", "deploy", value)
			checkCode(t, got, 1)
			checkContains(t, got, "stderr", got.stderr, "must be true or false", value)
			// naming the flag spelling is the whole point: it is what to type instead
			checkContains(t, got, "stderr", got.stderr, "--live")
			checkNotContains(t, got, "stdout", got.stdout, "live=[true]")
		}
	})

	t.Run("a single-dash value says why the letter did not matter", func(t *testing.T) {
		got := runMain(t, root, "--dry", "deploy", "-x")
		checkContains(t, got, "stderr", got.stderr, "binds by position, not by letter")

		// and a plain word does not get that explanation, which would be noise
		got = runMain(t, root, "--dry", "deploy", "typo")
		checkNotContains(t, got, "stderr", got.stderr, "binds by position")
	})

	t.Run("an explicit non-boolean value is refused too", func(t *testing.T) {
		// Both of these normalise inside splitArgs, so they are checked there —
		// the only point the text still exists — and are usage errors, exit 2.
		for _, words := range [][]string{{"--live=maybe"}, {"LIVE=maybe"}, {"--live=nonsense"}} {
			got := runMain(t, root, append([]string{"--dry", "deploy"}, words...)...)
			checkCode(t, got, 2)
			checkContains(t, got, "stderr", got.stderr, "must be true or false")
			checkNotContains(t, got, "stdout", got.stdout, "live=[true]")
		}
	})

	t.Run("every spelling of an actual boolean still works", func(t *testing.T) {
		for _, c := range []struct {
			words []string
			want  string
		}{
			{[]string{"--live"}, "live=[true]"},
			{[]string{"--live=yes"}, "live=[true]"},
			{[]string{"--live=1"}, "live=[true]"},
			{[]string{"--live=on"}, "live=[true]"},
			{[]string{"true"}, "live=[true]"},
			{[]string{"--live=false"}, "live=[]"},
			{[]string{"--live=off"}, "live=[]"},
			{[]string{"false"}, "live=[]"},
			{[]string{"0"}, "live=[]"},
			{[]string{"no"}, "live=[]"},
			{[]string{}, "live=[]"},
		} {
			got := runMain(t, root, append([]string{"--dry", "deploy"}, c.words...)...)
			checkCode(t, got, 0)
			checkContains(t, got, "stdout", got.stdout, c.want)
		}
	})
}

// A parameter with no declared type takes a single-dash word as DATA, which is
// the documented reason `chore logs -f api` works, and validating bools must not
// disturb it. An int keeps its own check, negative numbers included.
func TestSingleDashDataIsUnaffectedByBoolValidation(t *testing.T) {
	root := writeTree(t, map[string]string{
		"chores.yml": "version: '3'\n" +
			"tasks:\n" +
			"  logs:\n" +
			"    args: [follow, service]\n" +
			"    cmds: ['echo docker logs {{.FOLLOW}} {{.SERVICE}}']\n" +
			"  scale:\n" +
			"    args:\n" +
			"      - {name: n, type: int}\n" +
			"    cmds: ['echo n=[{{.N}}]']\n",
	})

	got := runMain(t, root, "--dry", "logs", "-f", "api")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "docker logs -f api")

	got = runMain(t, root, "--dry", "scale", "-5")
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "n=[-5]")

	got = runMain(t, root, "--dry", "scale", "-x")
	checkCode(t, got, 1)
	checkContains(t, got, "stderr", got.stderr, "must be a whole number")
}

// TestHyphenatedSpellingOfAMultiWordParameter: `args: [{name: train_bars}]`
// declares a parameter no hyphenated flag can reach, because the lookup is
// strings.ToLower(name) with no dash-to-underscore mapping. A human types
// --train-bars; chore matches only --train_bars.
//
// The failure is not a clean rejection. For an int the value is at least
// type-checked ("must be a whole number, got \"--train-bars\""), but for a
// string or bool it binds silently.
func TestHyphenatedSpellingOfAMultiWordParameter(t *testing.T) {
	root := writeTree(t, map[string]string{
		"chores.yml": "version: '3'\n" +
			"tasks:\n" +
			"  probe:\n" +
			"    args:\n" +
			"      - {name: train_bars, type: int}\n" +
			"    vars:\n" +
			"      train_bars: 250\n" +
			"    cmds: ['echo bars=[{{.TRAIN_BARS}}]']\n",
	})
	got := runMain(t, root, "--dry", "probe", "--train-bars", "504")

	// Either spelling reaching the parameter is fine; silently doing something
	// else is not.
	checkCode(t, got, 0)
	checkContains(t, got, "stdout", got.stdout, "bars=[504]")
}
