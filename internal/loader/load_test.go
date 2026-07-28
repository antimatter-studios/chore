package loader

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// writeTree materialises a Taskfile tree under a fresh temp dir. Keys are slash
// separated paths relative to that dir; intermediate directories are created.
// Every test builds its own tree on disk so nothing depends on the process
// working directory or on the repository's own Taskfiles.
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

func taskNames(p *chorefile.Project) []string {
	return slices.Sorted(maps.Keys(p.Tasks))
}

// TestLoadTrees covers include resolution end to end: real YAML on disk, read
// through chorefile.Decode, one case per rule in the spec.
func TestLoadTrees(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		want    []string // every key expected in Project.Tasks, sorted
		wantErr []string // substrings every one of which must appear in the error
	}{
		{
			name: "single file has bare names",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
tasks:
  build:
    desc: build it
    cmds:
      - echo build
  test:
    cmds:
      - echo test
`,
			},
			want: []string{"build", "test"},
		},
		{
			name: "include namespaces its tasks",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
includes:
  postgres:
    taskfile: postgres/Taskfile.yml
tasks:
  build:
    cmds: [echo build]
`,
				"postgres/Taskfile.yml": `version: '3'
tasks:
  up:
    cmds: [echo up]
  down:
    cmds: [echo down]
`,
			},
			want: []string{"build", "postgres:down", "postgres:up"},
		},
		{
			name: "nested includes chain their namespaces",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
includes:
  a:
    taskfile: a/Taskfile.yml
`,
				"a/Taskfile.yml": `version: '3'
includes:
  b:
    taskfile: b/Taskfile.yml
tasks:
  own:
    cmds: [echo a-own]
`,
				"a/b/Taskfile.yml": `version: '3'
tasks:
  task:
    cmds: [echo deep]
`,
			},
			want: []string{"a:b:task", "a:own"},
		},
		{
			name: "include path is relative to the including file",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
includes:
  stack:
    taskfile: stack/Taskfile.yml
`,
				// stack/ includes "db/Taskfile.yml", which must resolve to
				// stack/db, never to the root-level decoy below.
				"stack/Taskfile.yml": `version: '3'
includes:
  db:
    taskfile: db/Taskfile.yml
`,
				"stack/db/Taskfile.yml": `version: '3'
tasks:
  right:
    cmds: [echo right]
`,
				"db/Taskfile.yml": `version: '3'
tasks:
  decoy:
    cmds: [echo wrong]
`,
			},
			want: []string{"stack:db:right"},
		},
		{
			name: "include naming a directory finds Taskfile.yml inside it",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
includes:
  pg:
    taskfile: postgres
`,
				"postgres/Taskfile.yml": `version: '3'
tasks:
  up:
    cmds: [echo up]
`,
			},
			want: []string{"pg:up"},
		},
		{
			name: "optional missing include is skipped",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
includes:
  gone:
    taskfile: nowhere/Taskfile.yml
    optional: true
tasks:
  build:
    cmds: [echo build]
`,
			},
			want: []string{"build"},
		},
		{
			name: "missing include is an error naming it and the path",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
includes:
  gone:
    taskfile: nowhere/Taskfile.yml
tasks:
  build:
    cmds: [echo build]
`,
			},
			wantErr: []string{`include "gone"`, filepath.FromSlash("nowhere/Taskfile.yml")},
		},
		{
			name: "flatten keeps bare names",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
includes:
  extra:
    taskfile: extra/Taskfile.yml
    flatten: true
tasks:
  build:
    cmds: [echo build]
`,
				"extra/Taskfile.yml": `version: '3'
tasks:
  lint:
    cmds: [echo lint]
`,
			},
			want: []string{"build", "lint"},
		},
		{
			name: "flatten collision is an error naming both files",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
includes:
  extra:
    taskfile: extra/Taskfile.yml
    flatten: true
tasks:
  build:
    cmds: [echo build]
`,
				"extra/Taskfile.yml": `version: '3'
tasks:
  build:
    cmds: [echo other build]
`,
			},
			wantErr: []string{
				`duplicate task "build"`,
				filepath.FromSlash("extra/Taskfile.yml"),
				"Taskfile.yml and in",
			},
		},
		{
			name: "flatten of a nested include drops only its own segment",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
includes:
  a:
    taskfile: a/Taskfile.yml
`,
				"a/Taskfile.yml": `version: '3'
includes:
  hidden:
    taskfile: hidden/Taskfile.yml
    flatten: true
`,
				"a/hidden/Taskfile.yml": `version: '3'
tasks:
  task:
    cmds: [echo flat]
`,
			},
			want: []string{"a:task"},
		},
		{
			name: "cycle is reported, not followed",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
includes:
  a:
    taskfile: a/Taskfile.yml
`,
				"a/Taskfile.yml": `version: '3'
includes:
  back:
    taskfile: ../Taskfile.yml
tasks:
  own:
    cmds: [echo a]
`,
			},
			wantErr: []string{"include cycle", filepath.FromSlash("a/Taskfile.yml"), " -> "},
		},
		{
			name: "aliases register extra keys, namespaced with the include",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
includes:
  pg:
    taskfile: pg/Taskfile.yml
tasks:
  build:
    aliases: [b, compile]
    cmds: [echo build]
`,
				"pg/Taskfile.yml": `version: '3'
tasks:
  up:
    aliases: [start]
    cmds: [echo up]
`,
			},
			want: []string{"b", "build", "compile", "pg:start", "pg:up"},
		},
		{
			name: "alias colliding with a real task is an error",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
tasks:
  build:
    aliases: [test]
    cmds: [echo build]
  test:
    cmds: [echo test]
`,
			},
			wantErr: []string{`alias "test"`, `task "build"`, `task "test"`},
		},
		{
			name: "an include with neither taskfile nor dir is an error",
			files: map[string]string{
				"Taskfile.yml": `version: '3'
includes:
  broken:
    optional: true
`,
			},
			wantErr: []string{`include "broken"`, "neither taskfile nor dir"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, tc.files)
			p, err := Load(filepath.Join(root, "Taskfile.yml"))

			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("want error, got project with tasks %v", taskNames(p))
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if got := taskNames(p); !slices.Equal(got, tc.want) {
				t.Errorf("tasks = %v, want %v", got, tc.want)
			}
			if p.RootDir != root {
				t.Errorf("RootDir = %q, want %q", p.RootDir, root)
			}
			if p.Root == nil || p.Root.Path != filepath.Join(root, "Taskfile.yml") {
				t.Fatalf("Root = %+v, want the root Taskfile", p.Root)
			}
			if p.Root.Dir != root {
				t.Errorf("Root.Dir = %q, want %q", p.Root.Dir, root)
			}
			for key, task := range p.Tasks {
				if task.File == nil {
					t.Errorf("task %q has no File", key)
					continue
				}
				if task.Name == "" {
					t.Errorf("task %q has no Name", key)
				}
				// Name is the canonical key; aliases point at the same task.
				if p.Tasks[task.Name] != task {
					t.Errorf("task %q: Name %q does not resolve back to it", key, task.Name)
				}
				if !filepath.IsAbs(task.File.Path) || !filepath.IsAbs(task.File.Dir) {
					t.Errorf("task %q: File.Path=%q File.Dir=%q, want absolute", key, task.File.Path, task.File.Dir)
				}
			}
		})
	}
}

// TestLoadIncludeVars pins the only variable flow between files: down, from the
// include into the included file, overriding that file's own defaults.
func TestLoadIncludeVars(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Taskfile.yml": `version: '3'
vars:
  ROOT_ONLY: root
  IMAGE: 'root:image'
includes:
  pg:
    taskfile: pg/Taskfile.yml
    vars:
      IMAGE: 'postgres:17'
      PORT: '5432'
tasks:
  build:
    cmds: [echo build]
`,
		"pg/Taskfile.yml": `version: '3'
vars:
  IMAGE: 'postgres:15'
  USER: postgres
tasks:
  up:
    cmds: [echo up]
`,
	})

	p, err := Load(filepath.Join(root, "Taskfile.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	up, ok := p.Tasks["pg:up"]
	if !ok {
		t.Fatalf("pg:up missing, have %v", taskNames(p))
	}
	// The two layers reach the runner SEPARATELY: the file keeps its own defaults,
	// and the include's mapping is recorded unresolved, to be rendered in the scope
	// of the file that wrote it. Which value wins is the runtime's business — see
	// TestIncludeMappingUsesTheParentScope in internal/run.
	want := map[string]string{
		"IMAGE": "postgres:15", // the included file's own default, left alone
		"USER":  "postgres",
	}
	got := map[string]string{}
	for k, v := range up.File.Vars {
		got[k] = v.Value
	}
	if len(got) != len(want) {
		t.Fatalf("included file vars = %v, want %v", got, want)
	}
	if v := up.File.IncludeVars["IMAGE"].Value; v != "postgres:17" {
		t.Errorf("IncludeVars[IMAGE] = %q, want the include's value", v)
	}
	if v := up.File.IncludeVars["PORT"].Value; v != "5432" {
		t.Errorf("IncludeVars[PORT] = %q, want the include-only var", v)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("included file var %s = %q, want %q", k, got[k], v)
		}
	}

	// Nothing bleeds upward or sideways: the parent keeps its own IMAGE and
	// never learns about PORT or USER.
	if v := p.Root.Vars["IMAGE"].Value; v != "root:image" {
		t.Errorf("root IMAGE = %q, want root:image (include vars must not leak up)", v)
	}
	for _, leaked := range []string{"PORT", "USER"} {
		if _, ok := p.Root.Vars[leaked]; ok {
			t.Errorf("root file gained %s from the include", leaked)
		}
	}
	if _, ok := up.File.Vars["ROOT_ONLY"]; ok {
		t.Error("included file gained ROOT_ONLY: parent vars must not be flattened in")
	}
}

// TestLoadIncludeDir checks that `dir:` survives on the included File, and that
// the file's physical location is still recoverable from Path — that difference
// is how the runner knows a working directory was mapped in.
func TestLoadIncludeDir(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Taskfile.yml": `version: '3'
includes:
  pg:
    taskfile: pg/Taskfile.yml
    dir: work/pg
  plain:
    taskfile: plain/Taskfile.yml
`,
		"pg/Taskfile.yml": `version: '3'
tasks:
  up:
    cmds: [echo up]
`,
		"plain/Taskfile.yml": `version: '3'
tasks:
  up:
    cmds: [echo up]
`,
		"work/pg/.keep":   "",
		"plain/sub/.keep": "",
	})

	p, err := Load(filepath.Join(root, "Taskfile.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	mapped := p.Tasks["pg:up"].File
	if want := filepath.Join(root, "work", "pg"); mapped.Dir != want {
		t.Errorf("mapped include Dir = %q, want %q", mapped.Dir, want)
	}
	if want := filepath.Join(root, "pg", "Taskfile.yml"); mapped.Path != want {
		t.Errorf("mapped include Path = %q, want %q", mapped.Path, want)
	}

	plain := p.Tasks["plain:up"].File
	if want := filepath.Join(root, "plain"); plain.Dir != want {
		t.Errorf("plain include Dir = %q, want the file's own directory %q", plain.Dir, want)
	}
	if plain.Dir != filepath.Dir(plain.Path) {
		t.Errorf("plain include Dir = %q, want it to equal filepath.Dir(Path) = %q", plain.Dir, filepath.Dir(plain.Path))
	}
}

// TestLoadIncludeDirDoesNotMoveIncludeResolution guards the interaction between
// the two: `dir:` changes where tasks run, never where nested includes are
// looked up.
func TestLoadIncludeDirDoesNotMoveIncludeResolution(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Taskfile.yml": `version: '3'
includes:
  a:
    taskfile: a/Taskfile.yml
    dir: elsewhere
`,
		"a/Taskfile.yml": `version: '3'
includes:
  b:
    taskfile: b/Taskfile.yml
`,
		"a/b/Taskfile.yml": `version: '3'
tasks:
  deep:
    cmds: [echo deep]
`,
		"elsewhere/.keep": "",
	})

	p, err := Load(filepath.Join(root, "Taskfile.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := p.Tasks["a:b:deep"]; !ok {
		t.Fatalf("a:b:deep missing, have %v", taskNames(p))
	}
}

// TestLoadDirectoryArgument — the CLI may hand Load a directory.
func TestLoadDirectoryArgument(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Taskfile.yml": `version: '3'
tasks:
  build:
    cmds: [echo build]
`,
	})
	p, err := Load(root)
	if err != nil {
		t.Fatalf("Load(dir): %v", err)
	}
	if p.RootDir != root {
		t.Errorf("RootDir = %q, want %q", p.RootDir, root)
	}
	if _, ok := p.Tasks["build"]; !ok {
		t.Errorf("tasks = %v, want build", taskNames(p))
	}
}

func TestLoadMissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "Taskfile.yml")
	if _, err := Load(missing); err == nil {
		t.Fatal("want error for a missing root Taskfile")
	} else if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name %q", err, missing)
	}
}

// TestLoadDeterministic — two loads of a colliding tree must produce the same
// error text, because includes and tasks are walked in sorted order.
func TestLoadDeterministic(t *testing.T) {
	files := map[string]string{
		"Taskfile.yml": `version: '3'
includes:
  one:
    taskfile: one/Taskfile.yml
    flatten: true
  two:
    taskfile: two/Taskfile.yml
    flatten: true
`,
		"one/Taskfile.yml": `version: '3'
tasks:
  up:
    cmds: [echo one]
  zz:
    cmds: [echo one-zz]
`,
		"two/Taskfile.yml": `version: '3'
tasks:
  up:
    cmds: [echo two]
`,
	}

	var first string
	for i := range 5 {
		root := writeTree(t, files)
		_, err := Load(filepath.Join(root, "Taskfile.yml"))
		if err == nil {
			t.Fatal("want a duplicate-task error")
		}
		// Temp dir differs per iteration; compare the shape after stripping it.
		got := strings.ReplaceAll(err.Error(), root, "$ROOT")
		if i == 0 {
			first = got
			if !strings.Contains(got, "one") || !strings.Contains(got, "two") {
				t.Fatalf("error %q should name both colliding files", got)
			}
			continue
		}
		if got != first {
			t.Fatalf("run %d error %q differs from %q", i, got, first)
		}
	}
}

// --- unit tests over hand-built Files, independent of the YAML layer ---

func TestRegister(t *testing.T) {
	fileAt := func(path string, tasks map[string]*chorefile.Task) *chorefile.File {
		return &chorefile.File{Path: path, Dir: filepath.Dir(path), Tasks: tasks}
	}

	cases := []struct {
		name    string
		entries []entry
		want    []string
		wantErr []string
	}{
		{
			name: "namespaces and aliases",
			entries: []entry{
				{prefix: "", file: fileAt("/p/Taskfile.yml", map[string]*chorefile.Task{
					"build": {Aliases: []string{"b"}},
				})},
				{prefix: "pg", file: fileAt("/p/pg/Taskfile.yml", map[string]*chorefile.Task{
					"up": {Aliases: []string{"start"}},
				})},
				{prefix: "a:b", file: fileAt("/p/a/b/Taskfile.yml", map[string]*chorefile.Task{
					"deep": {},
				})},
			},
			want: []string{"a:b:deep", "b", "build", "pg:start", "pg:up"},
		},
		{
			name: "an empty task body is a no-op task, not a panic",
			entries: []entry{
				{prefix: "", file: fileAt("/p/Taskfile.yml", map[string]*chorefile.Task{"build": nil})},
			},
			want: []string{"build"},
		},
		{
			name: "duplicate names name both files",
			entries: []entry{
				{prefix: "", file: fileAt("/p/Taskfile.yml", map[string]*chorefile.Task{"up": {}})},
				{prefix: "", file: fileAt("/p/extra/Taskfile.yml", map[string]*chorefile.Task{"up": {}})},
			},
			wantErr: []string{`duplicate task "up"`, "/p/Taskfile.yml", "/p/extra/Taskfile.yml"},
		},
		{
			name: "an alias cannot shadow a task declared in a later file",
			entries: []entry{
				{prefix: "", file: fileAt("/p/Taskfile.yml", map[string]*chorefile.Task{
					"build": {Aliases: []string{"lint"}},
				})},
				{prefix: "", file: fileAt("/p/extra/Taskfile.yml", map[string]*chorefile.Task{
					"lint": {},
				})},
			},
			wantErr: []string{`alias "lint"`, `task "build"`, `task "lint"`},
		},
		{
			name: "two tasks claiming the same alias collide",
			entries: []entry{
				{prefix: "", file: fileAt("/p/Taskfile.yml", map[string]*chorefile.Task{
					"build": {Aliases: []string{"x"}},
					"test":  {Aliases: []string{"x"}},
				})},
			},
			wantErr: []string{`alias "x"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tasks, err := register(tc.entries)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("want error, got %v", slices.Sorted(maps.Keys(tasks)))
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			if got := slices.Sorted(maps.Keys(tasks)); !slices.Equal(got, tc.want) {
				t.Errorf("keys = %v, want %v", got, tc.want)
			}
			for _, e := range tc.entries {
				for name, task := range e.file.Tasks {
					if want := qualify(e.prefix, name); task.Name != want {
						t.Errorf("task %s: Name = %q, want %q", name, task.Name, want)
					}
					if task.File != e.file {
						t.Errorf("task %s: File = %v, want %s", name, task.File, e.file.Path)
					}
				}
			}
		})
	}
}

