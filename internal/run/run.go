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
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/rest-mail/go-tsk/internal/fingerprint"
	"github.com/rest-mail/go-tsk/internal/shell"
	"github.com/rest-mail/go-tsk/internal/taskfile"
	"github.com/rest-mail/go-tsk/internal/tmpl"
)

// Runner executes tasks from a loaded project.
type Runner struct {
	Project *taskfile.Project
	Out     io.Writer
	Err     io.Writer

	DryRun  bool
	Force   bool
	Verbose bool
	CLIArgs string // everything after `--`

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
func New(p *taskfile.Project, out, errOut io.Writer) *Runner {
	return &Runner{
		Project: p,
		Out:     out,
		Err:     errOut,
		once:    map[string]*onceEntry{},
		warned:  map[string]bool{},
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

func (r *Runner) execute(ctx context.Context, t *taskfile.Task, scope *tmpl.Scope) error {
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
	var deferred []taskfile.Cmd
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

	for i := len(deferred) - 1; i >= 0; i-- {
		// A deferred step runs even after a failure, and its own failure must not
		// hide why the task failed in the first place.
		if err := r.command(ctx, t, scope, sh, deferred[i]); err != nil {
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

// deps runs a task's dependencies concurrently. The first failure cancels the
// rest, and its error is what the task reports.
func (r *Runner) deps(ctx context.Context, t *taskfile.Task, scope *tmpl.Scope) error {
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
func (r *Runner) command(ctx context.Context, t *taskfile.Task, scope *tmpl.Scope, sh shell.Shell, c taskfile.Cmd) error {
	if c.Task != "" {
		name, err := scope.Render(c.Task)
		if err != nil {
			return fmt.Errorf("%s: cmd task name: %w", t.Name, err)
		}
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
func (r *Runner) scope(ctx context.Context, t *taskfile.Task, args []string, callVars map[string]string) (*tmpl.Scope, error) {
	base := tmpl.New(os.Environ())
	base.Set("ROOT_DIR", r.Project.RootDir)
	base.Set("TASK", t.Name)
	base.Set("CLI_ARGS", r.CLIArgs)
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
	early := base.Push(argVars).Push(callVars)

	sh := r.shell(r.Project.RootDir, early)

	fileVars := map[string]string{}
	if t.File != nil {
		fileVars, err = early.Resolve(ctx, t.File.Vars, sh)
		if err != nil {
			return nil, fmt.Errorf("file vars: %w", err)
		}
	}

	// A declared parameter's DEFAULT has to be available before the dotenv path is
	// rendered, because the path is usually keyed on that very parameter
	// (dotenv: ['config/{{.CONFIG}}/config.env']). Task vars as a whole cannot be
	// resolved this early — they are allowed to read dotenv values — so resolve
	// just the parameter defaults, and only those the caller did not supply.
	paramDefaults, err := early.Push(fileVars).Resolve(ctx, parameterVars(t, argVars, callVars), sh)
	if err != nil {
		return nil, fmt.Errorf("parameter defaults: %w", err)
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
	dotenvVars, err := r.dotenv(t, base.Push(fileVars).Push(paramDefaults).Push(callVars).Push(argVars))
	if err != nil {
		return nil, err
	}

	scope := base.Push(dotenvVars).Push(fileVars).Push(paramDefaults)
	taskVars, err := scope.Push(argVars).Push(callVars).Resolve(ctx, t.Vars, sh)
	if err != nil {
		return nil, fmt.Errorf("task vars: %w", err)
	}
	final := scope.Push(taskVars).Push(callVars).Push(argVars)

	// A parameter's DEFAULT comes from vars, which the author writes in one case
	// only. Mirror it to the other so `args: [config]` with `vars: {config: x}`
	// still answers to {{.CONFIG}}, exactly as a supplied argument does — the
	// alternative is a value that works when passed and vanishes when defaulted.
	for _, name := range t.Args {
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
func declaresDefault(t *taskfile.Task, name string) bool {
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
// `tsk up mail4.test --config restmail.test`. One of the two was going to be
// ignored, and picking a winner silently is how you act on a config you did not
// mean to name.
func checkArgConflicts(t *taskfile.Task, argVars, callVars map[string]string) error {
	for _, name := range t.Args {
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
func parameterVars(t *taskfile.Task, supplied ...map[string]string) map[string]taskfile.Var {
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
	out := map[string]taskfile.Var{}
	for _, name := range t.Args {
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
func bindArgs(t *taskfile.Task, args []string) (map[string]string, error) {
	out := map[string]string{}
	if len(args) > len(t.Args) {
		if len(t.Args) == 0 {
			return nil, fmt.Errorf("task %s takes no arguments, got %d (%s)",
				t.Name, len(args), strings.Join(args, " "))
		}
		return nil, fmt.Errorf("task %s takes %d argument(s) (%s), got %d",
			t.Name, len(t.Args), strings.Join(t.Args, ", "), len(args))
	}
	for i, a := range args {
		name := t.Args[i]
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
func checkArgs(t *taskfile.Task, args []string, callVars map[string]string, scope *tmpl.Scope) error {
	var missing []string

	for i, name := range t.Args {
		if i < len(args) || suppliedByName(name, callVars) {
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
		return fmt.Errorf("needs argument(s) %s — pass positionally (tsk %s <%s>), as %s=value, or give it a default in vars",
			strings.Join(missing, ", "), t.Name, missing[0], strings.ToUpper(missing[0]))
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
func (r *Runner) dotenv(t *taskfile.Task, scope *tmpl.Scope) (map[string]string, error) {
	files := map[string][]string{}
	if root := r.Project.Root; root != nil {
		files[root.Dir] = append(files[root.Dir], root.Dotenv...)
	}
	if t.File != nil && (r.Project.Root == nil || t.File.Path != r.Project.Root.Path) {
		files[t.File.Dir] = append(files[t.File.Dir], t.File.Dotenv...)
	}

	out := map[string]string{}
	declared, loaded := 0, 0
	var missing []string

	for dir, entries := range files {
		for _, e := range entries {
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
			fmt.Fprintf(r.Err, "tsk: %s does not exist, continuing without it (prefix the path with ? to silence this)\n", m)
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

func (r *Runner) taskDir(t *taskfile.Task, scope *tmpl.Scope) (string, error) {
	dir := r.Project.RootDir
	if t.File != nil && t.File.Dir != "" {
		dir = t.File.Dir
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
	env := os.Environ()
	for k, v := range scope.All() {
		if isEnvName(k) {
			env = append(env, k+"="+v)
		}
	}
	return shell.Shell{Dir: dir, Env: env, Out: r.Out, Err: r.Err}
}

func (r *Runner) silent(t *taskfile.Task) bool {
	if r.Verbose {
		return false
	}
	if t.Silent {
		return true
	}
	return t.File != nil && t.File.Silent
}

func (r *Runner) cacheDir() string { return filepath.Join(r.Project.RootDir, ".tsk") }

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

func checkRequires(t *taskfile.Task, scope *tmpl.Scope) error {
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
func skipPlatform(t *taskfile.Task) (bool, string) {
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
