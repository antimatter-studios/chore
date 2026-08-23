// Package run schedules and executes tasks.
//
// The variable scope is assembled once per invocation, in the order the spec
// documents, with positional arguments at the top — which is the difference
// that motivated this program. Task resolves `dotenv:` while parsing its
// Taskfile, before command-line variables exist, so a config could only be
// selected by an environment variable set before the command.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/fingerprint"
	"github.com/antimatter-studios/chore/internal/shell"
	"github.com/antimatter-studios/chore/internal/tmpl"
)

// Runner executes tasks from a loaded project.
type Runner struct {
	Project *chorefile.Project
	Out     io.Writer
	Err     io.Writer

	DryRun  bool
	Force   bool
	Verbose bool
	// NoLifecycle turns off the file's `lifecycle:` hooks for this run (--no-lifecycle).
	NoLifecycle bool
	CLIArgs     string // everything after `--`

	// ChoreExe and ChoreVersion describe the binary actually running, published
	// to tasks as {{.CHORE_EXE}} and {{.CHORE_VERSION}}.
	//
	// Not {{.CHORE}}: that name is already the ENV var set to 1 so a Taskfile can
	// tell this runner from go-task.
	//
	// A task that has to invoke chore — a launchd plist needing an absolute
	// ProgramArguments path, a wrapper, a self-check — otherwise resolves the word
	// `chore` through PATH, which answers "the one I would get if I typed it",
	// not "the one running me". Those differ exactly when it matters: a file whose
	// env: pins PATH, or a dev binary run from a checkout, silently writes the
	// installed copy into the plist instead of itself.
	ChoreExe     string
	ChoreVersion string

	// CLIVars are the NAME=value pairs typed on the command line.
	//
	// They are global to the RUN, not to the task named, and they outrank anything
	// written in the file. What you typed is what you meant: `chore down
	// CONFIG=mail1` has to reach the `- task: postgres:down` that `down` calls, or
	// the child renders `mailref-{{.CONFIG}}-postgres` as "mailref--postgres",
	// matches no container, and reports success having stopped nothing. That is the
	// exact class of silent-wrong-target failure this program exists to remove.
	//
	// The one thing above them is vars a parent passes EXPLICITLY to a child
	// (`- task: x` with `vars:`), because a file that names a value for one step is
	// describing that step, not taking a guess: e2e brings up two reference servers
	// by passing each its own name, and a global must not collapse them into one.
	CLIVars map[string]string

	// once tracks `run: once` tasks by name plus rendered variables, so the same
	// task invoked with different arguments still runs twice.
	onceMu sync.Mutex
	once   map[string]*onceEntry

	// warned tracks dotenv files already reported missing, so a run of 20 tasks
	// that share a config does not print the same line 20 times.
	warnMu sync.Mutex
	warned map[string]bool
}

// New returns a Runner writing to out and errOut.
func New(p *chorefile.Project, out, errOut io.Writer) *Runner {
	return &Runner{
		Project: p,
		Out:     out,
		Err:     errOut,
		once:    map[string]*onceEntry{},
		warned:  map[string]bool{},
	}
}

// Invoke is the top-level entry for the one task named on the command line. It
// runs the file's lifecycle hooks around that task — before_all before it,
// after_all/on_error after — then defers to Run for the task itself. Sub-tasks
// (deps and `- task:` steps) go through Run directly and are NOT re-wrapped, so
// the hooks fire exactly once per `chore` invocation.
//
// Backward compatible: with no `lifecycle:` block (or --no-lifecycle) this is
// just Run.
func (r *Runner) Invoke(ctx context.Context, name string, args []string, callVars map[string]string) (err error) {
	// `internal: true` means "callable, but not by a person": a helper factored out
	// of two tasks is part of their implementation, not part of the command
	// surface. Hiding it from --list said that and did not enforce it, so the
	// promise was documentation. Refusing it HERE and not in Run is the whole
	// distinction — Invoke is the command line's entry point and has exactly one
	// caller, while deps: and `- task:` steps go through Run, which is untouched.
	//
	// Refused before before_all, because a run that will not happen should not run
	// a setup gate for it.
	// EXCEPT when chore is the caller. CHORE=1 is exported to every task script
	// (see shell()), so a nested `{{.CHORE_EXE}} _helper` reaching this point is
	// chore calling itself from inside a run — which is the documented way to
	// capture a helper's VALUE, since a `- task:` step returns nothing. Refusing
	// it would have broken the one pattern that makes an internal helper useful
	// for anything but side effects. The rule is "callable by chore, not by a
	// person", and this is chore.
	if t, ok := r.Project.Tasks[name]; ok && t.Internal && os.Getenv("CHORE") == "" {
		return fmt.Errorf("%s is internal: a task can call it with deps:, `- task:`, or `{{.CHORE_EXE}} %s` — but it cannot be run from the command line", name, name)
	}

	lc := r.lifecycle()
	if lc == nil {
		return r.Run(ctx, name, args, callVars)
	}

	// on_error fires for a failure of EITHER before_all or the task, so register it
	// first (outermost defer) against the named return value.
	defer func() {
		if err != nil {
			r.runHookBestEffort(ctx, "on_error", lc.OnError, name)
		}
	}()

	if e := r.runHook(ctx, "before_all", lc.BeforeAll, name); e != nil {
		// A setup gate that did not pass stops the run — the task never starts and
		// there is nothing for after_all to tear down.
		err = fmt.Errorf("before_all: %w", e)
		return err
	}
	// Now that the run has been entered, after_all is a trap: it runs on the way
	// out whether the task succeeds or fails.
	defer r.runHookBestEffort(ctx, "after_all", lc.AfterAll, name)

	err = r.Run(ctx, name, args, callVars)
	return err
}

