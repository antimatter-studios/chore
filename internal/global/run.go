package global

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Runner runs global tasks. It holds the streams and the dialer rather than
// reaching for os.Stdout, so a test can drive the whole path — a real ssh server,
// a real agent — and read what a person would have seen.
type Runner struct {
	Out    io.Writer
	Err    io.Writer
	In     *os.File
	Dialer Dialer
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
	n := set.Namespaces[t.Namespace]
	route := n.Routes[t.Route]

	client, err := r.Dialer.Dial(ctx, t.Route, route)
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
	return Exec(ctx, client, t.Exec, r.In, r.Out, r.Err)
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
	return fmt.Sprintf("%s (route %s)", strings.Join(t.Exec, " "), t.Route)
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

// DryRun prints what a task would do without touching the network: the route it
// would travel, hop by hop, and the command or tunnel at the end of it.
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
	route := n.Routes[t.Route]

	fmt.Fprintf(w, "%s  (%s)\n", t.Address(), n.Path)
	fmt.Fprintf(w, "route %s:\n", t.Route)
	for i, hop := range route {
		from := "this machine"
		if i > 0 {
			from = "hop " + fmt.Sprint(i) + " (" + route[i-1].Host + ")"
		}
		fmt.Fprintf(w, "  hop %d  %s@%s\n", i+1, hop.User, hop.Addr())
		fmt.Fprintf(w, "         dialled from %s\n", from)
	}
	if t.Forward != nil {
		fmt.Fprintf(w, "forward: %s on this machine -> %s at the far end\n", t.Forward.Local, t.Forward.Remote)
		return nil
	}
	fmt.Fprintf(w, "exec:    %s\n", quoteArgv(t.Exec))
	return nil
}
