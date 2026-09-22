package global

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Runner runs global tasks. It holds the streams and the dialer rather than
// reaching for os.Stdout, so a test can drive the whole path — a real ssh server,
// a real agent — and read what a person would have seen.
type Runner struct {
	// Verbose reports the branch a conditional route took, for the same reason
	// --dry prints it: a predicate always taking one branch looks like working.
	Verbose bool
	Out     io.Writer
	Err     io.Writer
	In      *os.File
	Dialer  Dialer
}

// predicate returns the function Resolve asks for a branch.
//
// A predicate's non-zero exit is an ANSWER, not a failure: it is not reported,
// its status is not propagated, and it does not stop the run. That inversion is
// why this is a path of its own rather than an ordinary dependency.
//
// It goes through run rather than runLocal, so a predicate gets everything any
// other task gets — its own `deps:` above all. That is the capability this
// design was chosen for: a predicate that must unlock a vault before it can
// answer is an ordinary thing to want, and one whose dependency was skipped
// does not fail, it answers WRONGLY and sends the task down the slow route in
// silence.
//
// Both maps belong to the run rather than to the Runner. done is the one the
// dependency walk already keeps, shared so a task that is both a dependency and
// a predicate runs once — an unlock asked twice is a passphrase prompted twice.
// asked holds the answers, which done cannot: it records that a task ran, not
// how it ended.
func (r *Runner) predicate(set *Set, n *Namespace, done, asked map[string]bool) Predicate {
	return func(ctx context.Context, ref string) bool {
		t, err := set.dep(n, ref)
		if err != nil {
			// Load-time validation makes this unreachable; false is the safe
			// reading if it is ever reached anyway.
			return false
		}
		// Keyed by address, so a predicate named two ways is asked once — the
		// same rule the dependency memo follows.
		if answer, ok := asked[t.Address()]; ok {
			return answer
		}
		if done[t.Address()] {
			// Already run as a dependency, and a dependency that failed stops
			// the run — so reaching here means it succeeded.
			return true
		}
		done[t.Address()] = true
		answer := r.runWith(ctx, set, t, done, asked) == nil
		asked[t.Address()] = answer
		return answer
	}
}

// Run resolves an address, travels its route, and does the one thing the task
// says: run a command, or hold a tunnel open.
func (r *Runner) Run(ctx context.Context, set *Set, address string) error {
	t, err := set.Lookup(address)
	if err != nil {
		return err
	}
	// `internal: true` means the same here as it does in a project: callable as
	// part of something, not part of the surface a person types at. Refused at the
	// command line, which is the only way in for a global task today.
	if t.Internal {
		return fmt.Errorf("%s is internal: it is a helper inside %s, not a command to run", t.Address(), set.Namespaces[t.Namespace].Path)
	}
	// The root is marked before recursing, so an indirect cycle is a task seen
	// twice rather than a task RUN twice — which is what run's memo exists to
	// prevent and did not, since only dependencies were being marked.
	// Both maps last exactly one run. The answer to "am I on the LAN" is true
	// until it is not, so nothing here is kept on the Runner.
	return r.runWith(ctx, set, t, map[string]bool{t.Address(): true}, map[string]bool{})
}

// run executes one task and whatever it depends on, first and in order.
//
// `done` is what keeps a diamond from running the same dependency twice: two
// tasks both depending on `unlock` want it to have happened, not to have happened
// twice — and unlocking a vault twice would prompt twice.
func (r *Runner) run(ctx context.Context, set *Set, t *Task, done map[string]bool) error {
	return r.runWith(ctx, set, t, done, map[string]bool{})
}