// lifecycle returns the file's lifecycle hooks, or nil when there are none or the
// run opted out with --no-lifecycle.
func (r *Runner) lifecycle() *chorefile.Lifecycle {
	if r.NoLifecycle || r.Project == nil || r.Project.Root == nil {
		return nil
	}
	return r.Project.Root.Lifecycle
}

// runHook runs one lifecycle hook — a list of steps (shell lines or `- task:`
// calls) — as if it were a tiny internal task of the root file. trigger is the
// name of the invoked task, exposed to the hook as {{.TASK}}. An empty hook is a
// no-op. A step failure is returned so before_all can gate the run.
func (r *Runner) runHook(ctx context.Context, hook string, cmds chorefile.Cmds, trigger string) error {
	if len(cmds) == 0 {
		return nil
	}
	t := &chorefile.Task{
		Name:     "lifecycle:" + hook,
		Cmds:     cmds,
		Internal: true,
		File:     r.Project.Root,
	}
	// {{.TASK}} in a hook is the task the run is about, not the synthetic hook task,
	// so callers can log or branch on it. CLIVars ride along like any other run.
	cv := map[string]string{"TASK": trigger}
	for k, v := range r.CLIVars {
		if k != "TASK" {
			cv[k] = v
		}
	}
	scope, err := r.scope(ctx, t, nil, cv)
	if err != nil {
		return fmt.Errorf("lifecycle %s: %w", hook, err)
	}
	if err := r.execute(ctx, t, scope); err != nil {
		return fmt.Errorf("lifecycle %s: %w", hook, err)
	}
	return nil
}

// runHookBestEffort runs a teardown/notify hook whose own failure must not mask
// the run's outcome; it only reports the failure.
func (r *Runner) runHookBestEffort(ctx context.Context, hook string, cmds chorefile.Cmds, trigger string) {
	// after_all and on_error are teardown, so they run on the way out of an
	// INTERRUPTED run too — the same reason deferred steps get a grace context.
	ctx, done := cleanupContext(ctx)
	defer done()
	if err := r.runHook(ctx, hook, cmds, trigger); err != nil {
		fmt.Fprintf(r.Err, "chore: %v\n", err)
	}
}

// Run executes a task by name. args are its positional parameters; callVars are
// variables supplied by a `- task:` reference or a dependency.
func (r *Runner) Run(ctx context.Context, name string, args []string, callVars map[string]string) error {
	t, ok := r.Project.Tasks[name]
	if !ok {
		return fmt.Errorf("no task %q%s", name, r.suggest(name))
	}

	scope, err := r.scope(ctx, t, args, callVars)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	if skip, why := skipPlatform(t); skip {
		if r.Verbose {
			fmt.Fprintf(r.Out, "  skipping %s (%s)\n", name, why)
		}
		return nil
	}
	if err := checkArgs(t, args, callVars, scope); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := checkRequires(t, scope); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	// `run: once` is keyed on the rendered variables, not just the name: two
	// invocations with different arguments are different work.
	if t.RunOnce() {
		e := r.onceEntry(onceKey(name, scope))
		e.once.Do(func() { e.err = r.execute(ctx, t, scope) })
		return e.err
	}
	return r.execute(ctx, t, scope)
}

func (r *Runner) execute(ctx context.Context, t *chorefile.Task, scope *tmpl.Scope) error {
	dir, err := r.taskDir(t, scope)
	if err != nil {
		return fmt.Errorf("%s: %w", t.Name, err)
	}
	sh := r.shell(dir, scope)

	if !r.Force {
		up, err := fingerprint.UpToDate(ctx, t, scope, sh, dir, r.cacheDir())
		if err != nil {
			return fmt.Errorf("%s: up-to-date check: %w", t.Name, err)
		}
		if up {
			if !r.silent(t) {
				fmt.Fprintf(r.Out, "task: %s is up to date\n", t.Name)
			}
			return nil
		}
	}

	if err := r.deps(ctx, t, scope); err != nil {
		return err
	}

	// Deferred steps run when the task finishes, in reverse order, whether or not
	// it succeeded — which is the only reason a task can promise to tear down
	// what it brought up.
	var deferred []chorefile.Cmd
	var runErr error

	for i, c := range t.Cmds {
		if c.Defer {
			deferred = append(deferred, c)
			continue
		}
		if err := r.command(ctx, t, scope, sh, c); err != nil {
			if c.IgnoreError || t.IgnoreError {
				fmt.Fprintf(r.Err, "task: %s: cmd %d failed, ignored: %v\n", t.Name, i+1, err)
				continue
			}
			runErr = err
			break
		}
	}

	// Teardown outlives an interrupt. Once the run's context is cancelled,
	// exec.CommandContext refuses to START a process, so passing ctx straight
	// through would silently skip every deferred step at the one moment they
	// matter most: Ctrl-C on a task that brought a topology up. The grace budget
	// is bounded so a hung teardown cannot wedge the tool — and a second Ctrl-C
	// is not caught at all, so there is always a way out.
	cleanupCtx, endCleanup := cleanupContext(ctx)
	defer endCleanup()

	for i := len(deferred) - 1; i >= 0; i-- {
		// A deferred step runs even after a failure, and its own failure must not
		// hide why the task failed in the first place.
		if err := r.command(cleanupCtx, t, scope, sh, deferred[i]); err != nil {
			fmt.Fprintf(r.Err, "task: %s: deferred step failed: %v\n", t.Name, err)
			if runErr == nil {
				runErr = err
			}
		}
	}
	if runErr != nil {
		return runErr
	}

	if len(t.Sources) > 0 || len(t.Generates) > 0 {
		if err := fingerprint.Save(t, dir, r.cacheDir()); err != nil {
			return fmt.Errorf("%s: recording fingerprint: %w", t.Name, err)
		}
	}
	return nil
}

