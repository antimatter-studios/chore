package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rest-mail/go-tsk/internal/taskfile"
)

// writeDotenv puts body in a file of its own so parseDotenv is exercised the
// only way it is ever used: against a real path it has to open itself.
func writeDotenv(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestParseDotenv pins the accepted grammar. It is deliberately a small subset —
// the files this loads are generated — and everything outside it is an error
// rather than a guess, because a silently mis-parsed value is how a stack ends
// up pointing at the wrong host.
func TestParseDotenv(t *testing.T) {
	cases := []struct {
		name string
		body string
		want map[string]string
	}{
		{
			name: "plain KEY=VALUE",
			body: "HOST=db.internal\nPORT=5432\n",
			want: map[string]string{"HOST": "db.internal", "PORT": "5432"},
		},
		{
			name: "comments and blank lines are skipped",
			body: "# the database\n\nHOST=db\n\n   # indented comment\nPORT=5432\n",
			want: map[string]string{"HOST": "db", "PORT": "5432"},
		},
		{
			name: "export prefix is dropped",
			body: "export HOST=db\n",
			want: map[string]string{"HOST": "db"},
		},
		{
			name: "only a real export prefix is dropped",
			// The prefix includes the space, so a name that merely starts with
			// "export" survives intact.
			body: "exported=yes\n",
			want: map[string]string{"exported": "yes"},
		},
		{
			name: "double quotes are stripped",
			body: `GREETING="hello world"` + "\n",
			want: map[string]string{"GREETING": "hello world"},
		},
		{
			name: "single quotes are stripped",
			body: "GREETING='hello world'\n",
			want: map[string]string{"GREETING": "hello world"},
		},
		{
			name: "a value may contain = signs",
			// Only the FIRST = separates; a DSN or a docker filter is full of them.
			body: "DSN=host=db port=5432 sslmode=disable\n",
			want: map[string]string{"DSN": "host=db port=5432 sslmode=disable"},
		},
		{
			name: "an empty value is a value",
			body: "OPTIONAL=\n",
			want: map[string]string{"OPTIONAL": ""},
		},
		{
			name: "surrounding whitespace is trimmed from both sides",
			body: "  HOST  =   db.internal   \n",
			want: map[string]string{"HOST": "db.internal"},
		},
		{
			name: "# only starts a comment at the start of a line",
			// Trailing-comment support would silently truncate any value with a
			// # in it, and generated files put fragments and colours in values.
			body: "TAG=v1 # not a comment\n",
			want: map[string]string{"TAG": "v1 # not a comment"},
		},
		{
			name: "escapes inside double quotes are interpreted",
			body: `COLUMNS="a\tb"` + "\n",
			want: map[string]string{"COLUMNS": "a\tb"},
		},
		{
			name: "an unparseable escape falls back to stripping the quotes",
			// Go's unquoting rejects \p, and refusing the whole file over it
			// would be worse than taking the text literally.
			body: `PATTERN="C:\path"` + "\n",
			want: map[string]string{"PATTERN": `C:\path`},
		},
		{
			name: "a single character in quotes is still that character",
			body: "SEP='|'\n",
			want: map[string]string{"SEP": "|"},
		},
		{
			name: "no trailing newline",
			body: "HOST=db",
			want: map[string]string{"HOST": "db"},
		},
		{
			name: "an empty file yields no variables",
			body: "",
			want: map[string]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDotenv(writeDotenv(t, tc.body))
			if err != nil {
				t.Fatalf("parseDotenv: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("%s = %q, want %q", k, got[k], want)
				}
			}
		})
	}
}

func TestParseDotenvErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string // substrings that must all appear
	}{
		{
			name: "empty variable name",
			body: "HOST=db\n=orphan\n",
			// The line number is part of the contract: a generated file can be
			// hundreds of lines long.
			want: []string{":2:", "empty variable name"},
		},
		{
			name: "whitespace-only variable name",
			body: "   =orphan\n",
			want: []string{":1:", "empty variable name"},
		},
		{
			name: "a line with no = at all",
			body: "HOST=db\nJUST_A_WORD\n",
			want: []string{":2:", "expected KEY=VALUE", "JUST_A_WORD"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeDotenv(t, tc.body)
			got, err := parseDotenv(path)
			if err == nil {
				t.Fatalf("parseDotenv returned %v, want an error", got)
			}
			for _, want := range tc.want {
				mustContain(t, err.Error(), want, "error")
			}
			mustContain(t, err.Error(), path, "error")
		})
	}
}

