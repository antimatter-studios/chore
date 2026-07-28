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
	"path/filepath"
	"strings"

	"github.com/antimatter-studios/chore/internal/buildinfo"
	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/loader"
	"github.com/antimatter-studios/chore/internal/run"
	"github.com/antimatter-studios/chore/internal/ui"
)

// Version is the build's version string, set by main. Exported rather than
// linker-stamped here so the binary has one obvious place it comes from.
var Version = "dev"

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
	if opts.help {
		out.Raw(usage)
		return 0
	}
	if opts.version {
		info := buildinfo.Get(Version)
		// stdout stays EXACTLY the bare version: the Homebrew formula matches on
		// this output, and anything scripted reads the first thing it prints. The
		// context goes to stderr, where it cannot break a pipeline.
		out.Raw(info.Version + "\n")
		errUI.Detail([][2]string{
			{"commit", commitOf(info)},
			{"built", info.Go + " " + info.Platform},
			{"file", foundTaskfile(opts.file)},
		})
		return 0
	}

	// Say so when this is not the installed release — see ui.Banner.
	if info := buildinfo.Get(Version); info.Dev {
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
			out.Raw(buildinfo.Get(Version).Version + "\n")
			return 0
		}
	}

	if opts.list || len(rest) == 0 {
		out.List(listing(project))
		return 0
	}

	r := run.New(project, stdout, stderr)
	r.DryRun, r.Force, r.Verbose = opts.dry, opts.force, opts.verbose
	r.CLIArgs = strings.Join(opts.cliArgs, " ")

	// Words after the task name are its positional arguments, except NAME=value
	// and --param value for a parameter the task declares. All three are bound
	// before `dotenv:` is resolved, so `chore config:check CONFIG=mail4.test` acts
	// on mail4.test. Task accepts the NAME=value syntax but resolves dotenv while
	// parsing, before CLI variables exist, so it silently used the default.
	args, callVars, err := splitArgs(rest[1:], declaredParams(project, rest[0]))
	if err != nil {
		errUI.Errorf("%v", err)
		return 2
	}

	// The same NAME=value pairs are ALSO global for the run, so they survive into
	// the tasks this one calls — `chore down CONFIG=mail1` has to reach the
	// `- task: postgres:down` inside `down`.
	r.CLIVars = callVars

	if err := r.Run(context.Background(), rest[0], args, callVars); err != nil {
		errUI.Errorf("%v", err)
		return run.ExitCode(err)
	}
	return 0
}

type options struct {
	dir, file                                         string
	list, dry, force, verbose, help, version, noColor bool
	cliArgs                                           []string
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
		out[strings.ToLower(a.Name)] = param{name: a.Name, boolean: a.IsBool()}
	}
	return out
}

// param is a declared parameter as the parser needs to see it: what to call it,
// and whether it takes a value.
type param struct {
	name    string
	boolean bool
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
func splitArgs(words []string, params map[string]param) ([]string, map[string]string, error) {
	var args []string
	vars := map[string]string{}

	for i := 0; i < len(words); i++ {
		w := words[i]

		if strings.HasPrefix(w, "--") && len(w) > 2 {
			name, value, hasValue := strings.Cut(w[2:], "=")
			if declared, ok := params[strings.ToLower(name)]; ok {
				switch {
				case declared.boolean:
					// Presence is the value. Crucially it must not eat the next
					// word: `chore logs --follow api` leaves api as a positional.
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

		if name, value, ok := strings.Cut(w, "="); ok && isVarName(name) {
			vars[name] = value
			// If it names a declared parameter, set the declared spelling too, so
			// {{.config}} and {{.CONFIG}} cannot disagree inside one task — a
			// supplied value must beat the default in BOTH cases.
			if declared, isParam := params[strings.ToLower(name)]; isParam {
				if declared.boolean {
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
