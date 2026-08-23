// Package cli is the command line: flags, positional arguments, and listing.
//
// The grammar is the one thing this program will not compromise on. A bare word
// after the task name is an ARGUMENT, never another task name — so
// `chore up mail4.test` means what it looks like. Running several tasks is
// `chore a && chore b`, which is what people type anyway; inheriting make's
// multi-target grammar is what costs every other runner its arguments.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/antimatter-studios/chore/internal/buildinfo"
	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/loader"
	"github.com/antimatter-studios/chore/internal/run"
	"github.com/antimatter-studios/chore/internal/ui"
)

// Version is the build's version string, set by main. Exported rather than
// linker-stamped here so the binary has one obvious place it comes from.
var Version = "dev"

// BuildDate is the commit date the release stamped in, empty for a build from
// source (where the toolchain's own vcs.time is used instead).
var BuildDate = ""

const usage = `chore — run tasks from a chores.yml

usage:
  chore [flags] <task> [args...] [-- extra]

flags:
  -C, --dir DIR     change to DIR before looking for the Taskfile
  -f, --file FILE   file to read (default: chores.yml, searched upward)
  -l, --list        list tasks with their descriptions
      --dry         print the commands a task would run, without running them
      --force       run even if up-to-date checks say the work is done
  -v, --verbose     echo commands even for silent tasks
      --no-color    plain output, no colour (also: NO_COLOR, or a non-terminal)
  -h, --help        this text
      --version     print the version

arguments:
  A task declares its parameters and receives them positionally:

      up:
        args: [config]
        cmds: ['docker compose --project-name {{.CONFIG}} up']

      chore up mail4.test

  Everything after -- is available as {{.CLI_ARGS}}.
`

// Main runs the command line and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	// One UI per destination: stdout can be a pipe while stderr is still a
	// terminal, so each decides for itself whether to style.
	out, errUI := ui.New(stdout), ui.New(stderr)

	opts, rest, err := parseFlags(args)
	if err != nil {
		errUI.Errorf("%v", err)
		errUI.Raw("\n" + usage)
		return 2
	}
	if opts.noColor {
		out.SetPlain(true)
		errUI.SetPlain(true)
	}
	// --help is global and answers about what the command line NAMES: the program
	// when no task is given, that task when one is. Recognised in either position,
	// since `chore --help up` and `chore up --help` are the same question.
	if opts.help && len(rest) == 0 {
		out.Raw(usage)
		return 0
	}
	if opts.version {
		info := buildinfo.Get(Version, BuildDate)
		// stdout stays EXACTLY the bare version: the Homebrew formula matches on
		// this output, and anything scripted reads the first thing it prints. The
		// context goes to stderr, where it cannot break a pipeline.
		out.Raw(info.Version + "\n")
		errUI.Detail([][2]string{
			{"commit", commitOf(info)},
			{"dated", dated(info.Date)},
			{"built", info.Go + " " + info.Platform},
			{"file", foundTaskfile(opts.file)},
		})
		return 0
	}

	// Say so when this is not the installed release — see ui.Banner.
	if info := buildinfo.Get(Version, BuildDate); info.Dev {
		errUI.Banner("chore", info.Version)
	}

	if opts.dir != "" {
		if err := os.Chdir(opts.dir); err != nil {
			errUI.Errorf("%v", err)
			return 1
		}
	}

	path, err := findTaskfile(opts.file)
	if err != nil {
		errUI.Errorf("%v", err)
		return 1
	}
	if base := filepath.Base(path); strings.EqualFold(base, "Taskfile.yml") || strings.EqualFold(base, "Taskfile.yaml") {
		errUI.Errorf("reading %s — rename it to %s; go-task ignores `args:` and mishandles `task <task> VAR=value`, so one file for both runners is a trap",
			base, Filenames[0])
	}

	project, err := loader.Load(path)
	if err != nil {
		errUI.Errorf("%v", err)
		return 1
	}

	// No task named, or an explicit --list: describe what is available. This is
	// the same answer, so `chore` on its own is never a mystery.
	if len(rest) == 1 && rest[0] == "version" {
		if _, ok := project.Tasks["version"]; !ok {
			out.Raw(buildinfo.Get(Version, BuildDate).Version + "\n")
			return 0
		}
	}

	if opts.list || len(rest) == 0 {
		out.List(listing(project))
		return 0
	}

	r := run.New(project, stdout, stderr)
	r.DryRun, r.Force, r.Verbose = opts.dry, opts.force, opts.verbose
	r.ChoreExe, r.ChoreVersion = choreExe(), buildinfo.Get(Version, BuildDate).Version
	r.NoLifecycle = opts.noLifecycle
	r.CLIArgs = strings.Join(opts.cliArgs, " ")

	// Words after the task name are its positional arguments, except NAME=value
	// and --param value for a parameter the task declares. All three are bound
	// before `dotenv:` is resolved, so `chore config:check CONFIG=mail4.test` acts
	// on mail4.test. Task accepts the NAME=value syntax but resolves dotenv while
	// parsing, before CLI variables exist, so it silently used the default.
	// `chore <task> --help` describes the TASK, and never runs it. Everything after
	// a task name is otherwise the task's data, so --help was binding as a
	// positional argument and `chore instance:up --help` STARTED a stack. A flag
	// that reads as "tell me about this" must never do anything. Text after `--` is
	// left alone, so a task that genuinely needs to pass --help along still can.
	if opts.help || wantsHelp(rest[1:]) {
		taskHelp(out, project, rest[0])
		return 0
	}

	// Checked here, not at load: --list and --help are inspection, and someone
	// staring at a version refusal needs to be able to read the file that caused
	// it. Everything that RUNS passes through this point.
	if err := checkChoreVersion(project, buildinfo.Get(Version, BuildDate)); err != nil {
		errUI.Errorf("%v", err)
		return 1
	}

	args, callVars, err := splitArgs(rest[1:], declaredParams(project, rest[0]))
	if err != nil {
		errUI.Errorf("%v", err)
		return 2
	}

	// The same NAME=value pairs are ALSO global for the run, so they survive into
	// the tasks this one calls — `chore down CONFIG=mail1` has to reach the
	// `- task: postgres:down` inside `down`.
	r.CLIVars = callVars

	ctx, stopSignals, interruptedBy := signalContext()
	defer stopSignals()

	if err := r.Invoke(ctx, rest[0], args, callVars); err != nil {
		// An interrupted run is not a failed one, and the error it produced is
		// "context canceled" — true and useless. Report the interrupt, and exit
		// 128+signal, which is what every shell reports for a signalled command
		// and what a caller checking $? already knows how to read.
		if sig := interruptedBy(); sig != 0 {
			verb := "interrupted"
			if sig == syscall.SIGTERM {
				verb = "terminated"
			}
			errUI.Errorf("%s: stopped %s and anything it started", verb, rest[0])
			return 128 + int(sig)
		}
		errUI.Errorf("%v", err)
		return run.ExitCode(err)
	}
	return 0
}

