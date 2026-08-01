// Package taskfile is the schema: the Go shape of a Taskfile.yml, and nothing
// else. It has no dependencies on the rest of the program, so every other
// package can import it.
//
// The schema covers the features rest-mail's Taskfiles actually use, plus the
// ones that cost almost nothing to support. Everything else Task grew —
// remote includes, watch mode, v2 compatibility, matrix/for expansion,
// interactive tasks, output styles — is deliberately absent.
package chorefile

import "strings"

// Project is a loaded Taskfile and everything it includes, with tasks
// flattened into one namespaced map (`postgres:up`, `instance:down`).
type Project struct {
	Root  *File
	Tasks map[string]*Task
	// RootDir is the directory of the root Taskfile — the value of {{.ROOT_DIR}},
	// and the working directory a task runs in unless it sets `dir:`.
	RootDir string
}

// File is one Taskfile on disk.
type File struct {
	Version   string              `yaml:"version"`
	Silent    bool                `yaml:"silent"`
	Dotenv    []string            `yaml:"dotenv"`
	Includes  map[string]*Include `yaml:"includes"`
	Vars      map[string]Var      `yaml:"vars"`
	Env       map[string]Var      `yaml:"env"`
	Tasks     map[string]*Task    `yaml:"tasks"`
	Lifecycle *Lifecycle          `yaml:"lifecycle"`

	// Set by the loader, not the YAML.
	Path string `yaml:"-"` // absolute path to this file
	Dir  string `yaml:"-"` // directory holding this file, or the include's dir:
	// WorkDir is the include's `dir:`, absolute, and "" when the include did not
	// declare one. Recorded separately because it is the ONE thing that moves a
	// task's working directory away from the project root, and inferring it from
	// `Dir != filepath.Dir(Path)` is ambiguous: an include whose dir: happens to
	// be the directory the file already lives in is indistinguishable from an
	// include with no dir: at all.
	WorkDir string `yaml:"-"`
	// Namespace is the include prefix this file's tasks are flattened under —
	// "monitoring", "a:b", or "" for the root file. Recorded because a task's own
	// NAME cannot reveal it: tasks/monitoring.yml defines a task literally called
	// "prometheus:up", so `monitoring:prometheus:up` has the namespace
	// "monitoring", not "monitoring:prometheus", and trimming at the last colon
	// gets it wrong.
	Namespace string `yaml:"-"`
	// Parent is the file that included this one, nil for the root. Kept so the
	// mapped vars below can be resolved where they were WRITTEN.
	Parent *File `yaml:"-"`
	// IncludeVars is the `vars:` block of the include that pulled this file in.
	//
	// Held separately from Vars, because the two are resolved in different scopes.
	// `vars: {IP: '{{.POSTGRES_IP}}'}` is written in the PARENT and means the
	// parent's POSTGRES_IP; resolving it here, where an include deliberately sees
	// nothing but what was mapped to it, yields "" — which is how a reference mail
	// server came up as "mailref-mail1-postgres @" with no address at all.
	IncludeVars map[string]Var `yaml:"-"`
	// Inherit is the include's `inherit:`. False by default: an included file sees
	// the outside world and what was MAPPED to it, and nothing else. Set true and
	// the including file's variables come with it, as a layer BELOW the file's own —
	// so a global config can be declared once at the root without every include
	// listing every name, and a file still wins on any name it defines itself.
	Inherit bool `yaml:"-"`
}