func (r *Runner) runWith(ctx context.Context, set *Set, t *Task, done, asked map[string]bool) error {
	n := set.Namespaces[t.Namespace]
	for _, name := range t.Deps {
		// Resolved to an address BEFORE the memo is consulted: from inside a
		// namespace a task can be named either way, and keying on the spelling
		// would run one dependency twice.
		dep, err := set.dep(n, name)
		if err != nil {
			return fmt.Errorf("%s: dependency %q: %w", t.Address(), name, err)
		}
		if done[dep.Address()] {
			continue
		}
		done[dep.Address()] = true
		if err := r.runWith(ctx, set, dep, done, asked); err != nil {
			return fmt.Errorf("%s: dependency %q: %w", t.Address(), name, err)
		}
	}

	// No route means it runs here, and there is nothing to travel.
	if t.Route == "" {
		var extra []string
		if t.WithRoute != "" {
			resolved, err := n.Resolve(ctx, t.WithRoute, r.predicate(set, n, done, asked))
			if err != nil {
				return err
			}
			if resolved.Why != "" && r.Verbose {
				fmt.Fprintf(r.Err, "chore: route %s -> %s [%s]\n", t.WithRoute, resolved.Name, resolved.Why)
			}
			extra = resolved.Env()
		}
		return r.runLocal(ctx, t, extra)
	}

	resolved, err := n.Resolve(ctx, t.Route, r.predicate(set, n, done, asked))
	if err != nil {
		return err
	}
	client, err := r.Dialer.Dial(ctx, resolved.Name, resolved.Hops)
	if err != nil {
		return err
	}
	defer client.Close()

	if t.Forward != nil {
		return RunForward(ctx, client, *t.Forward, func(addr string) {
			// The far end named as well as the near one, because the two are
			// commonly both loopback and mean different machines.
			fmt.Fprintf(r.Err, "chore: %s -> %s via route %s. Ctrl-C to stop.\n", addr, t.Forward.Remote, t.Route)
		})
	}
	return Exec(ctx, client, t.Cmd, r.In, r.Out, r.Err)
}

// runLocal runs a task's argv on this machine.
//
// Directly, with no shell: it is a real argv here, since nothing in the way
// insists on flattening it to a string the way the SSH protocol does. So there is
// no quoting to get wrong and nothing to inject into.
//
// It gets chore's own stdin, because the step this exists for asks for a
// passphrase when nothing else has supplied one.
func (r *Runner) runLocal(ctx context.Context, t *Task, extra []string) error {
	cmd := localCommand(ctx, t.Cmd)
	if len(extra) > 0 {
		cmd.Env = append(os.Environ(), extra...)
	}
	cmd.Stdout, cmd.Stderr = r.Out, r.Err
	if r.In != nil {
		cmd.Stdin = r.In
	}
	// With `exports:` the stdout is the point, so it is captured rather than
	// shown. stderr is left streaming: that is where such tools put the messages
	// a person is meant to read, and swallowing those to harvest variables would
	// hide the one thing that explains a failure.
	var captured strings.Builder
	if t.Exports {
		cmd.Stdout = &captured
	}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &ExitError{Code: exitErr.ExitCode(), Err: err}
		}
		return fmt.Errorf("running %s: %w", t.Cmd, err)
	}
	if t.Exports {
		applyExports(captured.String())
	}
	return nil
}

// applyExports reads `KEY=value` lines — with or without a leading `export`, and
// with or without quotes — and sets them for the rest of this process.
//
// Process-wide rather than threaded through, because that is the scope the
// variables actually have: SSH_AUTH_SOCK is read by the agent dialler, and the
// point of absorbing it is that everything afterwards sees it. Anything that is
// not shaped like an assignment is ignored rather than guessed at.
func applyExports(out string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || !isEnvName(key) {
			continue
		}
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		_ = os.Setenv(key, value)
	}
}

// isEnvName keeps the parser from setting something that is not a variable name,
// so a line of prose that happens to contain an `=` is skipped rather than
// becoming an environment entry.
func isEnvName(s string) bool {
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return s != ""
}