// signalContext returns a context cancelled by SIGINT or SIGTERM, a function to
// release the handler, and one reporting which signal arrived (0 if none).
//
// Without this, Ctrl-C left processes running. A task's script runs in its OWN
// process group (see internal/shell), so the terminal's SIGINT — which reaches
// only the foreground process group — never touches it; chore died instantly
// from the default action, and the Cancel hook that would have killed the
// group never fired because nothing ever cancelled the context. `chore app:run`
// exited while `flutter run` carried on.
//
// A SECOND signal is deliberately not caught: someone pressing Ctrl-C twice has
// stopped waiting for a tidy shutdown, and the default action takes over.
func signalContext() (context.Context, func(), func() syscall.Signal) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	// done releases the waiter when the run ends without a signal. Without it the
	// goroutine blocks on the channel forever — invisible in a CLI that is about
	// to exit, but Main is called hundreds of times in one test binary.
	var got atomic.Int32
	done := make(chan struct{})
	go func() {
		select {
		case s := <-ch:
			if sig, ok := s.(syscall.Signal); ok {
				got.Store(int32(sig))
			}
			// Stop catching: a SECOND Ctrl-C means the person has stopped waiting
			// for a tidy shutdown, and the default action should take over.
			signal.Stop(ch)
			cancel()
		case <-done:
		}
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			signal.Stop(ch)
			close(done)
			cancel()
		})
	}
	return ctx, stop, func() syscall.Signal { return syscall.Signal(got.Load()) }
}

type options struct {
	dir, file                                                      string
	list, dry, force, verbose, help, version, noColor, noLifecycle bool
	cliArgs                                                        []string
}