// TestParseDotenvMissingFile: the caller distinguishes "absent" from "malformed"
// with os.IsNotExist, so the error has to keep that distinction intact rather
// than wrapping it in something opaque.
func TestParseDotenvMissingFile(t *testing.T) {
	_, err := parseDotenv(filepath.Join(t.TempDir(), "nope.env"))
	if err == nil {
		t.Fatal("want an error for a missing file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("error %v is not recognisable as not-exist; the Runner needs that to tell absent from malformed", err)
	}
}

// ---------- Runner-level dotenv rules (SPEC "Fixed semantics" #4) ----------

// TestDotenvNoneLoadedIsFatal is the guarantee the rule exists for: a task never
// runs against a config with NO environment at all. That is how every value
// silently becomes a default, container names resolve to "-suffix", and commands
// match nothing while reporting success.
func TestDotenvNoneLoadedIsFatal(t *testing.T) {
	f := newFixture(t, &taskfile.File{
		Dotenv: []string{"config.env", "secrets.env"},
	}, map[string]*taskfile.Task{
		"up": {Cmds: cmds("printf ran > ran.txt")},
	})

	err := f.mustFail("up", nil, nil)

	mustContain(t, err.Error(), "no environment loaded", "error")
	// Both candidates are named, because the fix is to create one of them.
	mustContain(t, err.Error(), filepath.Join(f.dir, "config.env"), "error")
	mustContain(t, err.Error(), filepath.Join(f.dir, "secrets.env"), "error")
	if f.exists("ran.txt") {
		t.Error("the task ran with no environment at all")
	}
}

// TestDotenvPartialMissWarnsAndContinues: an absent secrets.env is normal, so a
// partial miss must not stop the run — but it must not be invisible either.
func TestDotenvPartialMissWarnsAndContinues(t *testing.T) {
	f := newFixture(t, &taskfile.File{
		Dotenv: []string{"config.env", "secrets.env"},
	}, map[string]*taskfile.Task{
		"up": {Cmds: cmds(`printf '%s' '{{.STACK}}' > stack.txt`)},
	})
	f.write("config.env", "STACK=alpha\n")

	f.mustRun("up", nil, nil)

	if got := f.read("stack.txt"); got != "alpha" {
		t.Errorf("stack.txt = %q, want alpha — the file that does exist must still load", got)
	}
	mustContain(t, f.err.String(), filepath.Join(f.dir, "secrets.env"), "stderr")
	mustContain(t, f.err.String(), "does not exist, continuing without it", "stderr")
	// The message has to say how to make it stop, or it becomes noise people
	// learn to scroll past.
	mustContain(t, f.err.String(), "prefix the path with ?", "stderr")
}

// TestDotenvWarnsOncePerRun: with 20 tasks sharing one config, repeating the
// same warning 20 times is how a real diagnostic gets ignored. Deps run
// concurrently, so this also exercises the lock around the warned set.
func TestDotenvWarnsOncePerRun(t *testing.T) {
	f := newFixture(t, &taskfile.File{
		Dotenv: []string{"config.env", "secrets.env"},
	}, map[string]*taskfile.Task{
		"top": {Deps: depsOn("a", "b"), Cmds: cmds("printf top > top.txt")},
		"a":   {Cmds: cmds("printf a > a.txt")},
		"b":   {Cmds: cmds("printf b > b.txt")},
	})
	f.write("config.env", "STACK=alpha\n")

	f.mustRun("top", nil, nil)

	// Four invocations resolve dotenv: top, its two deps, and top again is not
	// re-resolved — what matters is that three separate resolutions produce one
	// line.
	if n := strings.Count(f.err.String(), "does not exist, continuing without it"); n != 1 {
		t.Errorf("warned %d times, want 1:\n%s", n, f.err)
	}
}

// TestDotenvOptionalPrefixIsSilent: `?` is the documented way to say "this one
// is absent by design", and it must suppress the report without suppressing the
// load when the file does appear.
func TestDotenvOptionalPrefixIsSilent(t *testing.T) {
	t.Run("absent optional file is not reported", func(t *testing.T) {
		f := newFixture(t, &taskfile.File{
			Dotenv: []string{"config.env", "?secrets.env"},
		}, map[string]*taskfile.Task{
			"up": {Cmds: cmds(`printf '%s' '{{.STACK}}' > stack.txt`)},
		})
		f.write("config.env", "STACK=alpha\n")

		f.mustRun("up", nil, nil)

		if got := f.read("stack.txt"); got != "alpha" {
			t.Errorf("stack.txt = %q, want alpha", got)
		}
		if f.err.String() != "" {
			t.Errorf("stderr = %q, want nothing: ? silences the report", f.err)
		}
	})

	t.Run("present optional file still loads and outranks the earlier one", func(t *testing.T) {
		f := newFixture(t, &taskfile.File{
			Dotenv: []string{"config.env", "?secrets.env"},
		}, map[string]*taskfile.Task{
			"up": {Cmds: cmds(`printf '%s/%s' '{{.STACK}}' '{{.TOKEN}}' > out.txt`)},
		})
		f.write("config.env", "STACK=alpha\nTOKEN=placeholder\n")
		f.write("secrets.env", "TOKEN=real\n")

		f.mustRun("up", nil, nil)

		if got, want := f.read("out.txt"), "alpha/real"; got != want {
			t.Errorf("out.txt = %q, want %q", got, want)
		}
	})

	t.Run("all files optional and all absent is allowed", func(t *testing.T) {
		// `?` is the author declaring that running without this file is fine, so
		// it is exempt from the "a config with no environment" rule. If EVERY
		// declared path is optional, the task has been told it needs none of them
		// — treating that as fatal would contradict the annotation, and produced
		// an error message that named no files at all.
		f := newFixture(t, &taskfile.File{
			Dotenv: []string{"?config.env"},
		}, map[string]*taskfile.Task{
			"up": {Cmds: cmds("printf ran > ran.txt")},
		})

		f.mustRun("up", nil, nil)

		if !f.exists("ran.txt") {
			t.Error("the task did not run, but every dotenv path was marked optional")
		}
	})

	t.Run("a required path missing alongside an optional one is still fatal", func(t *testing.T) {
		// The rule still bites where it matters: one unmarked path that does not
		// exist means the environment the author DID require is absent.
		f := newFixture(t, &taskfile.File{
			Dotenv: []string{"config.env", "?secrets.env"},
		}, map[string]*taskfile.Task{
			"up": {Cmds: cmds("printf ran > ran.txt")},
		})

		err := f.mustFail("up", nil, nil)

		mustContain(t, err.Error(), "no environment loaded", "error")
		if f.exists("ran.txt") {
			t.Error("the task ran with no environment at all")
		}
	})
}

// TestDotenvAbsolutePath: a path that is already absolute is used as written,
// rather than being joined onto the taskfile's directory.
func TestDotenvAbsolutePath(t *testing.T) {
	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, "shared.env"), []byte("STACK=shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := newFixture(t, &taskfile.File{
		Dotenv: []string{filepath.Join(shared, "shared.env")},
	}, map[string]*taskfile.Task{
		"up": {Cmds: cmds(`printf '%s' '{{.STACK}}' > stack.txt`)},
	})

	f.mustRun("up", nil, nil)

	if got := f.read("stack.txt"); got != "shared" {
		t.Errorf("stack.txt = %q, want shared", got)
	}
}

// TestDotenvMalformedFileIsFatal: a file that exists but does not parse is a
// different failure from one that is absent, and it must not be treated as a
// silent miss — the values the author wrote are the ones the task needs.
func TestDotenvMalformedFileIsFatal(t *testing.T) {
	f := newFixture(t, &taskfile.File{
		Dotenv: []string{"config.env"},
	}, map[string]*taskfile.Task{
		"up": {Cmds: cmds("printf ran > ran.txt")},
	})
	f.write("config.env", "STACK=alpha\nthis line has no equals sign\n")

	err := f.mustFail("up", nil, nil)

	mustContain(t, err.Error(), "expected KEY=VALUE", "error")
	if f.exists("ran.txt") {
		t.Error("the task ran despite an unparseable env file")
	}
}
