// Package cli is the command line: flags, positional arguments, and listing.
//
// The grammar is the one thing this program will not compromise on. A bare word
// after the task name is an ARGUMENT, never another task name — so
// `tsk up mail4.test` means what it looks like. Running several tasks is
// `tsk a && tsk b`, which is what people type anyway; inheriting make's
// multi-target grammar is what costs every other runner its arguments.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rest-mail/go-tsk/internal/loader"
	"github.com/rest-mail/go-tsk/internal/run"
	"github.com/rest-mail/go-tsk/internal/taskfile"
)

const usage = `tsk — run tasks from a Taskfile.yml

usage:
  tsk [flags] <task> [args...] [-- extra]

flags:
  -C, --dir DIR     change to DIR before looking for the Taskfile
  -f, --file FILE   Taskfile to read (default: Taskfile.yml, searched upward)
  -l, --list        list tasks with their descriptions
      --dry         print the commands a task would run, without running them
      --force       run even if up-to-date checks say the work is done
  -v, --verbose     echo commands even for silent tasks
  -h, --help        this text

arguments:
  A task declares its parameters and receives them positionally:

      up:
        args: [config]
        cmds: ['docker compose --project-name {{.CONFIG}} up']

      tsk up mail4.test

  Everything after -- is available as {{.CLI_ARGS}}.
`

// Main runs the command line and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	opts, rest, err := parseFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "tsk: %v\n\n%s", err, usage)
		return 2
	}
	if opts.help {
		fmt.Fprint(stdout, usage)
		return 0
	}

	if opts.dir != "" {
		if err := os.Chdir(opts.dir); err != nil {
			fmt.Fprintf(stderr, "tsk: %v\n", err)
			return 1
		}
	}

	path, err := findTaskfile(opts.file)
	if err != nil {
		fmt.Fprintf(stderr, "tsk: %v\n", err)
		return 1
	}

	project, err := loader.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "tsk: %v\n", err)
		return 1
	}

	// No task named, or an explicit --list: describe what is available. This is
	// the same answer, so `tsk` on its own is never a mystery.
	if opts.list || len(rest) == 0 {
		writeList(stdout, project)
		return 0
	}

	r := run.New(project, stdout, stderr)
	r.DryRun, r.Force, r.Verbose = opts.dry, opts.force, opts.verbose
	r.CLIArgs = strings.Join(opts.cliArgs, " ")

	// Words after the task name are its positional arguments, except NAME=value
	// and --param value for a parameter the task declares. All three are bound
	// before `dotenv:` is resolved, so `tsk config:check CONFIG=mail4.test` acts
	// on mail4.test. Task accepts the NAME=value syntax but resolves dotenv while
	// parsing, before CLI variables exist, so it silently used the default.
	args, callVars, err := splitArgs(rest[1:], declaredParams(project, rest[0]))
	if err != nil {
		fmt.Fprintf(stderr, "tsk: %v\n", err)
		return 2
	}

	if err := r.Run(context.Background(), rest[0], args, callVars); err != nil {
		fmt.Fprintf(stderr, "tsk: %v\n", err)
		return run.ExitCode(err)
	}
	return 0
}

type options struct {
	dir, file                       string
	list, dry, force, verbose, help bool
	cliArgs                         []string
}

// parseFlags reads flags up to the first non-flag word, which is the task name.
// Anything after it belongs to the task, so `tsk logs -f api` passes -f to the
// task rather than to tsk. Anything after `--` becomes CLI_ARGS.
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
		case "dry", "dry-run":
			o.dry = true
		case "force":
			o.force = true
		case "v", "verbose":
			o.verbose = true
		case "h", "help":
			o.help = true
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
func declaredParams(p *taskfile.Project, task string) map[string]param {
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
//	tsk up mail4.test           positional, in the order `args:` declares
//	tsk up CONFIG=mail4.test    a variable, as Task spells it
//	tsk up --config mail4.test  a named parameter, if the task declares `config`
//
// A flag is only consumed when it names a DECLARED parameter; anything else
// stays a positional word and reaches the task, so `tsk logs -f api` still
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
					// word: `tsk logs --follow api` leaves api as a positional.
					value = taskfile.NormalizeBool(valueOr(value, hasValue, "true"))
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
					value = taskfile.NormalizeBool(value)
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

// findTaskfile resolves the Taskfile to read, searching upward from the working
// directory so `tsk` works from a subdirectory.
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
		for _, name := range []string{"Taskfile.yml", "Taskfile.yaml", "taskfile.yml"} {
			candidate := filepath.Join(dir, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no Taskfile.yml here or in any parent directory")
		}
		dir = parent
	}
}

// writeList prints the tasks, grouped by namespace, hiding internal ones. The
// description is the task's `desc`, so there is nothing to keep in sync.
func writeList(w io.Writer, p *taskfile.Project) {
	type entry struct{ name, desc string }
	groups := map[string][]entry{}
	width := 0
	for name, t := range p.Tasks {
		if t.Internal || name != t.Name { // skip aliases: list the canonical name once
			continue
		}
		ns := "(root)"
		if i := strings.LastIndex(name, ":"); i >= 0 {
			ns = name[:i]
		}
		groups[ns] = append(groups[ns], entry{name, t.Desc})
		if len(name) > width {
			width = len(name)
		}
	}
	if len(groups) == 0 {
		fmt.Fprintln(w, "no tasks")
		return
	}

	names := make([]string, 0, len(groups))
	for ns := range groups {
		names = append(names, ns)
	}
	sort.Strings(names)

	fmt.Fprintln(w, "tasks:")
	for _, ns := range names {
		es := groups[ns]
		sort.Slice(es, func(i, j int) bool { return es[i].name < es[j].name })
		fmt.Fprintf(w, "\n  [%s]\n", ns)
		for _, e := range es {
			if e.desc == "" {
				fmt.Fprintf(w, "    %s\n", e.name)
				continue
			}
			fmt.Fprintf(w, "    %-*s  %s\n", width, e.name, e.desc)
		}
	}
}