// parseFlags reads flags up to the first non-flag word, which is the task name.
// Anything after it belongs to the task, so `chore logs -f api` passes -f to the
// task rather than to chore. Anything after `--` becomes CLI_ARGS.
func parseFlags(args []string) (options, []string, error) {
	var o options
	var rest []string

	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			o.cliArgs = args[i+1:]
			return o, rest, nil
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		need := func() (string, error) {
			if hasValue {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag -%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		switch name {
		case "C", "dir":
			v, err := need()
			if err != nil {
				return o, nil, err
			}
			o.dir = v
		case "f", "file", "taskfile":
			v, err := need()
			if err != nil {
				return o, nil, err
			}
			o.file = v
		case "l", "list":
			o.list = true
		case "no-color", "no-colour":
			o.noColor = true
		case "dry", "dry-run":
			o.dry = true
		case "force":
			o.force = true
		case "v", "verbose":
			o.verbose = true
		case "no-lifecycle":
			o.noLifecycle = true
		case "h", "help":
			o.help = true
		case "version", "V":
			o.version = true
		default:
			return o, nil, fmt.Errorf("unknown flag %q", a)
		}
	}

	for ; i < len(args); i++ {
		if args[i] == "--" {
			o.cliArgs = args[i+1:]
			break
		}
		rest = append(rest, args[i])
	}
	return o, rest, nil
}

// choreExe is the path of the running binary, for {{.CHORE_EXE}}. It falls back to
// the bare name: on the rare platform where the path cannot be determined,
// letting PATH answer is better than interpolating an empty string, which would
// silently produce a command that runs the next word instead.
func choreExe() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "chore"
}

// checkChoreVersion enforces `chore_min_version`, which a file uses to say what
// it is written against.
//
// The need is real: a Taskfile's safety can rest on the RUNNER. One driving
// money declared every dangerous flag as a string compared to "true" purely
// because chore < 0.4.0 bound an unknown --flag positionally and let a bool take
// any value, so `--robot-name x` set an unrelated flag and spent a one-shot
// resource. With the floor stated, the file can drop the workaround.
//
// A dev build is exempt. It is built from source, has no version to compare, and
// already announces itself with a banner on every run — judging it against a
// floor would only stop chore from running its own Taskfile.
func checkChoreVersion(p *chorefile.Project, info buildinfo.Info) error {
	need, from := choreVersionFloor(p)
	if need == "" || info.Dev {
		return nil
	}
	want, _ := chorefile.ParseSemver(need) // decode rejected anything else
	have, ok := chorefile.ParseSemver(info.Version)
	if !ok {
		// Not a release and not flagged dev: nothing to compare, so refuse rather
		// than assume, the same way the rest of this program treats unknown.
		return fmt.Errorf("%s requires chore_min_version %s, and this build reports"+
			" %q, which is not a version — refusing rather than assuming", from, need, info.Version)
	}
	if chorefile.SemverLess(have, want) {
		return fmt.Errorf("chore %s is too old: %s requires chore_min_version %s.\n"+
			"  Upgrade with `brew upgrade chore`, or run an older copy of the file.",
			info.Version, from, need)
	}
	return nil
}

// choreVersionFloor returns the strictest floor any loaded file declares, and
// the file that declared it. Includes are covered: a floor belongs to the file
// that needs it, and the tasks it contributes are still run by this binary.
func choreVersionFloor(p *chorefile.Project) (need, from string) {
	var best [3]int
	consider := func(f *chorefile.File) {
		if f == nil || f.ChoreMinVersion == "" {
			return
		}
		v, ok := chorefile.ParseSemver(f.ChoreMinVersion)
		if !ok || !chorefile.SemverLess(best, v) {
			return
		}
		best, need, from = v, f.ChoreMinVersion, f.Path
	}
	consider(p.Root)
	for _, t := range p.Tasks {
		consider(t.File)
	}
	return need, from
}

// declaredParams returns the parameter names a task declares, lowercased for
// matching. An unknown task yields none, so the runner reports the bad name
// rather than the parser complaining about its arguments.
func declaredParams(p *chorefile.Project, task string) map[string]param {
	t, ok := p.Tasks[task]
	if !ok {
		return nil
	}
	out := make(map[string]param, len(t.Args))
	for _, a := range t.Args {
		p := param{name: a.Name, boolean: a.IsBool()}
		out[paramKey(a.Name)] = p
		if a.Short != "" {
			// Keyed WITH its dash. A parameter name cannot contain one (it has to
			// be usable as {{.Name}}), so a short can never collide with a name,
			// and one map answers both lookups.
			out["-"+a.Short] = p
		}
	}
	return out
}