// Lifecycle declares hooks that run AROUND a whole `chore` invocation, once —
// not per task. This is chore's own extension (Task has no equivalent): it lets a
// project run setup and teardown for the run without wiring a dependency into
// every task by hand, and — unlike a `deps:` entry — it runs even when the task
// it wraps is up to date, because it is not that task's prerequisite.
//
// The canonical use is a self-installing guard: `before_all: [{task: hooks:ensure}]`
// activates a repo's git hooks the first time anyone runs any task, with no
// per-task boilerplate.
//
// Ordering, for the one task named on the command line:
//
//		before_all → <task> → after_all
//
//	  - before_all runs first, once. If it FAILS, the task does not run and neither
//	    does after_all — a setup gate that did not pass must not let work proceed.
//	  - after_all runs on the way out once the task has been entered, whether the
//	    task succeeded or not, so it can tear down what before_all set up.
//	  - on_error runs when before_all or the task returns non-zero.
//
// Hooks are skipped for `--list`, `--help` and `version` (which never run a task)
// and can be turned off for a run with `--no-lifecycle`. Each hook is a list of
// steps in the same shape as a task's `cmds:` — a shell line or `- task: name` —
// and `{{.TASK}}` in a hook renders the name of the invoked task.
type Lifecycle struct {
	BeforeAll Cmds `yaml:"before_all"`
	AfterAll  Cmds `yaml:"after_all"`
	OnError   Cmds `yaml:"on_error"`
}

// Include pulls another Taskfile into a namespace.
//
// Unlike Task, an included file sees ONLY the vars mapped to it here. Task
// flattens included vars into the parent's namespace, which is why two included
// files can silently overwrite each other's variables — the reason rest-mail
// cannot include reference-mailserver at all.
type Include struct {
	// Inherit brings the including file's variables into this one, as a layer below
	// its own. Off by default, because a file that silently sees everything above it
	// is the variable-bleed this format is known for.
	Inherit  bool           `yaml:"inherit"`
	Taskfile string         `yaml:"taskfile"`
	Dir      string         `yaml:"dir"`
	Optional bool           `yaml:"optional"`
	Flatten  bool           `yaml:"flatten"`
	Vars     map[string]Var `yaml:"vars"`
}

// Var is a variable value: either a literal or a shell command to capture.
//
// YAML accepts both forms:
//
//	FOO: bar
//	FOO: {sh: git rev-parse HEAD}
type Var struct {
	Value string
	Sh    string
}

// Task is one runnable unit.
type Task struct {
	Desc     string   `yaml:"desc"`
	Summary  string   `yaml:"summary"`
	Aliases  []string `yaml:"aliases"`
	Dir      string   `yaml:"dir"`
	Silent   bool     `yaml:"silent"`
	Internal bool     `yaml:"internal"`
	// Run is "always" (default) or "once": a task marked once executes one time
	// per invocation of chore, keyed on its rendered variables.
	Run         string   `yaml:"run"`
	IgnoreError bool     `yaml:"ignore_error"`
	Platforms   []string `yaml:"platforms"`
	Requires    []string `yaml:"requires"`

	Vars map[string]Var `yaml:"vars"`
	Env  map[string]Var `yaml:"env"`
	// Dotenv REPLACES the files its taskfile declares, and `dotenv: []` declines
	// them outright. A task that drives another project has no business loading
	// this one's environment: the root file requires a config's config.env, which
	// is right for every task that operates on a config and wrong for one whose
	// whole job is to hand off to a peer repository that owns its own.
	//
	// nil means "not declared" — inherit, which is what almost every task wants.
	Dotenv []string `yaml:"dotenv"`

	// Args declares the task's parameters, in positional order:
	//
	//	up:
	//	  args:
	//	    - config                 # shorthand for {name: config}
	//	    - name: follow
	//	      type: bool
	//	      desc: keep streaming
	//
	// invoked as `chore up mail4.test --follow`. This is the whole point of the
	// program: Task has no equivalent, so a config could only be selected by an
	// environment variable set before the command, and `task up CONFIG=x`
	// silently acted on something else.
	//
	// A Taskfile that does not use `args:` remains valid input to Task, so both
	// programs can be run against the same file and diffed.
	Args Args `yaml:"args"`

	Deps Deps `yaml:"deps"`
	Cmds Cmds `yaml:"cmds"`

	// Up-to-date checks. Status is a list of shell commands: all exiting zero
	// means "already done, skip". Sources/Generates compare content checksums.
	Status    []string `yaml:"status"`
	Sources   []string `yaml:"sources"`
	Generates []string `yaml:"generates"`

	// Set by the loader.
	Name string `yaml:"-"` // namespaced name, e.g. "postgres:up"
	File *File  `yaml:"-"` // the file this task came from
}