// localCommand builds the process for a task with no route.
//
// The two forms of `cmd:` mean what they say on this side too: an argv is run
// directly, with no shell to quote for, and a shell line gets a shell — because
// a line that pipes or redirects has to.
func localCommand(ctx context.Context, c Cmd) *exec.Cmd {
	if len(c.Argv) > 0 {
		return exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	}
	return exec.CommandContext(ctx, shellBin(), "-c", c.Line)
}

// shellBin prefers bash and falls back to sh, matching internal/shell: a script
// written for one of chore's tasks should not mean something different here.
func shellBin() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "sh"
}

// ListNamespaces answers `chore global:` — what is installed on this machine.
func (r *Runner) ListNamespaces(set *Set) {
	if len(set.Namespaces) == 0 {
		fmt.Fprintf(r.Out, "no global namespaces in %s\n", set.Dir)
		fmt.Fprintf(r.Out, "\nA namespace is one file there, and its `name:` is what follows `global:`.\n")
		return
	}
	fmt.Fprintf(r.Out, "global namespaces in %s\n\n", set.Dir)
	for _, name := range sortedKeys(set.Namespaces) {
		n := set.Namespaces[name]
		fmt.Fprintf(r.Out, "  %-20s %d task(s), %d route(s)\n", "global:"+name+":", countVisible(n), len(n.Routes))
	}
	fmt.Fprintf(r.Out, "\nlist one with: chore global:%s:\n", sortedKeys(set.Namespaces)[0])
}