// param is a declared parameter as the parser needs to see it: what to call it,
// and whether it takes a value.
type param struct {
	name    string
	boolean bool
}

// flagSpelling renders a parameter name the way a caller types it: hyphens where
// the declaration must use underscores.
func flagSpelling(name string) string {
	return strings.ReplaceAll(name, "_", "-")
}

// paramKey normalises a parameter spelling for matching: case-folded, with `-`
// folded onto `_`.
//
// A declared name cannot contain a hyphen — it has to be a usable variable
// name, and the loader rejects `args: [train-bars]` for that reason — so a
// two-word parameter is always `train_bars` in the file. On the command line
// the convention is the opposite: nobody types --train_bars. Without this fold
// the underscore spelling is the only one that reaches the parameter, and
// --train-bars silently becomes a positional instead.
func paramKey(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "-", "_")
}

// splitArgs sorts the words after a task name into positional arguments and
// variables. Three forms are accepted:
//
//	chore up mail4.test           positional, in the order `args:` declares
//	chore up CONFIG=mail4.test    a variable, as Task spells it
//	chore up --config mail4.test  a named parameter, if the task declares `config`
//
// A flag is only consumed when it names a DECLARED parameter; anything else
// stays a positional word and reaches the task, so `chore logs -f api` still
// passes -f to the task rather than being rejected here.
//
// Nothing is rejected HERE, but a word left over that is spelled like a long
// flag is refused when it is bound (see bindArgs): reaching that point means it
// names no parameter the task has, which makes it a typo, and binding it anyway
// is silent — for a bool parameter NormalizeBool reads the flag's own text as
// "true".
func splitArgs(words []string, params map[string]param) ([]string, map[string]string, error) {
	var args []string
	vars := map[string]string{}

	for i := 0; i < len(words); i++ {
		w := words[i]

		if strings.HasPrefix(w, "--") && len(w) > 2 {
			name, value, hasValue := strings.Cut(w[2:], "=")
			if declared, ok := params[paramKey(name)]; ok {
				switch {
				case declared.boolean:
					// Presence is the value. Crucially it must not eat the next
					// word: `chore logs --follow api` leaves api as a positional.
					//
					// Checked before normalising, which is the only point the
					// text still exists: NormalizeBool would have turned
					// --live=maybe into "true" and nothing downstream could
					// tell it from --live=yes.
					if hasValue && !chorefile.BoolLiteral(value) {
						return nil, nil, fmt.Errorf(
							"--%s must be true or false, got %q", name, value)
					}
					value = chorefile.NormalizeBool(valueOr(value, hasValue, "true"))
				case !hasValue:
					if i+1 >= len(words) {
						return nil, nil, fmt.Errorf("--%s needs a value", name)
					}
					i++
					value = words[i]
				}
				setParam(vars, declared.name, value)
				continue
			}
		}

		// A short flag, but only one a parameter opted into with `short:`. An
		// undeclared single-dash word stays data, which is what keeps `chore logs
		// -f api` and `tar -xzf` working in files that declare no shorts.
		if strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && len(w) > 1 {
			letters, value, hasValue := strings.Cut(w[1:], "=")

			if len(letters) == 1 {
				if declared, ok := params["-"+letters]; ok {
					switch {
					case declared.boolean:
						if hasValue && !chorefile.BoolLiteral(value) {
							return nil, nil, fmt.Errorf("-%s must be true or false, got %q", letters, value)
						}
						value = chorefile.NormalizeBool(valueOr(value, hasValue, "true"))
					case !hasValue:
						if i+1 >= len(words) {
							return nil, nil, fmt.Errorf("-%s needs a value", letters)
						}
						i++
						value = words[i]
					}
					setParam(vars, declared.name, value)
					continue
				}
			} else if !hasValue {
				bundled, err := bundledShorts(letters, params)
				if err != nil {
					return nil, nil, err
				}
				if bundled != nil {
					for _, declared := range bundled {
						setParam(vars, declared.name, "true")
					}
					continue
				}
			}
		}

		if name, value, ok := strings.Cut(w, "="); ok && isVarName(name) {
			vars[name] = value
			// If it names a declared parameter, set the declared spelling too, so
			// {{.config}} and {{.CONFIG}} cannot disagree inside one task — a
			// supplied value must beat the default in BOTH cases.
			if declared, isParam := params[paramKey(name)]; isParam {
				if declared.boolean {
					if !chorefile.BoolLiteral(value) {
						return nil, nil, fmt.Errorf(
							"%s must be true or false, got %q", name, value)
					}
					value = chorefile.NormalizeBool(value)
				}
				setParam(vars, declared.name, value)
			}
			continue
		}
		args = append(args, w)
	}

	if len(vars) == 0 {
		return args, nil, nil
	}
	return args, vars, nil
}

