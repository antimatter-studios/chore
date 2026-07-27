// Package taskfile is the schema: the Go shape of a Taskfile.yml, and nothing
// else. It has no dependencies on the rest of the program, so every other
// package can import it.
//
// The schema covers the features rest-mail's Taskfiles actually use, plus the
// ones that cost almost nothing to support. Everything else Task grew —
// remote includes, watch mode, v2 compatibility, matrix/for expansion,
// interactive tasks, output styles — is deliberately absent.
package taskfile

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
	Version  string              `yaml:"version"`
	Silent   bool                `yaml:"silent"`
	Dotenv   []string            `yaml:"dotenv"`
	Includes map[string]*Include `yaml:"includes"`
	Vars     map[string]Var      `yaml:"vars"`
	Env      map[string]Var      `yaml:"env"`
	Tasks    map[string]*Task    `yaml:"tasks"`

	// Set by the loader, not the YAML.
	Path string `yaml:"-"` // absolute path to this file
	Dir  string `yaml:"-"` // directory holding this file
}

// Include pulls another Taskfile into a namespace.
//
// Unlike Task, an included file sees ONLY the vars mapped to it here. Task
// flattens included vars into the parent's namespace, which is why two included
// files can silently overwrite each other's variables — the reason rest-mail
// cannot include reference-mailserver at all.
type Include struct {
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
	// per invocation of tsk, keyed on its rendered variables.
	Run         string   `yaml:"run"`
	IgnoreError bool     `yaml:"ignore_error"`
	Platforms   []string `yaml:"platforms"`
	Requires    []string `yaml:"requires"`

	Vars map[string]Var `yaml:"vars"`
	Env  map[string]Var `yaml:"env"`

	// Args names the task's positional parameters, in order:
	//
	//	up:
	//	  args: [config]
	//
	// invoked as `tsk up mail4.test`. This is the whole point of the program:
	// Task has no equivalent, so a config could only be selected by an
	// environment variable set before the command, and `task up CONFIG=x`
	// silently acted on something else.
	//
	// A Taskfile that does not use `args:` remains valid input to Task, so both
	// programs can be run against the same file and diffed.
	Args []string `yaml:"args"`

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

// Param is a declared parameter: its name, and whether the caller must supply it
// even when a default exists.
type Param struct {
	Name     string
	Required bool
}

// Params parses the `args:` list.
//
// A trailing "!" marks a parameter the caller must supply on the command line,
// defaults notwithstanding: `args: [config!]`. The marker is a SUFFIX because a
// leading "!" is YAML tag syntax — `args: [!config]` fails to parse, and
// `- !config` in block style is worse: it parses as a tag with null content and
// silently yields a parameter named "".
//
// Without the marker a parameter is required only when nothing defines it: a
// default in `vars:` makes it optional, and an explicitly empty default
// (`filter: ""`) is how an author says "may be omitted".
func (t *Task) Params() []Param {
	out := make([]Param, 0, len(t.Args))
	for _, a := range t.Args {
		if name, found := strings.CutSuffix(a, "!"); found {
			out = append(out, Param{Name: name, Required: true})
			continue
		}
		out = append(out, Param{Name: a})
	}
	return out
}

// RunOnce reports whether the task should execute at most once per invocation.
func (t *Task) RunOnce() bool { return t.Run == "once" }