// cleanupGrace bounds teardown that runs after the run's context is already
// cancelled. Long enough for a `docker rm` or an `rm -f`, short enough that a
// wedged teardown is not the thing keeping someone at the terminal.
const cleanupGrace = 15 * time.Second

// cleanupContext returns a context fit for teardown. While the run is healthy it
// is the run's own context, so nothing changes. Once that context is cancelled —
// an interrupt, or a deadline — it is a FRESH one with a bounded budget, because
// teardown issued on a cancelled context never starts.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.Background(), cleanupGrace)
}

// deps runs a task's dependencies concurrently. The first failure cancels the
// rest, and its error is what the task reports.
func (r *Runner) deps(ctx context.Context, t *chorefile.Task, scope *tmpl.Scope) error {
	if len(t.Deps) == 0 {
		return nil
	}
	g, gctx := errgroup.WithContext(ctx)
	for _, d := range t.Deps {
		d := d
		g.Go(func() error {
			name, err := scope.Render(d.Task)
			if err != nil {
				return fmt.Errorf("%s: dep name: %w", t.Name, err)
			}
			name = reference(t, name)
			vars, err := scope.Resolve(gctx, d.Vars, r.shell(r.Project.RootDir, scope))
			if err != nil {
				return fmt.Errorf("%s: dep %s vars: %w", t.Name, name, err)
			}
			return r.Run(gctx, name, nil, vars)
		})
	}
	return g.Wait()
}

// command runs one step: either a call to another task or a shell script.
func (r *Runner) command(ctx context.Context, t *chorefile.Task, scope *tmpl.Scope, sh shell.Shell, c chorefile.Cmd) error {
	if c.Task != "" {
		name, err := scope.Render(c.Task)
		if err != nil {
			return fmt.Errorf("%s: cmd task name: %w", t.Name, err)
		}
		name = reference(t, name)
		vars, err := scope.Resolve(ctx, c.Vars, sh)
		if err != nil {
			return fmt.Errorf("%s: cmd task %s vars: %w", t.Name, name, err)
		}
		return r.Run(ctx, name, nil, vars)
	}

	script, err := scope.Render(c.Cmd)
	if err != nil {
		return fmt.Errorf("%s: rendering cmd: %w", t.Name, err)
	}
	if strings.TrimSpace(script) == "" {
		return nil
	}

	echo := !r.silent(t) && !c.Silent
	if r.DryRun {
		fmt.Fprintf(r.Out, "%s\n", script)
		return nil
	}
	if echo {
		fmt.Fprintf(r.Out, "%s\n", script)
	}
	if err := sh.Run(ctx, script); err != nil {
		return fmt.Errorf("%s: %w", t.Name, err)
	}
	return nil
}