// bundledShorts resolves `-abc` into the parameters those letters name.
//
// It returns nil (and no error) unless EVERY letter is a declared short, because
// a word chore does not recognise has to stay data — `tar -xzf archive` is not
// its business. When every letter is declared but one of them takes a value, the
// bundle is refused instead of guessed: `-abo file` could reasonably mean either
// `-a -b -o file` or an -o whose value is the letters after it, and picking one
// silently is how a flag ends up set to a filename.
func bundledShorts(letters string, params map[string]param) ([]param, error) {
	out := make([]param, 0, len(letters))
	for _, c := range letters {
		declared, ok := params["-"+string(c)]
		if !ok {
			return nil, nil
		}
		if !declared.boolean {
			return nil, fmt.Errorf(
				"-%s takes a value, so it cannot be bundled in -%s; write it on its own",
				string(c), letters)
		}
		out = append(out, declared)
	}
	return out, nil
}

// setParam records a parameter under the declared spelling and its uppercase
// form, so {{.config}} and {{.CONFIG}} cannot disagree.
func setParam(vars map[string]string, name, value string) {
	vars[name] = value
	if upper := strings.ToUpper(name); upper != name {
		vars[upper] = value
	}
}

func valueOr(value string, has bool, fallback string) string {
	if has {
		return value
	}
	return fallback
}

func isVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
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

// Filenames are the names looked for, in order.
//
// chores.yml rather than Taskfile.yml because the two runners are no longer
// interchangeable: chore reads `args:` and go-task ignores it, and go-task's
// silent mishandling of `task <t> CONFIG=x` is a trap a shared filename invites
// people to walk into. Taskfile.yml is still accepted last, so a repository can
// migrate without a flag day — with a notice, because a file that two programs
// might claim should not be ambiguous for long.
var Filenames = []string{"chores.yml", "chores.yaml", "Taskfile.yml", "Taskfile.yaml"}

// findTaskfile resolves the file to read, searching upward from the working
// directory so `chore` works from a subdirectory.
func findTaskfile(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(abs); err != nil {
			return "", err
		}
		return abs, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		for _, name := range Filenames {
			candidate := filepath.Join(dir, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s here or in any parent directory", Filenames[0])
		}
		dir = parent
	}
}

// listing groups the project's tasks by namespace for rendering, hiding internal
// ones and listing each canonical name once. The description is the task's
// `desc`, so there is nothing to keep in sync.
//
// Grouping only — how it LOOKS is internal/ui's business, including the padding.
// The width used to be computed here with %-*s, which counts bytes, so a task
// name containing anything outside ASCII pushed every later description out of
// column.
func listing(p *chorefile.Project) []ui.Group {
	groups := map[string][]ui.Task{}
	for name, t := range p.Tasks {
		if t.Internal || name != t.Name { // skip aliases: list the canonical name once
			continue
		}
		ns := "(root)"
		if i := strings.LastIndex(name, ":"); i >= 0 {
			ns = name[:i]
		}
		groups[ns] = append(groups[ns], ui.Task{Name: name, Desc: t.Desc})
	}

	out := make([]ui.Group, 0, len(groups))
	for ns, tasks := range groups {
		out = append(out, ui.Group{Name: ns, Tasks: tasks})
	}
	return out
}

// dated renders the commit date with its age, which is the question someone
// actually has: not "what is the timestamp" but "how old is this". The age is
// computed at RUN time — only the stamp has to be fixed for a rebuild to produce
// identical bytes.
func dated(iso string) string {
	if iso == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	return fmt.Sprintf("%s (%s)", t.Format("2006-01-02 15:04 MST"), age(time.Since(t)))
}

// age says roughly how long ago, in the largest unit that is still informative.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 60*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	default:
		return plural(int(d.Hours()/24/30), "month")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s ago", unit)
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}