// An include's vars are recorded, NOT merged into the file they are given to:
// they are written in the parent and mean the parent's variables, so the runtime
// resolves them there. Merging them here is what made '{{.POSTGRES_IP}}' inside an
// include render empty.
func TestIncludeVarsAreRecordedNotMerged(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"chores.yml": `version: "3"
vars:
  PARENT_VALUE: from-parent
  IMAGE: parent-image
includes:
  kid:
    taskfile: ./kid.yml
    vars:
      MAPPED: '{{.PARENT_VALUE}}'
      IMAGE: mapped-image
tasks:
  root: {cmds: ['true']}
`,
		"kid.yml": `version: "3"
vars:
  IMAGE: kid-default
tasks:
  show: {cmds: ['true']}
`,
	})

	p, err := Load(filepath.Join(dir, "chores.yml"))
	if err != nil {
		t.Fatal(err)
	}
	kid := p.Tasks["kid:show"].File

	if got := kid.IncludeVars["MAPPED"].Value; got != "{{.PARENT_VALUE}}" {
		t.Errorf("IncludeVars[MAPPED] = %q, want the UNRESOLVED template", got)
	}
	// Still a template here on purpose: resolving it needs the parent's scope,
	// which the loader does not have and must not guess at.
	if _, ok := kid.Vars["MAPPED"]; ok {
		t.Error("the include's vars were merged into the child's own")
	}
	if got := kid.Vars["IMAGE"].Value; got != "kid-default" {
		t.Errorf("child's own IMAGE = %q, want it left alone", got)
	}
	if kid.Parent == nil || kid.Parent.Path != filepath.Join(dir, "chores.yml") {
		t.Errorf("Parent = %v, want the including file", kid.Parent)
	}
	if p.Root.Parent != nil {
		t.Error("the root file has a parent")
	}
}

