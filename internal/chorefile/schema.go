// Package taskfile is the schema: the Go shape of a Taskfile.yml, and nothing
// else. It has no dependencies on the rest of the program, so every other
// package can import it.
//
// The schema covers the features rest-mail's Taskfiles actually use, plus the
// ones that cost almost nothing to support. Everything else Task grew —
// remote includes, watch mode, v2 compatibility, matrix/for expansion,
// output styles — is deliberately absent. (Interactive tasks were, until a
// credential-rotation task needed a terminal; they are opt-in per task.)
package chorefile

import (
	"strconv"
	"strings"
)

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
	Version string `yaml:"version"`
	// ChoreMinVersion is the oldest chore that may run this file. Optional: with
	// no value there is no restriction, which is what every existing file wants.
	//
	// It exists because a file's safety can depend on the RUNNER, not only on
	// what the file says. A Taskfile driving money declared its dangerous flags
	// as strings compared to "true" for exactly one reason — chore < 0.4.0 bound
	// an unknown --flag positionally and let a bool take any value, so a typo set
	// another flag. Once that is fixed the file can say what it is written
	// against, instead of carrying the workaround forever.
	//
	// A chore too old to know this field refuses the file anyway: unknown
	// top-level keys are an error, so the floor fails closed even against
	// versions that predate it. This only replaces a confusing message with an
	// actionable one.
	ChoreMinVersion string              `yaml:"chore_min_version"`
	Silent          bool                `yaml:"silent"`
	Dotenv          []string            `yaml:"dotenv"`
	Includes        map[string]*Include `yaml:"includes"`
	Vars            map[string]Var      `yaml:"vars"`
	Env             map[string]Var      `yaml:"env"`
	Tasks           map[string]*Task    `yaml:"tasks"`
	Lifecycle       *Lifecycle          `yaml:"lifecycle"`

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