// ListTasks answers `chore global:homelab:` — what is in one namespace.
func (r *Runner) ListTasks(set *Set, name string) error {
	n, ok := set.Namespaces[name]
	if !ok {
		return fmt.Errorf("no global namespace %q in %s%s", name, set.Dir, set.suggestNamespace(name))
	}
	fmt.Fprintf(r.Out, "%s (%s)\n\n", "global:"+name, n.Path)

	var names []string
	for k, t := range n.Tasks {
		if !t.Internal {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintf(r.Out, "  no tasks\n")
		return nil
	}
	width := 0
	for _, k := range names {
		if len(k) > width {
			width = len(k)
		}
	}
	for _, k := range names {
		t := n.Tasks[k]
		fmt.Fprintf(r.Out, "  %-*s  %s\n", width, k, describe(t))
	}
	fmt.Fprintf(r.Out, "\nrun one with: chore global:%s:%s\n", name, names[0])
	return nil
}

// describe is a task's own `desc:`, or failing that what it will do — which for
// these is usually more informative than a sentence anyway.
func describe(t *Task) string {
	if t.Desc != "" {
		return t.Desc
	}
	if t.Forward != nil {
		return fmt.Sprintf("forward %s -> %s (route %s)", t.Forward.Local, t.Forward.Remote, t.Route)
	}
	if t.Route == "" {
		return t.Cmd.String() + " (here)"
	}
	return fmt.Sprintf("%s (route %s)", t.Cmd, t.Route)
}

func countVisible(n *Namespace) int {
	count := 0
	for _, t := range n.Tasks {
		if !t.Internal {
			count++
		}
	}
	return count
}

// printRoute prints one route, and for a selector prints BOTH branches as a
// tree rather than choosing between them.
//
// --dry resolves nothing, because a predicate is an ordinary task and may
// modify data: "nothing happens except the parts chore judged safe" is not a
// promise --dry can make. Which branch you would actually get is answered by
// running the predicate, which is a named task precisely so that it can be.
//
// A route already printed is named rather than expanded again. Terminating is
// not the same as bounded: a cycle is refused at load, so the walk ends, but a
// chain of selectors whose branches converge doubles the output per link —
// eighteen of them printed 137MB before this. Printing the same hop list twice
// was never worth anything anyway.
func printRoute(w io.Writer, n *Namespace, name, indent string, seen map[string]bool) {
	route, ok := n.Routes[name]
	if !ok {
		// Unreachable: every name here was validated at load. A guard, not a
		// case to handle.
		fmt.Fprintf(w, "%sroute %s: not declared in %s\n", indent, name, n.Path)
		return
	}
	if seen[name] {
		fmt.Fprintf(w, "%sroute %s: printed above\n", indent, name)
		return
	}
	seen[name] = true
	if !route.Conditional() {
		fmt.Fprintf(w, "%sroute %s:\n", indent, name)
		printHops(w, indent+"  ", route.Hops)
		return
	}
	desc := ""
	if p, ok := n.Tasks[route.If]; ok && p.Desc != "" {
		// Naming the predicate buys a `desc:`; printing it is what collects on
		// that, since the reader is here to find out what is being asked.
		desc = " — " + p.Desc
	}
	fmt.Fprintf(w, "%sroute %s: if %s%s\n", indent, name, route.If, desc)
	for _, branch := range []struct{ key, to string }{{"then", route.Then}, {"else", route.Else}} {
		fmt.Fprintf(w, "%s  %s -> %s\n", indent, branch.key, branch.to)
		printRoute(w, n, branch.to, indent+"    ", seen)
	}
}

// printHops says which machine each hop is dialled FROM, which is the part a
// reader most often gets wrong: every hop after the first is reached through
// the one before it, so a file with three 127.0.0.1s in it means three
// different machines.
func printHops(w io.Writer, indent string, hops []Hop) {
	for i, hop := range hops {
		from := "this machine"
		if i > 0 {
			from = "hop " + fmt.Sprint(i) + " (" + hops[i-1].Host + ")"
		}
		fmt.Fprintf(w, "%shop %d  %s@%s\n", indent, i+1, hop.User, hop.Addr())
		fmt.Fprintf(w, "%s       dialled from %s\n", indent, from)
	}
}

// DryRun prints what a task would do without touching the network: the route it
// would travel, hop by hop, and the command or tunnel at the end of it.
//
// It resolves nothing. A selector is printed as both of its branches, because
// choosing between them means running a predicate, and --dry runs nothing.
//
// The hop list is the part worth printing. Every hop after the first is resolved
// FROM THE HOP BEFORE IT, so a file with three `127.0.0.1`s in it is easy to
// write wrong and hard to read; this says which machine each one is dialled from
// before anything is dialled at all.
func (s *Set) DryRun(w io.Writer, address string) error {
	t, err := s.Lookup(address)
	if err != nil {
		return err
	}
	n := s.Namespaces[t.Namespace]

	// A task either travels a route or hands one's details to something here that
	// will travel it itself. Both want the same thing printed — which branch a
	// predicate chose, and where that leads.
	name := t.Route
	if name == "" {
		name = t.WithRoute
	}
	fmt.Fprintf(w, "%s  (%s)\n", t.Address(), n.Path)
	if name == "" {
		fmt.Fprintf(w, "runs here\n")
		fmt.Fprintf(w, "cmd:     %s\n", t.Cmd)
		return nil
	}
	printRoute(w, n, name, "", map[string]bool{})

	if t.Forward != nil {
		fmt.Fprintf(w, "forward: %s on this machine -> %s at the far end\n", t.Forward.Local, t.Forward.Remote)
		return nil
	}
	if t.WithRoute != "" {
		// Not travelled by chore: handed over. Printing the variables is the
		// point, since they are what the child interpolates — but a selector has
		// no single answer to print without running its predicate.
		if n.Routes[name].Conditional() {
			fmt.Fprintf(w, "handed to a command HERE as CHORE_ROUTE_*, from whichever branch above applies\n")
		} else {
			resolved := Resolved{Name: name, Hops: n.Routes[name].Hops}
			fmt.Fprintf(w, "handed to a command HERE as:\n")
			for _, kv := range resolved.Env() {
				fmt.Fprintf(w, "         %s\n", kv)
			}
		}
	}
	fmt.Fprintf(w, "cmd:     %s\n", t.Cmd)
	return nil
}