// Cmds and Deps are named slice types purely so they can reject a null element.
// yaml.v3 zero-fills a null into a struct slice entry BEFORE any element
// unmarshaler runs, so a stray "- " in a cmds: list silently disappears — a step
// that vanishes without a word is the failure mode this program is built to
// eliminate, so the list itself has to catch it.
type Cmds []Cmd

// Deps is the dependency list; see Cmds for why it is a named type.
type Deps []Dep

// Dep is a prerequisite. Dependencies of one task run concurrently.
type Dep struct {
	Task   string         `yaml:"task"`
	Vars   map[string]Var `yaml:"vars"`
	Silent bool           `yaml:"silent"`
}

// Cmd is one step of a task: either a shell command or a call to another task.
type Cmd struct {
	Cmd         string         `yaml:"cmd"`
	Task        string         `yaml:"task"`
	Vars        map[string]Var `yaml:"vars"`
	Silent      bool           `yaml:"silent"`
	IgnoreError bool           `yaml:"ignore_error"`
	// Defer marks a step written as `- defer: …`, which runs when the task
	// finishes — in reverse order, and whether or not the task succeeded. It is
	// how a task that brings a topology up guarantees it comes back down.
	Defer bool `yaml:"-"`
}

// Args is a task's parameter list.
type Args []Arg

// Arg is one declared parameter.
//
// Type is declared rather than inferred. Inferring "boolean" from a true/false
// default was tried first and is subtly wrong: a string parameter whose default
// happens to be "false" would silently become a flag, and a flag would then stop
// consuming its value. An explicit type also lets `--help` say what a task takes.
type Arg struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"` // "" or "string" (default), "bool", "int"
	Desc string `yaml:"desc"`
}

// IsBool reports whether the parameter is a flag: present or absent, no value.
func (a Arg) IsBool() bool { return a.Type == TypeBool }

// Parameter types.
const (
	TypeString = "string"
	TypeBool   = "bool"
	TypeInt    = "int"
)

// Names returns the parameter names in declared order.
func (as Args) Names() []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Name
	}
	return out
}

// Find returns the declaration for a name, matched case-insensitively because a
// parameter answers to both its declared spelling and its uppercase form.
func (as Args) Find(name string) (Arg, bool) {
	for _, a := range as {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Arg{}, false
}

// DefaultFor returns the literal default declared for a parameter, in any
// spelling, from the task or the file it lives in. A `sh:` var is not a default
// for this purpose: its value is not known until something runs it.
func (t *Task) DefaultFor(name string) (string, bool) {
	for _, spelling := range []string{name, strings.ToUpper(name), strings.ToLower(name)} {
		if v, ok := t.Vars[spelling]; ok && v.Sh == "" {
			return v.Value, true
		}
		if t.File != nil {
			if v, ok := t.File.Vars[spelling]; ok && v.Sh == "" {
				return v.Value, true
			}
		}
	}
	return "", false
}

// ParamIsBool reports whether a parameter is a flag rather than a value.
func (t *Task) ParamIsBool(name string) bool {
	a, ok := t.Args.Find(name)
	return ok && a.IsBool()
}

// NormalizeBool renders a boolean parameter the way both consumers of a variable
// expect: "true" when set, EMPTY when not.
//
// Empty rather than "false" because a Go template treats any non-empty string as
// true — `{{if .VERBOSE}}` would fire on the string "false" — and because the
// shell idiom is `[ -n "$VERBOSE" ]`. One value that reads correctly in both.
func NormalizeBool(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "false", "no", "off":
		return ""
	}
	return "true"
}

// RunOnce reports whether the task should execute at most once per invocation.
func (t *Task) RunOnce() bool { return t.Run == "once" }