func TestQualify(t *testing.T) {
	cases := []struct{ prefix, name, want string }{
		{"", "up", "up"},
		{"pg", "up", "pg:up"},
		{"a:b", "up", "a:b:up"},
	}
	for _, tc := range cases {
		if got := qualify(tc.prefix, tc.name); got != tc.want {
			t.Errorf("qualify(%q, %q) = %q, want %q", tc.prefix, tc.name, got, tc.want)
		}
	}
}

// TestSameFileIncludedTwice — a diamond is not a cycle, and each include gets
// its own copy of the file so their vars cannot cross-contaminate.
func TestSameFileIncludedTwice(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Taskfile.yml": `version: '3'
includes:
  first:
    taskfile: shared/Taskfile.yml
    vars:
      NAME: one
  second:
    taskfile: shared/Taskfile.yml
    vars:
      NAME: two
`,
		"shared/Taskfile.yml": `version: '3'
vars:
  NAME: default
tasks:
  up:
    cmds: [echo up]
`,
	})

	p, err := Load(filepath.Join(root, "Taskfile.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := taskNames(p); !slices.Equal(got, []string{"first:up", "second:up"}) {
		t.Fatalf("tasks = %v", got)
	}
	// Each include keeps its OWN mapping: the same file included twice is two
	// independent copies, or one instance's name would leak into the other.
	if v := p.Tasks["first:up"].File.IncludeVars["NAME"].Value; v != "one" {
		t.Errorf("first NAME = %q, want one", v)
	}
	if v := p.Tasks["second:up"].File.IncludeVars["NAME"].Value; v != "two" {
		t.Errorf("second NAME = %q, want two", v)
	}
	if v := p.Tasks["first:up"].File.Vars["NAME"].Value; v != "default" {
		t.Errorf("the file's own NAME = %q, want its untouched default", v)
	}
	if p.Tasks["first:up"] == p.Tasks["second:up"] {
		t.Error("both includes share one task pointer; each include must load its own copy")
	}
}