// chore:manual hooks
// title: Hooks
// summary: before/on_success/on_failure/after, on a task or the whole run
// aliases: lifecycle-hooks lifecycle
// order: 10
//
// # Hooks
//
// Nine hooks in three families. The distinction that decides which one you want
// is not what they do but WHEN they are scoped.
//
// | hook | written as | scope |
// |---|---|---|
// | `before` | a task field | once per task run |
// | `on_success` | a task field | once per task run |
// | `on_failure` | a task field | once per task run |
// | `after` | a task field | once per task run |
// | `lifecycle.before_all` | a top-level block | once per run |
// | `lifecycle.on_success_all` | a top-level block | once per run |
// | `lifecycle.on_failure_all` | a top-level block | once per run |
// | `lifecycle.after_all` | a top-level block | once per run |
// | `defer:` | a step inside `cmds:` | once per task run |
//
// Every `lifecycle:` name is its per-task name plus `_all`, and `_all` is the
// whole mnemonic: it marks the hook that fires once for the `chore` invocation
// rather than once for each task in it.
//
// ## Order, for one task run
//
// ```
// before
//   -> deps (concurrent)
//   -> cmds
//   -> deferred steps, reverse order, only those reached
//   -> on_success | on_failure
//   -> after
// ```
//
// The body unwinds BEFORE the outcome branch, so `after` is a finishing step
// that runs once the thing it is finishing is already down.
//
// `before` precedes `deps:`, not the other way round: a gate is cheaper the
// earlier it fails, and hooks fire for a task that was skipped as up to date
// while `deps:` do not — so any other order would make the gate's position
// depend on the up-to-date result.
//
// ## The four task fields
//
// ```yaml
// build:
//   before:     [ ./check-toolchain.sh ]
//   cmds:       [ make ]
//   on_success: [ ./publish.sh ]
//   on_failure: [ ./collect-logs.sh ]
//   after:      [ 'echo "ended {{.EXIT_CODE}}"' ]
// ```
//
// - **`before` gates.** If it fails, `cmds:` do not run and the task fails with
//   the gate's own status. `on_failure` fires for that failure.
// - **`after` runs as well as the outcome hook, never instead of it.** On failure
//   the order is `on_failure`, then `after`.
// - **`after` reads `{{.EXIT_CODE}}`** — `"0"`, or the task's own status. It is
//   `$EXIT_CODE` in the script too, from the same value, and exists only in
//   `after` and `after_all`.
// - **The last three cannot change the exit status.** They are best-effort: a
//   failure is reported on stderr and the task's own status stands. `on_failure`
//   is where someone will reach to swallow a failure, and it cannot.
// - **They run in the TASK's scope** — its variables, parameters and `dir:` — so
//   `after: echo done {{.TARGET}}` reads the argument the task was called with.
// - **They fire wherever the task runs**, as a dependency or a `- task:` step
//   included. A hook that must fire once per invocation belongs in `lifecycle:`.
// - **They run even when the task is up to date**, because a hook is not the
//   task's prerequisite. That is the whole reason `before` is not a slower
//   spelling of `deps:`.
// - **A `- defer:` inside a hook is refused.** A hook runs to completion at one
//   point in the task's life, so there is nothing for it to defer to.
//
// ## `lifecycle:` — once around the whole run
//
// ```yaml
// lifecycle:
//   before_all:     [ {task: hooks:ensure} ]
//   on_success_all: [ ./notify-green.sh ]
//   on_failure_all: [ ./notify-failure.sh ]
//   after_all:      [ 'echo done with {{.TASK}}, status {{.EXIT_CODE}}' ]
// ```
//
// The canonical use is a self-installing guard: `before_all` activates a repo's
// git hooks the first time anyone runs any task, with no per-task boilerplate,
// and it fires even when the task it wraps is up to date — which a `deps:` entry
// could not, being skipped along with the task.
//
// - `before_all` is a gate. If it fails the task never starts and `after_all`
//   never runs, but `on_failure_all` still fires.
// - `{{.TASK}}` is the invoked task's name.
// - Only the ROOT file's block runs. A `lifecycle:` in an included file is
//   ignored, silently.
// - Skipped for `--list`, `--help` and `--version`, and for a whole run with
//   `--no-lifecycle`.
// - A MISTYPED task name still runs them: the name is resolved inside the run.
//
// ## `child_hooks:` — one task speaking for its whole subtree
//
// ```yaml
// build:all:
//   child_hooks: false                # everything BELOW me runs no hooks
//   deps:  [ prep ]
//   cmds:  [ {task: driver}, {task: driver} ]
//   after: ./sweep.sh                 # mine still runs — once, not twice
// ```
//
// - **It does not touch the declaring task's own hooks.** A task that did not
//   want those would delete them. What it silences is the tree below, which
//   cannot be deleted: the same library task is right to run its hooks when it is
//   the top of a run and wrong when nested inside a bigger one, and only the
//   caller knows which.
// - **It reaches every depth**, through `deps:` and `- task:` alike. A dep is a
//   task invocation, so there is no second rule for it.
// - **A child cannot opt back in.** `child_hooks: true` inside a suppressed
//   subtree does nothing.
// - **It never suppresses `defer:`.** That is what makes deep suppression safe:
//   all it can silence is advice, never a teardown paired with something already
//   brought up.
// - **It says nothing about the `lifecycle:` block**, which is per invocation and
//   not part of anybody's subtree.

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
//		before_all → <task> → on_success_all | on_failure_all → after_all
//
//	  - before_all runs first, once. If it FAILS, the task does not run and neither
//	    does after_all — a setup gate that did not pass must not let work proceed.
//	  - on_success_all and on_failure_all are exclusive: exactly one fires, or
//	    neither if before_all gated the run.
//	  - after_all runs on the way out once the task has been entered, whether the
//	    task succeeded or not, so it can tear down what before_all set up. It runs
//	    IN ADDITION to the outcome hook, not instead of it.
//	  - on_failure_all runs when before_all or the task returns non-zero.
//
// Hooks are skipped for `--list`, `--help` and `version` (which never run a task)
// and can be turned off for a run with `--no-lifecycle`. Each hook is a list of
// steps in the same shape as a task's `cmds:` — a shell line or `- task: name` —
// and `{{.TASK}}` in a hook renders the name of the invoked task.
type Lifecycle struct {
	BeforeAll Cmds `yaml:"before_all"`
	OnSuccess Cmds `yaml:"on_success_all"`
	// OnFailure was `on_error` up to 0.7.0. Renamed with no alias: every global
	// hook is now its per-task name plus `_all`, and `on_error` was the one that
	// broke the pattern. An alias is the right call when a rename would strand
	// existing files; nothing on disk used this one, so the only thing an alias
	// would buy is two spellings of one hook, for ever.
	//
	// A file written against the new names and run by an older chore gets
	// `unknown field "on_failure_all"`, which is confusing rather than wrong;
	// `chore_min_version: 0.8.0` is how that file says what it needs instead.
	OnFailure Cmds `yaml:"on_failure_all"`
	AfterAll  Cmds `yaml:"after_all"`
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
	// Interactive gives the task chore's own terminal: a real stdin, and the
	// foreground process group a full-screen program needs. Opt-in per task,
	// because it costs the cancellation guarantee every other task has — see
	// shell.Shell.Interactive.
	Interactive bool `yaml:"interactive"`
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

	// The per-task half of the lifecycle. Same four names the file-level block
	// uses, minus the `_all` that marks a hook as per-invocation, and they fire
	// every time the task runs — including when another task calls it. A hook
	// that must fire once per run belongs in `lifecycle:`, which is what that
	// block is for.
	//
	// Order, with the body in the middle:
	//
	//	before → deps → cmds → deferred steps → on_success|on_failure → after
	//
	// `before` precedes `deps:` rather than following them, which the first design
	// had the other way round. Two reasons, and the second is decisive: a gate is
	// cheaper the earlier it fails, and there is no dependency work to undo when
	// the toolchain check it guards has already said no. And a hook must fire even
	// when the task is up to date, while deps do NOT — so "deps then before" could
	// only hold for a task that was not skipped, making the gate's position depend
	// on the up-to-date result. A fixed position is worth more than the tidier
	// diagram.
	//
	// Before gates, exactly as before_all does: if it fails, cmds do not run,
	// and the task fails with the gate's status. The other three are
	// best-effort and cannot change it — on_failure especially, which is where
	// someone will reach to swallow a failure.
	//
	// Deferred steps unwind BEFORE the outcome branch, because `after` is the
	// finishing step and finishing something while it is still up is the wrong
	// way round: a task that brought a topology up has taken it down again by
	// the time `after` sweeps.
	Before    Cmds `yaml:"before"`
	OnSuccess Cmds `yaml:"on_success"`
	OnFailure Cmds `yaml:"on_failure"`
	// After runs whatever the outcome, IN ADDITION to on_success/on_failure
	// rather than instead of them, and reads {{.EXIT_CODE}} to tell which
	// happened. Without that variable the only way to write "always" would be
	// the same line in both outcome hooks — the duplication After exists to
	// remove.
	After Cmds `yaml:"after"`

	// ChildHooks, set false, suppresses the hooks of every task BELOW this one —
	// its deps, its `- task:` steps, and everything they reach in turn — while
	// leaving this task's own hooks alone.
	//
	// It is the coordinator's declaration that it is handling something for the
	// whole tree: `build:all` sweeps once in its own `after:` instead of letting
	// three `driver` runs sweep three times. Suppressing its OWN hooks is not
	// what it is for — a task that did not want those would delete them; the
	// hooks below cannot be deleted, because the same library task is right to
	// run them when it is the top of a run and wrong when it is nested inside
	// one, and only the caller knows which.
	//
	// Deep, and a child cannot opt back in: the guarantee is written at the
	// coordinator and has to be readable there. `defer:` is never suppressed at
	// any depth — that is what makes deep suppression safe, since all this can
	// silence is advice, never a teardown that pairs with something already
	// brought up.
	//
	// A pointer so "not declared" is distinguishable from an explicit `true`.
	ChildHooks *bool `yaml:"child_hooks"`

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
	// Short is a single-letter alias, so `-f` can mean `--force`. Opt-in per
	// parameter, because a single-dash word is otherwise DATA — `chore logs -f
	// api` passes -f to the task — and deriving shorts automatically would
	// silently change what every existing file does.
	Short string `yaml:"short"`
	Type  string `yaml:"type"` // "" or "string" (default), "bool", "int"
	Desc  string `yaml:"desc"`
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

// Shorts returns the declared short flags, dash included, in declared order.
func (as Args) Shorts() []string {
	var out []string
	for _, a := range as {
		if a.Short != "" {
			out = append(out, "-"+a.Short)
		}
	}
	return out
}

// ParseSemver reads MAJOR.MINOR.PATCH, with an optional leading v, into
// comparable parts. ok is false for anything else — including chore's own
// "dev+a581449", which is deliberately not a version and must not be treated as
// one.
func ParseSemver(s string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "" {
			return out, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// SemverLess compares parts numerically, so 0.10.0 is newer than 0.4.0 — which a
// string comparison gets backwards.
func SemverLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// ValidShort reports whether s can be a short flag: exactly one ASCII letter.
// Digits are excluded because `-5` is a negative number reaching an int
// parameter, and that has to stay data.
func ValidShort(s string) bool {
	if len(s) != 1 {
		return false
	}
	c := s[0]
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
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

// BoolLiteral reports whether s is a word NormalizeBool can READ as a boolean,
// rather than one it merely coerces.
//
// NormalizeBool has to answer for every value a variable can arrive with —
// defaults, dotenv, environment — so "anything not false is true" is the right
// rule there. At the command line it is the wrong one: it makes every typo a
// `true`. `chore deploy typo` set a `live` flag, and so did `-x`, because the
// text was never a boolean and nothing said so. This is the vocabulary a caller
// is allowed to type, and the caller-facing checks reject the rest.
func BoolLiteral(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "false", "no", "off", "1", "true", "yes", "on":
		return true
	}
	return false
}

// RunOnce reports whether the task should execute at most once per invocation.
func (t *Task) RunOnce() bool { return t.Run == "once" }

// SuppressesChildHooks reports whether this task turns off hooks for everything
// it invokes. Absent means no: hooks run unless a coordinator says otherwise.
func (t *Task) SuppressesChildHooks() bool {
	return t.ChildHooks != nil && !*t.ChildHooks
}

// HasHooks reports whether the task declares any lifecycle hook at all, so the
// runner can skip the whole apparatus for the overwhelming majority of tasks
// that declare none.
func (t *Task) HasHooks() bool {
	return len(t.Before) > 0 || len(t.OnSuccess) > 0 || len(t.OnFailure) > 0 || len(t.After) > 0
}