// commitOf renders the revision for the --version block, marking a tree that had
// uncommitted changes.
//
// Empty — so Detail drops the row — in two cases: the build recorded no revision
// (a release, built with -buildvcs=false so that two builds of the same source are
// byte-identical, with the stamped version as its identity instead), or the
// version string already contains it, which is every dev build. Printing
// "dev+8e37399-dirty" and then "commit 8e37399 (uncommitted changes)" underneath
// says one thing twice.
func commitOf(i buildinfo.Info) string {
	if i.Commit == "" || strings.Contains(i.Version, i.Commit) {
		return ""
	}
	if i.Dirty {
		return i.Commit + " (uncommitted changes)"
	}
	return i.Commit
}

// foundTaskfile reports which file chore would read from here, so `--version` also
// answers "and is it even looking at the project I think it is". Not an error if
// there is none — asking for a version outside a project is normal.
func foundTaskfile(flag string) string {
	path, err := findTaskfile(flag)
	if err != nil {
		return ""
	}
	return path
}

// wantsHelp reports whether -h/--help appears among a task's words, before any `--`
// that hands the rest to the task verbatim.
func wantsHelp(words []string) bool {
	for _, w := range words {
		if w == "--" {
			return false
		}
		if w == "-h" || w == "--help" {
			return true
		}
	}
	return false
}

// hasDefault reports whether a parameter has a value to fall back on: in the task's
// own vars, or in the vars of the file it is written in, under either spelling.
func hasDefault(t *chorefile.Task, name string) bool {
	for _, vars := range []map[string]chorefile.Var{t.Vars, fileVars(t)} {
		for _, n := range []string{name, strings.ToUpper(name), strings.ToLower(name)} {
			if _, ok := vars[n]; ok {
				return true
			}
		}
	}
	return false
}

func fileVars(t *chorefile.Task) map[string]chorefile.Var {
	if t.File == nil {
		return nil
	}
	return t.File.Vars
}

// taskHelp prints what a task takes, from its own declarations — the desc, its
// parameters with their types and descriptions, and the ways it can be called.
func taskHelp(u *ui.UI, p *chorefile.Project, name string) {
	t, ok := p.Tasks[name]
	if !ok {
		u.Errorf("no task %q", name)
		return
	}
	u.Title("chore "+name, "")
	if t.Desc != "" {
		u.Dim("%s", "  "+t.Desc)
	}
	if len(t.Args) == 0 {
		u.Dim("\n  takes no arguments")
		return
	}

	rows := make([][2]string, 0, len(t.Args))
	for _, a := range t.Args {
		kind := a.Type
		if kind == "" {
			kind = "string"
		}
		// Whether a parameter is optional follows from the declaration rather than
		// from a marker: a default anywhere in scope makes it optional. The task's
		// own vars are not enough — this project defaults CONFIG at the FILE level,
		// so reading only t.Vars called it required while its description says
		// otherwise.
		req := "required"
		if hasDefault(t, a.Name) {
			req = "optional"
		}
		desc := a.Desc
		if desc == "" {
			desc = "(no description)"
		}
		label := a.Name
		if a.Short != "" {
			label += " (-" + a.Short + ")"
		}
		rows = append(rows, [2]string{label, fmt.Sprintf("%s, %s — %s", kind, req, desc)})
	}
	u.Raw("\n")
	u.Dim("  arguments:")
	u.Detail(rows)

	first := t.Args[0]
	// A bool is complete on its own, so showing it as `--flag <value>` taught the
	// one spelling that is now an error; and the flag form is hyphenated, which is
	// what a caller types even though the declaration cannot be.
	positional := fmt.Sprintf("chore %s <%s>", name, first.Name)
	flag := fmt.Sprintf("chore %s --%s <value>", name, flagSpelling(first.Name))
	if first.IsBool() {
		positional = fmt.Sprintf("chore %s <true|false>", name)
		flag = fmt.Sprintf("chore %s --%s", name, flagSpelling(first.Name))
	}
	forms := [][2]string{
		{"positional", positional},
		{"flag", flag},
	}
	if first.Short != "" {
		short := fmt.Sprintf("chore %s -%s <value>", name, first.Short)
		if first.IsBool() {
			short = fmt.Sprintf("chore %s -%s", name, first.Short)
		}
		forms = append(forms, [2]string{"short flag", short})
	}
	forms = append(forms,
		[2]string{"variable", fmt.Sprintf("chore %s %s=<value>", name, strings.ToUpper(first.Name))},
		[2]string{"passthrough", fmt.Sprintf("chore %s -- <words for {{.CLI_ARGS}}>", name)},
	)
	u.Raw("\n")
	u.Dim("  called as:")
	u.Detail(forms)
}