// scope assembles the variables for one invocation.
//
// Lowest priority first: process environment, dotenv, the task's file vars
// (which the loader has already merged its include's vars into), task vars, vars
// passed by the caller, then positional arguments. Nothing later shadows
// something earlier, and it is all resolved here — there is no second, earlier
// evaluation point of the kind that made Task's dotenv see stale values.
func (r *Runner) scope(ctx context.Context, t *chorefile.Task, args []string, callVars map[string]string) (*tmpl.Scope, error) {
	base := tmpl.New(os.Environ())
	base.Set("ROOT_DIR", r.Project.RootDir)
	base.Set("TASK", t.Name)
	base.Set("CLI_ARGS", r.CLIArgs)
	base.Set("CHORE_EXE", r.ChoreExe)
	base.Set("CHORE_VERSION", r.ChoreVersion)
	if wd, err := os.Getwd(); err == nil {
		base.Set("USER_WORKING_DIR", wd)
	}
	if t.File != nil {
		base.Set("TASKFILE_DIR", t.File.Dir)
	}

	// Arguments bind before dotenv is resolved, so a dotenv path may reference
	// them: dotenv: ['config/{{.CONFIG}}/config.env'].
	argVars, err := bindArgs(t, args)
	if err != nil {
		return nil, err
	}
	if err := checkArgConflicts(t, argVars, callVars); err != nil {
		return nil, err
	}
	// Positional values are checked as they bind; named ones arrive by another
	// route and would otherwise skip the check entirely.
	if err := checkNamedTypes(t, callVars); err != nil {
		return nil, err
	}
	early := base.Push(r.CLIVars).Push(argVars).Push(callVars)

	sh := r.shell(r.Project.RootDir, early)

	// A declared parameter's DEFAULT has to be available before the dotenv path is
	// rendered, because the path is usually keyed on that very parameter
	// (dotenv: ['config/{{.CONFIG}}/config.env']). Task vars as a whole cannot be
	// resolved this early — they are allowed to read dotenv values — so resolve
	// just the parameter defaults, and only those the caller did not supply.
	paramDefaults, err := early.Resolve(ctx, parameterVars(t, argVars, callVars), sh)
	if err != nil {
		return nil, fmt.Errorf("parameter defaults: %w", err)
	}
	// A boolean default becomes "" rather than "false", so {{if .VERBOSE}} and
	// [ -n "$VERBOSE" ] both read it correctly — a template treats the string
	// "false" as true.
	for k, v := range paramDefaults {
		if t.ParamIsBool(k) {
			paramDefaults[k] = chorefile.NormalizeBool(v)
		}
	}
	// A default is written in one case; the path that consumes it may use the
	// other. `vars: {config: x}` must satisfy dotenv: ['config/{{.CONFIG}}/…'].
	for k, v := range maps.Clone(paramDefaults) {
		if upper := strings.ToUpper(k); upper != k {
			if _, ok := paramDefaults[upper]; !ok {
				paramDefaults[upper] = v
			}
		}
		if lower := strings.ToLower(k); lower != k {
			if _, ok := paramDefaults[lower]; !ok {
				paramDefaults[lower] = v
			}
		}
	}

	// File vars are resolved with arguments visible, so the self-defaulting idiom
	// CONFIG: '{{.CONFIG | default "x"}}' picks up a caller's value. But when the
	// dotenv PATH is rendered the caller must outrank the file outright:
	// otherwise a literal `vars: {CONFIG: a}` loads config a's environment while
	// the task runs with CONFIG=b — the silent wrong-stack failure this program
	// exists to remove, reintroduced one layer down.
	dotenvVars, err := r.dotenv(ctx, t, base.Push(r.CLIVars).Push(paramDefaults), callVars, argVars, sh)
	if err != nil {
		return nil, err
	}

	// File vars are resolved AFTER dotenv, because they routinely read it: an
	// include maps `ADMIN_PROJECT: '{{.RESTMAIL_PROJECT}}'`, and RESTMAIL_PROJECT
	// exists only once the config's env file is loaded. Resolving them earlier
	// yields empty strings and container names like "-postgres".
	//
	// The dotenv PATHS above are rendered from the same vars but resolved without
	// dotenv in scope — a path may depend on the argument, never on the contents
	// of the file it is about to load.
	// A file's variables, and the ones an include MAPPED into it.
	//
	// The two are resolved in different scopes, which is the whole point. An
	// include's `vars: {IP: '{{.POSTGRES_IP}}'}` is written in the parent and means
	// the parent's POSTGRES_IP, so it is rendered walking DOWN from the root: each
	// file's own variables form the scope in which its includes' mappings are
	// rendered. A file's own `vars:` then resolve seeing only the outside world and
	// what was mapped to it — never the parent's other variables, because an
	// include seeing everything above it is exactly the bleed this file format is
	// famous for. (`inherit:` is how a file will opt into that deliberately.)
	//
	// Resolved the other way round, `{{.POSTGRES_IP}}` renders as "" and a reference
	// mail server starts as "mailref-mail1-postgres @" with no address.
	outside := base.Push(dotenvVars).Push(paramDefaults).Push(r.CLIVars).
		Push(callVars).Push(argVars)

	// Walk the include chain from the root down. Two scopes, deliberately:
	//
	//   effective — everything the file at this level ends up contributing. The
	//               NEXT level's mappings are rendered against it, because
	//               `vars: {IP: '{{.POSTGRES_IP}}'}` is written HERE and means
	//               this file's POSTGRES_IP.
	//   visible   — what the file at this level may SEE while resolving its own
	//               `vars:`. Just the mapping, unless the include says `inherit:`.
	//
	// Keeping them apart is the whole point. Collapsing them lets an included file
	// silently read every variable above it, which is the bleed this format is
	// known for and which rest-mail's own comments warn about: two includes of one
	// file then differ by whatever their parents happened to define.
	fileVars := map[string]string{}
	effective := map[string]string{}
	for _, f := range fileChain(t.File) { // root first, the task's file last
		mapped := map[string]string{}
		if len(f.IncludeVars) > 0 {
			mapped, err = outside.Push(effective).Resolve(ctx, f.IncludeVars, sh)
			if err != nil {
				return nil, fmt.Errorf("include vars for %s: %w", f.Path, err)
			}
		}

		visible := mapped
		if f.Inherit {
			// Opt-in: the including file's values, with the mapping on top.
			visible = mergeStrings(effective, mapped)
		}
		own, err := outside.Push(visible).Resolve(ctx, f.Vars, sh)
		if err != nil {
			return nil, fmt.Errorf("file vars: %w", err)
		}
		// The mapping outranks the file's own value: the file states what it needs
		// by default, the include states what it is being handed. An inherited
		// value sits below both — a file always wins on a name it defines itself.
		fileVars = mergeStrings(mergeStrings(visible, own), mapped)
		effective = fileVars
	}

	// `env:` is the process environment, so unlike `vars:` it is INHERITED rather
	// than scoped: the ROOT file's env reaches a task in an included file. That is
	// what this exists for — one `OUTPUT` at the root, consumed by every image
	// build in tasks/*.yml as `>$OUTPUT`. Scoping it the way include vars are
	// scoped would leave those redirects empty, which is bash's "ambiguous
	// redirect" and a failed task.
	//
	// Resolved after dotenv, so an env value may read a config's variables, and
	// before the task's own vars, so those can read it. A value is a Var like any
	// other — `sh:` belongs to the VALUE, not to the key it sits under, so it works
	// here for the same reason it works in `vars:`, through the same resolver.
	fileEnv := map[string]string{}
	for _, f := range envFiles(r.Project, t) {
		resolved, err := base.Push(dotenvVars).Push(fileEnv).Push(paramDefaults).
			Push(r.CLIVars).Push(callVars).Push(argVars).Resolve(ctx, f.Env, sh)
		if err != nil {
			return nil, fmt.Errorf("file env: %w", err)
		}
		maps.Copy(fileEnv, resolved)
	}

	// Above fileVars: where a file sets the same name in both, `env:` is what the
	// shell would see under go-task, and a template disagreeing with the shell
	// about one name is worse than either answer.
	scope := base.Push(dotenvVars).Push(fileVars).Push(fileEnv).Push(paramDefaults)
	taskVars, err := scope.Push(argVars).Push(callVars).Resolve(ctx, t.Vars, sh)
	if err != nil {
		return nil, fmt.Errorf("task vars: %w", err)
	}
	taskEnv, err := scope.Push(taskVars).Push(argVars).Push(callVars).Resolve(ctx, t.Env, sh)
	if err != nil {
		return nil, fmt.Errorf("task env: %w", err)
	}
	// The caller stays on top of both: flipping a toggle per invocation
	// (`chore up VERBOSE=1`) is the reason these values are variable at all.
	// Typed values above everything the file says, and above the task's own vars:
	// a `vars:` block is a default, not a veto on the command line.
	final := scope.Push(taskVars).Push(taskEnv).Push(r.CLIVars).Push(callVars).Push(argVars)

	// Boolean parameters are normalised HERE, not only where their default is
	// resolved: the task's own vars are pushed on top afterwards and would
	// otherwise put the raw "false" back, making {{if .FOLLOW}} fire on a flag
	// nobody passed.
	for _, spec := range t.Args {
		name := spec.Name
		if !spec.IsBool() {
			continue
		}
		for _, spelling := range []string{name, strings.ToUpper(name), strings.ToLower(name)} {
			if v, ok := final.Get(spelling); ok {
				final.Set(spelling, chorefile.NormalizeBool(v))
			}
		}
	}

	// A parameter's DEFAULT comes from vars, which the author writes in one case
	// only. Mirror it to the other so `args: [config]` with `vars: {config: x}`
	// still answers to {{.CONFIG}}, exactly as a supplied argument does — the
	// alternative is a value that works when passed and vanishes when defaulted.
	for _, spec := range t.Args {
		name := spec.Name
		other := strings.ToUpper(name)
		if other == name {
			other = strings.ToLower(name)
		}
		if other == name {
			continue
		}
		have, ok := final.Get(name)
		mirror, mirrorSet := final.Get(other)
		switch {
		case ok && have != "" && mirror == "":
			final.Set(other, have)
		case mirrorSet && mirror != "" && have == "":
			final.Set(name, mirror)
		}
	}
	return final, nil
}

// declaresDefault reports whether the task or its file defines a variable for
// the parameter, in any spelling — presence, not emptiness.
func declaresDefault(t *chorefile.Task, name string) bool {
	for _, spelling := range []string{name, strings.ToUpper(name), strings.ToLower(name)} {
		if _, ok := t.Vars[spelling]; ok {
			return true
		}
		if t.File != nil {
			if _, ok := t.File.Vars[spelling]; ok {
				return true
			}
		}
	}
	return false
}

// checkArgConflicts rejects a parameter given twice with different values, e.g.
// `chore up mail4.test --config restmail.test`. One of the two was going to be
// ignored, and picking a winner silently is how you act on a config you did not
// mean to name.
func checkArgConflicts(t *chorefile.Task, argVars, callVars map[string]string) error {
	for _, spec := range t.Args {
		name := spec.Name
		positional, given := argVars[name]
		if !given {
			continue
		}
		for _, spelling := range []string{name, strings.ToUpper(name), strings.ToLower(name)} {
			named, ok := callVars[spelling]
			if ok && named != positional {
				return fmt.Errorf("%s given twice: %q positionally and %q as %s",
					name, positional, named, spelling)
			}
		}
	}
	return nil
}

// parameterVars picks out the task vars that define defaults for declared
// parameters the caller did not supply. Under both spellings, since a parameter
// answers to either.
func parameterVars(t *chorefile.Task, supplied ...map[string]string) map[string]chorefile.Var {
	given := func(name string) bool {
		for _, m := range supplied {
			if _, ok := m[name]; ok {
				return true
			}
			if _, ok := m[strings.ToUpper(name)]; ok {
				return true
			}
		}
		return false
	}
	out := map[string]chorefile.Var{}
	for _, spec := range t.Args {
		name := spec.Name
		if given(name) {
			continue
		}
		for _, spelling := range []string{name, strings.ToUpper(name), strings.ToLower(name)} {
			if v, ok := t.Vars[spelling]; ok {
				out[spelling] = v
			}
		}
	}
	return out
}

// bindArgs maps positional arguments onto the names the task declares. Fewer
// arguments than parameters is allowed — the remaining names fall through to
// vars, which is how a default value works. More is an error, because it means
// the caller expected something the task will not do.
func bindArgs(t *chorefile.Task, args []string) (map[string]string, error) {
	out := map[string]string{}
	if len(args) > len(t.Args) {
		if len(t.Args) == 0 {
			return nil, fmt.Errorf("task %s takes no arguments, got %d (%s)",
				t.Name, len(args), strings.Join(args, " "))
		}
		return nil, fmt.Errorf("task %s takes %d argument(s) (%s), got %d",
			t.Name, len(t.Args), strings.Join(t.Args.Names(), ", "), len(args))
	}
	for i, value := range args {
		spec := t.Args[i]
		name := spec.Name
		if err := checkFlagShaped(t, value); err != nil {
			return nil, err
		}
		if err := checkArgType(t, spec, value); err != nil {
			return nil, err
		}
		a := value
		out[name] = a
		// Go templates are case-sensitive and Taskfile convention is uppercase, so
		// `args: [config]` must also answer to {{.CONFIG}}. Binding only the name
		// as written means a Taskfile copied from this program's own usage text
		// interpolates an empty string — the exact silent default it exists to
		// prevent.
		if upper := strings.ToUpper(name); upper != name {
			out[upper] = a
		}
	}
	return out, nil
}

// checkArgs fails when a declared parameter was neither supplied nor defaulted.
// Running with an empty value is how a command ends up addressing nothing at
// all, so an unsupplied parameter with no default is an error, not a blank.
func checkArgs(t *chorefile.Task, args []string, callVars map[string]string, scope *tmpl.Scope) error {
	var missing []string

	for i, a := range t.Args {
		name := a.Name
		if i < len(args) || suppliedByName(name, callVars) {
			continue
		}
		// A flag is never required: its absence IS its value. Demanding one would
		// mean writing `chore logs --follow=false` to say nothing at all.
		if a.IsBool() {
			continue
		}
		if v, _ := scope.Get(name); v != "" {
			continue
		}
		if v, _ := scope.Get(strings.ToUpper(name)); v != "" {
			continue
		}
		// A DECLARED default satisfies the parameter even when it is empty:
		// `vars: {filter: ""}` is the author saying this one is optional. That is
		// also why there is no required/optional marker in `args:` — the presence
		// of a default already says which it is.
		if declaresDefault(t, name) {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		return fmt.Errorf("needs argument(s) %s — pass positionally (chore %s <%s>), as %s=value, or give it a default in vars",
			strings.Join(missing, ", "), t.Name, missing[0], strings.ToUpper(missing[0]))
	}
	return nil
}

// checkFlagShaped refuses a positional argument spelled like a long flag.
//
// A --word that reaches binding named no parameter the task declares — the
// lookup in splitArgs already had its chance — so it is a mistake, and binding
// it is the worst available answer: it becomes the value of an unrelated
// parameter, and for a bool NormalizeBool reads anything outside
// {"", "0", "false", "no", "off"} as "true". Measured on a Taskfile driving a
// trading platform, `chore tick --total-nonsense` rendered a live `tick`, and
// `chore backtest --robot-name x` set both holdout and force — a mistyped flag
// turning on flags nobody named. It is the same failure that once made
// `chore instance:up --help` START a stack, generalised past --help.
//
// Single-dash words are deliberately left alone, because `chore logs -f api`
// means "run logs with -f and api", and `--` still hands everything after it
// to the command verbatim.
func checkFlagShaped(t *chorefile.Task, value string) error {
	// A task that declares `short:` anywhere has opted into short-flag parsing,
	// so a single-dash letter it does not know is a typo, whatever the parameter
	// it would land on: with a string first, `-z` would silently BE the value.
	// Files that declare no shorts keep the data behaviour entirely — that is
	// what `chore logs -f api` relies on — and a bundle or a negative number is
	// left alone either way.
	if len(value) == 2 && value[0] == '-' && chorefile.ValidShort(value[1:]) {
		if shorts := t.Args.Shorts(); len(shorts) > 0 {
			return fmt.Errorf("task %s: %s is not one of its short flags (%s)",
				t.Name, value, strings.Join(shorts, ", "))
		}
	}
	if !strings.HasPrefix(value, "--") || len(value) <= 2 {
		return nil
	}
	// Named back in the spelling a caller would type: hyphens fold onto the
	// declared underscores, so --dry-run is what to suggest for `dry_run`.
	spellings := make([]string, len(t.Args))
	for i, a := range t.Args {
		spellings[i] = "--" + strings.ReplaceAll(a.Name, "_", "-")
	}
	return fmt.Errorf(
		"task %s: %s is not one of its parameters (%s); to pass it along as data instead: chore %s -- %s",
		t.Name, value, strings.Join(spellings, ", "), t.Name, value)
}

// checkArgType rejects a value the parameter cannot mean. Only int is checked:
// a string takes anything, and a bool is set by presence rather than by a value
// the caller types.
func checkArgType(t *chorefile.Task, spec chorefile.Arg, value string) error {
	if spec.IsBool() {
		if chorefile.BoolLiteral(value) {
			return nil
		}
		// A bool was the one declared type nothing validated, so any word bound
		// to it became "true".
		hint := ""
		if strings.HasPrefix(value, "-") {
			// The single-dash case earns the longer explanation: it binds by
			// POSITION, not by letter, so `chore t -c` set the FIRST parameter
			// whatever letter was typed — and coercion hid it by making the
			// answer "true" either way.
			hint = " (a single-dash word is data, and binds by position, not by letter)"
		}
		return fmt.Errorf(
			"task %s: %s must be true or false, got %q; a flag is supplied as --%s%s",
			t.Name, spec.Name, value, strings.ReplaceAll(spec.Name, "_", "-"), hint)
	}
	if spec.Type != chorefile.TypeInt {
		return nil
	}
	if _, err := strconv.Atoi(strings.TrimSpace(value)); err != nil {
		return fmt.Errorf("task %s: %s must be a whole number, got %q", t.Name, spec.Name, value)
	}
	return nil
}

// checkNamedTypes validates values supplied as --name or NAME=value.
func checkNamedTypes(t *chorefile.Task, callVars map[string]string) error {
	for _, spec := range t.Args {
		for _, spelling := range []string{spec.Name, strings.ToUpper(spec.Name), strings.ToLower(spec.Name)} {
			v, ok := callVars[spelling]
			if !ok {
				continue
			}
			if err := checkArgType(t, spec, v); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// suppliedByName reports whether the caller named the parameter, in any spelling.
func suppliedByName(name string, callVars map[string]string) bool {
	for _, spelling := range []string{name, strings.ToUpper(name), strings.ToLower(name)} {
		if _, ok := callVars[spelling]; ok {
			return true
		}
	}
	return false
}

// dotenv loads the env files declared by the root file and, if different, the
// task's own file. A missing file is an error: silently continuing with
// defaults is how an entire stack ends up running against unset variables. A
// path prefixed with `?` is optional.
// dotenvSource is one file's dotenv declaration together with the variables its
// paths must be rendered against — its OWN. A path in the root Taskfile means
// what the root file says, even when the task being run came from an include:
// rendering `{{.CONFIG_DIR}}` against an included file's variables produces
// "/config.env" and loads nothing, which is how nearly every namespaced task
// would silently run with no environment.
type dotenvSource struct {
	dir     string
	entries []string
	vars    map[string]chorefile.Var
}

func (r *Runner) dotenv(ctx context.Context, t *chorefile.Task, base *tmpl.Scope, callVars, argVars map[string]string, sh shell.Shell) (map[string]string, error) {
	var sources []dotenvSource
	// A task that declares `dotenv:` speaks for itself and inherits nothing —
	// including `dotenv: []`, which loads nothing at all. Rendered against its own
	// file's vars, like any other source.
	if t.Dotenv != nil {
		if len(t.Dotenv) == 0 {
			return map[string]string{}, nil
		}
		dir := r.Project.RootDir
		vars := map[string]chorefile.Var{}
		if t.File != nil {
			dir, vars = t.File.Dir, t.File.Vars
		}
		return r.loadDotenv(ctx, []dotenvSource{{dir: dir, entries: t.Dotenv, vars: vars}}, base, callVars, argVars, sh)
	}
	// The root's dotenv applies to every task, including those in included files.
	//
	// Tried the other way — each file answering only for its own `dotenv:` — and it
	// broke the stack: an include maps `POSTGRES_IP: '{{.MAIL3_POSTGRES_IP}}'`, and
	// that name comes from the root's config.env. A child's MAPPING therefore
	// depends on the PARENT's dotenv, so scoping dotenv per file would need it
	// resolved per level of the chain, not merely per task. Until then the root's
	// applies throughout, and a task that must not require it says so with
	// `dotenv: []` — which is what a hand-off to a peer project does.
	if root := r.Project.Root; root != nil {
		sources = append(sources, dotenvSource{dir: root.Dir, entries: root.Dotenv, vars: root.Vars})
	}
	if t.File != nil && (r.Project.Root == nil || t.File.Path != r.Project.Root.Path) {
		sources = append(sources, dotenvSource{dir: t.File.Dir, entries: t.File.Dotenv, vars: t.File.Vars})
	}
	return r.loadDotenv(ctx, sources, base, callVars, argVars, sh)
}

// loadDotenv reads one or more declarations, reporting a miss rather than
// silently continuing with defaults.
func (r *Runner) loadDotenv(ctx context.Context, sources []dotenvSource, base *tmpl.Scope, callVars, argVars map[string]string, sh shell.Shell) (map[string]string, error) {
	out := map[string]string{}
	declared, loaded := 0, 0
	var missing []string

	for _, src := range sources {
		if len(src.entries) == 0 {
			continue
		}
		// The caller still outranks the file: an argument selects the config, and
		// the file only says how to spell the path.
		fileVars, err := base.Push(callVars).Push(argVars).Resolve(ctx, src.vars, sh)
		if err != nil {
			return nil, fmt.Errorf("dotenv vars: %w", err)
		}
		scope := base.Push(fileVars).Push(callVars).Push(argVars)
		dir := src.dir

		for _, e := range src.entries {
			optional := strings.HasPrefix(e, "?")
			e = strings.TrimPrefix(e, "?")
			path, err := scope.Render(e)
			if err != nil {
				return nil, fmt.Errorf("dotenv path %q: %w", e, err)
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			if !optional {
				declared++
			}
			vars, err := parseDotenv(path)
			if err != nil {
				if os.IsNotExist(err) {
					if !optional {
						missing = append(missing, path)
					}
					continue
				}
				return nil, err
			}
			loaded++
			for k, v := range vars {
				out[k] = v
			}
		}
	}

	// The property worth guaranteeing is that a task never runs against a config
	// with NO environment at all — that is how every value silently becomes a
	// default, container names resolve to "-suffix", and commands match nothing.
	// A single missing file among several is ordinary: secrets are frequently
	// absent by design. So: nothing loaded is fatal, a partial miss is reported.
	if declared > 0 && loaded == 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("no environment loaded — none of these exist: %s",
			strings.Join(missing, ", "))
	}
	for _, m := range missing {
		if r.warnOnce(m) {
			fmt.Fprintf(r.Err, "chore: %s does not exist, continuing without it (prefix the path with ? to silence this)\n", m)
		}
	}
	return out, nil
}

// warnOnce reports whether this is the first time path has been warned about.
func (r *Runner) warnOnce(path string) bool {
	r.warnMu.Lock()
	defer r.warnMu.Unlock()
	if r.warned[path] {
		return false
	}
	r.warned[path] = true
	return true
}

func (r *Runner) taskDir(t *chorefile.Task, scope *tmpl.Scope) (string, error) {
	// The ROOT directory, not the directory of the file the task is written in.
	//
	// A task in an included file runs where the project runs — that is what
	// go-task does, and it is why {{.TASKFILE_DIR}} exists at all: you ask for the
	// file's own directory when you actually want it. Using it as the working
	// directory instead meant every relative path in an included file silently
	// pointed one level down: in rest-mail `-v $(pwd):/app` bind-mounted tasks/ as
	// the application, so the api container restarted forever on "open .air.toml:
	// no such file or directory", and `docker build … -f Dockerfile .` beside it
	// had the wrong build context.
	//
	// An include that declares `dir:` is the exception, and the one case where
	// moving is intended. The loader records that in WorkDir, and only then.
	dir := r.Project.RootDir
	if t.File != nil && t.File.WorkDir != "" {
		dir = t.File.WorkDir
	}
	if t.Dir != "" {
		d, err := scope.Render(t.Dir)
		if err != nil {
			return "", fmt.Errorf("rendering dir: %w", err)
		}
		if filepath.IsAbs(d) {
			dir = d
		} else {
			dir = filepath.Join(dir, d)
		}
	}
	return dir, nil
}

// shell builds a shell whose environment carries the resolved variables, so a
// script can use $VAR as well as {{.VAR}} — the target project relies on that
// for things like `>$OUTPUT`.
func (r *Runner) shell(dir string, scope *tmpl.Scope) shell.Shell {
	// Identify the runner, so a Taskfile can tell which one is executing it. The
	// concrete need: guards written to catch Task's "CLI variables do not reach
	// dotenv" trap must not fire here, where the trap does not exist and the
	// invocation they reject is the correct one.
	env := append(os.Environ(), "CHORE=1")
	// CHORE_BIN is THIS binary's path, so a task that has to drive another project
	// recurses with the runner it was launched with. Without it a task shells out to
	// whatever `chore` is first on PATH — the Homebrew build while you are testing a
	// local one, which is how a fix appears not to work.
	if exe, err := os.Executable(); err == nil {
		env = append(env, "CHORE_BIN="+exe)
	}
	for k, v := range scope.All() {
		if isEnvName(k) {
			env = append(env, k+"="+v)
		}
	}
	return shell.Shell{Dir: dir, Env: env, Out: r.Out, Err: r.Err}
}

func (r *Runner) silent(t *chorefile.Task) bool {
	if r.Verbose {
		return false
	}
	if t.Silent {
		return true
	}
	return t.File != nil && t.File.Silent
}

func (r *Runner) cacheDir() string { return filepath.Join(r.Project.RootDir, ".chore") }

// onceEntry is one `run: once` task's single execution, shared by every caller
// that asks for it.
//
// sync.Once, rather than a "have I run this?" flag, is what makes the guarantee
// hold: deps run concurrently, so two of them reach the same task before either
// has finished it, and a flag recorded only after execution would let both
// start. The second caller waits for the first and takes its result.
type onceEntry struct {
	once sync.Once
	err  error
}

// onceEntry returns the shared record for key, creating it on first ask.
func (r *Runner) onceEntry(key string) *onceEntry {
	r.onceMu.Lock()
	defer r.onceMu.Unlock()
	e := r.once[key]
	if e == nil {
		e = &onceEntry{}
		r.once[key] = e
	}
	return e
}

func onceKey(name string, scope *tmpl.Scope) string {
	all := scope.All()
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		b.WriteString("\x1f")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(all[k])
	}
	return b.String()
}

func checkRequires(t *chorefile.Task, scope *tmpl.Scope) error {
	var missing []string
	for _, name := range t.Requires {
		if v, ok := scope.Get(name); !ok || v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("required variable(s) not set: %s", strings.Join(missing, ", "))
	}
	return nil
}

// skipPlatform reports whether the task declares platforms and none of them
// match the host. Entries are "os", "arch", or "os/arch".
func skipPlatform(t *chorefile.Task) (bool, string) {
	if len(t.Platforms) == 0 {
		return false, ""
	}
	for _, p := range t.Platforms {
		os_, arch, _ := strings.Cut(p, "/")
		switch {
		case os_ != "" && arch != "":
			if os_ == runtime.GOOS && arch == runtime.GOARCH {
				return false, ""
			}
		case os_ == runtime.GOOS || os_ == runtime.GOARCH:
			return false, ""
		}
	}
	return true, fmt.Sprintf("not for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// suggest offers near matches for an unknown task name, since a namespaced
// project has 150+ of them and a typo is the likeliest cause.
func (r *Runner) suggest(name string) string {
	var near []string
	for k := range r.Project.Tasks {
		if strings.Contains(k, name) || strings.Contains(name, k) {
			near = append(near, k)
		}
	}
	if len(near) == 0 {
		return ""
	}
	sort.Strings(near)
	if len(near) > 5 {
		near = near[:5]
	}
	return " (did you mean: " + strings.Join(near, ", ") + "?)"
}

func isEnvName(k string) bool {
	if k == "" {
		return false
	}
	for i, c := range k {
		switch {
		case c >= 'A' && c <= 'Z', c == '_':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// reference resolves a task name written INSIDE a taskfile — a `- task:` step or a
// `deps:` entry — to the name the project knows it by.
//
// A reference is relative to the file it is written in: `- task: deps` inside
// tasks/webmail.yml means webmail's own `deps`, never a root task that happens to
// share the name. Resolving these globally is why `chore instance:up` stopped with
// `no task "deps"` while go-task ran it.
//
// The prefix is applied even when the reference already contains a colon, which is
// what makes tasks/monitoring.yml work: it holds a task literally named
// "prometheus:up", and `- task: prometheus:up` written beside it means that one —
// monitoring:prometheus:up.
//
// A leading colon escapes to the root namespace, as in `- task: :build`. This is
// go-task's behaviour in each case, verified against it rather than assumed.
func reference(caller *chorefile.Task, name string) string {
	if strings.HasPrefix(name, ":") {
		return strings.TrimPrefix(name, ":")
	}
	if caller.File == nil || caller.File.Namespace == "" {
		return name
	}
	return caller.File.Namespace + ":" + name
}

// fileChain returns f and its ancestors, ROOT FIRST, so a walk resolves each
// include's mapping in the scope of the file that wrote it.
func fileChain(f *chorefile.File) []*chorefile.File {
	var up []*chorefile.File
	for cur := f; cur != nil; cur = cur.Parent {
		up = append(up, cur)
	}
	slices.Reverse(up)
	return up
}

// mergeStrings returns a copy of a with b's entries on top.
func mergeStrings(a, b map[string]string) map[string]string {
	if len(b) == 0 {
		return a
	}
	out := make(map[string]string, len(a)+len(b))
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}

// envFiles lists the files whose `env:` applies to a task, lowest priority first:
// the root file, then the task's own file when that is a different one. Deliberately
// NOT the whole include chain — a task's file and the root are the two scopes a
// reader can see from where the task is written.
func envFiles(p *chorefile.Project, t *chorefile.Task) []*chorefile.File {
	var files []*chorefile.File
	if p != nil && p.Root != nil && len(p.Root.Env) > 0 {
		files = append(files, p.Root)
	}
	if t.File != nil && t.File != p.Root && len(t.File.Env) > 0 {
		files = append(files, t.File)
	}
	return files
}

// ExitCode reports the process exit code an error should produce.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *shell.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}
