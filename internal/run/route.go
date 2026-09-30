package run

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/global"
	"github.com/antimatter-studios/chore/internal/tmpl"
)

// The ssh half of a task: where its steps run when it names a `route:`, the
// tunnel it holds for a `forward:`, and the route details it hands to a local
// tool for `with_route:`. None of it is a separate kind of task — it is what the
// ONE runner does with those keys, for a task in any file. See
// `chore help routes`.

// remote is where a routed task's steps run: one connection for the whole task,
// one session per step.
type remote struct {
	client *ssh.Client
	route  string
	pty    bool
	// in is the task's terminal, and only when it asked for one — `interactive:`
	// or `pty:` — which is the rule a local step follows too.
	in io.Reader
	// exports is `export NAME='value' …; `, carrying the task's declared
	// arguments and CLI_ARGS to the far end, so `$NAME` in a routed step reads
	// what `$NAME` reads in a local one.
	exports string
}

// run executes one rendered step at the far end.
func (rem *remote) run(ctx context.Context, script string, out, errOut io.Writer) error {
	command := rem.exports + script
	if rem.pty {
		return global.ExecPTY(ctx, rem.client, command, rem.in, out, errOut)
	}
	return global.Exec(ctx, rem.client, command, rem.in, out, errOut)
}

// connect resolves a task's route and dials it. The caller closes the client.
func (r *Runner) connect(ctx context.Context, t *chorefile.Task, scope *tmpl.Scope) (*remote, error) {
	resolved, err := r.resolveRoute(ctx, t.File, t.Route, scope)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Name, err)
	}
	if resolved.Why != "" && r.Verbose {
		fmt.Fprintf(r.Err, "chore: route %s -> %s [%s]\n", t.Route, resolved.Name, resolved.Why)
	}
	client, err := r.Dialer.Dial(ctx, resolved.Name, resolved.Hops)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Name, err)
	}
	rem := &remote{client: client, route: resolved.Name, pty: t.PTY, exports: remoteExports(t, scope)}
	if t.Interactive || t.PTY {
		rem.in = r.Stdin
	}
	return rem, nil
}

// remoteExports renders the declared arguments and CLI_ARGS as a prefix for a
// remote command. Only those, not the whole scope: the scope holds THIS
// machine's environment, and `$HOME` in a routed step is the far end's.
func remoteExports(t *chorefile.Task, scope *tmpl.Scope) string {
	var parts []string
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] || !chorefile.IsName(name) {
			return
		}
		if v, ok := scope.Get(name); ok {
			seen[name] = true
			parts = append(parts, name+"="+global.QuoteArgv([]string{v}))
		}
	}
	for _, a := range t.Args {
		add(a.Name)
		add(strings.ToUpper(a.Name))
	}
	add("CLI_ARGS")
	if len(parts) == 0 {
		return ""
	}
	return "export " + strings.Join(parts, " ") + "; "
}

// forward holds a task's tunnel open until the run is interrupted.
func (r *Runner) forward(ctx context.Context, t *chorefile.Task, scope *tmpl.Scope) error {
	fwd, err := expandForward(t, scope)
	if err != nil {
		return err
	}
	rem, err := r.connect(ctx, t, scope)
	if err != nil {
		return err
	}
	defer rem.client.Close()
	return global.RunForward(ctx, rem.client, fwd, func(addr string) {
		// The far end named as well as the near one, because the two are
		// commonly both loopback and mean different machines.
		fmt.Fprintf(r.Err, "chore: %s -> %s via route %s. Ctrl-C to stop.\n", addr, fwd.Remote, rem.route)
	})
}

func expandForward(t *chorefile.Task, scope *tmpl.Scope) (chorefile.Forward, error) {
	where := t.Name + ": forward"
	remote, err := global.Expand(where+" remote", t.Forward.Remote, scope.Get)
	if err != nil {
		return chorefile.Forward{}, err
	}
	local, err := global.Expand(where+" local", t.Forward.Local, scope.Get)
	if err != nil {
		return chorefile.Forward{}, err
	}
	return chorefile.Forward{Remote: remote, Local: local}, nil
}

// resolveRoute walks a route reference to actual hops, asking predicates on the
// way. A reference is read the way a task reference is: bare in the file it is
// written in, `global:<ns>:<route>` in global.d/<ns>.yaml. A selector's own
// branches and `if:` are read from the file the SELECTOR is in.
func (r *Runner) resolveRoute(ctx context.Context, from *chorefile.File, ref string, scope *tmpl.Scope) (global.Resolved, error) {
	var trail []string
	seen := map[string]bool{}
	f := from
	for {
		ns, name := chorefile.RouteRef(f, ref)
		owner := r.Project.Files[ns]
		var route chorefile.Route
		ok := false
		if owner != nil {
			route, ok = owner.Routes[name]
		}
		if !ok {
			return global.Resolved{}, fmt.Errorf("no route %q", ref)
		}
		display := routeName(from, ns, name)
		if !route.Conditional() {
			hops, err := global.PrepareHops(display, route.Hops, scope.Get)
			if err != nil {
				return global.Resolved{}, err
			}
			return global.Resolved{Name: display, Hops: hops, Why: strings.Join(trail, " ")}, nil
		}
		// Refused at load; this only keeps a mistake there from being a hang.
		if seen[ns+"|"+name] {
			return global.Resolved{}, fmt.Errorf("route %s leads back to itself", display)
		}
		seen[ns+"|"+name] = true

		taken, branch := route.Else, "else"
		if r.ask(ctx, owner, route.If) {
			taken, branch = route.Then, "then"
		}
		trail = append(trail, fmt.Sprintf("%s: %s -> %s", display, branch, taken))
		f, ref = owner, taken
	}
}

// routeName is a route as the task's own file would write it: `pi` for its
// own, `global:homelab:pi` for one in another file. It is what CHORE_ROUTE says
// and what an error names, so a route keeps the name its author gave it.
func routeName(from *chorefile.File, ns, name string) string {
	if from != nil && ns == from.Namespace || ns == "" {
		return name
	}
	return ns + ":" + name
}

// predicate is one `if:` task's answer, asked at most once per run.
type predicate struct {
	once   sync.Once
	answer bool
}

// ask runs a route's `if:` task and reports whether it succeeded.
//
// A predicate's non-zero exit is an ANSWER, not a failure: it is not reported,
// and it does not stop the run. It goes through Run like any task, so it gets
// its own `deps:` — the unlock a predicate needs before it can answer is an
// ordinary thing to want — and a `run: once` dependency it shares with the task
// it is choosing a route for runs once.
//
// The answer lasts one run: "am I on the LAN" is true until it is not.
func (r *Runner) ask(ctx context.Context, f *chorefile.File, ref string) bool {
	name := chorefile.Reference(f, ref)
	r.predMu.Lock()
	if r.preds == nil {
		r.preds = map[string]*predicate{}
	}
	p := r.preds[name]
	if p == nil {
		p = &predicate{}
		r.preds[name] = p
	}
	r.predMu.Unlock()
	p.once.Do(func() {
		p.answer = r.Run(context.WithValue(ctx, answeringKey{}, true), name, nil, nil) == nil
	})
	return p.answer
}

// answeringKey marks a context in which a failing step is an answer, so the
// failing-step report — which is for failures — stays quiet.
type answeringKey struct{}

func answering(ctx context.Context) bool {
	on, _ := ctx.Value(answeringKey{}).(bool)
	return on
}

// dryRoute prints what a routed task would travel, without touching the
// network or running a predicate: a predicate is an ordinary task and may
// change something, so `--dry` prints both branches rather than choosing.
//
// The hop list is the part worth printing. Every hop after the first is dialled
// FROM THE HOP BEFORE IT, so a file with three `127.0.0.1`s in it is easy to
// write wrong and hard to read; this says which machine each one is dialled from.
func (r *Runner) dryRoute(t *chorefile.Task, scope *tmpl.Scope) {
	ref := t.Route
	if ref == "" {
		ref = t.WithRoute
	}
	r.printRoute(r.Out, t.File, ref, "", map[string]bool{}, scope)
	switch {
	case t.Forward != nil:
		fwd, err := expandForward(t, scope)
		if err != nil {
			fwd = *t.Forward
		}
		fmt.Fprintf(r.Out, "forward: %s on this machine -> %s at the far end\n", fwd.Local, fwd.Remote)
	case t.WithRoute != "":
		fmt.Fprintf(r.Out, "handed to the steps below, HERE, as CHORE_ROUTE_* from the route above\n")
	case t.PTY:
		fmt.Fprintln(r.Out, "pty:     allocated for interactive input")
	}
}

// printRoute prints one route, and for a selector BOTH branches as a tree.
//
// A route already printed is named rather than expanded again. Terminating is
// not the same as bounded: a chain of selectors whose branches converge doubles
// the output per link — eighteen of them printed 137MB before this.
func (r *Runner) printRoute(w io.Writer, from *chorefile.File, ref, indent string, seen map[string]bool, scope *tmpl.Scope) {
	ns, name := chorefile.RouteRef(from, ref)
	owner := r.Project.Files[ns]
	display := routeName(from, ns, name)
	var route chorefile.Route
	ok := false
	if owner != nil {
		route, ok = owner.Routes[name]
	}
	if !ok {
		fmt.Fprintf(w, "%sroute %s: not declared\n", indent, display)
		return
	}
	if seen[ns+"|"+name] {
		fmt.Fprintf(w, "%sroute %s: printed above\n", indent, display)
		return
	}
	seen[ns+"|"+name] = true
	if !route.Conditional() {
		fmt.Fprintf(w, "%sroute %s:\n", indent, display)
		hops, err := global.PrepareHops(display, route.Hops, scope.Get)
		if err != nil {
			fmt.Fprintf(w, "%s  %v\n", indent, err)
			hops = route.Hops
		}
		for i, hop := range hops {
			dialFrom := "this machine"
			if i > 0 {
				dialFrom = fmt.Sprintf("hop %d (%s)", i, hops[i-1].Host)
			}
			fmt.Fprintf(w, "%s  hop %d  %s@%s\n", indent, i+1, hop.User, hop.Addr())
			fmt.Fprintf(w, "%s         dialled from %s\n", indent, dialFrom)
		}
		return
	}
	desc := ""
	if p := r.Project.Tasks[chorefile.Reference(owner, route.If)]; p != nil && p.Desc != "" {
		// Naming the predicate buys a `desc:`; printing it is what collects on
		// that, since the reader is here to find out what is being asked.
		desc = " — " + p.Desc
	}
	fmt.Fprintf(w, "%sroute %s: if %s%s\n", indent, display, route.If, desc)
	for _, branch := range []struct{ key, to string }{{"then", route.Then}, {"else", route.Else}} {
		fmt.Fprintf(w, "%s  %s -> %s\n", indent, branch.key, branch.to)
		r.printRoute(w, owner, branch.to, indent+"    ", seen, scope)
	}
}

// applyExports reads `KEY=value` lines — with or without a leading `export`, and
// with or without quotes — and sets them for everything after this task in the
// run: os.Setenv, because the ssh agent dialler reads SSH_AUTH_SOCK from the
// process, and r.exported, because a task whose scope was built before this one
// ran would otherwise hand its steps the stale value.
//
// A line that is not an assignment is skipped: the tool printing them may say
// other things on stdout too, and a `KEY=value` parser that failed on prose
// would be a tool that could not be used.
func (r *Runner) applyExports(out string) {
	r.exportMu.Lock()
	defer r.exportMu.Unlock()
	if r.exported == nil {
		r.exported = map[string]string{}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok || !chorefile.IsName(key) {
			continue
		}
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		_ = os.Setenv(key, value)
		r.exported[key] = value
	}
}

func (r *Runner) exportedEnv() []string {
	r.exportMu.Lock()
	defer r.exportMu.Unlock()
	out := make([]string, 0, len(r.exported))
	for k, v := range r.exported {
		out = append(out, k+"="+v)
	}
	return out
}

// renderArgv turns an argv step into the one string a shell — here or at the
// far end — will split back into exactly these words.
//
// Each word's `$VAR` is expanded HERE first, since no shell will see the word
// unquoted to do it, and only outside `{{…}}`: a template's own `$x` is the
// template's. Then the word is rendered, so an argument's value is inserted
// verbatim — a `$` in what somebody typed is data, never a variable.
func renderArgv(t *chorefile.Task, scope *tmpl.Scope, argv []string) (string, error) {
	words := make([]string, len(argv))
	for i, w := range argv {
		expanded, err := expandOutsideTemplates(t.Name, w, scope)
		if err != nil {
			return "", err
		}
		if words[i], err = scope.Render(expanded); err != nil {
			return "", fmt.Errorf("%s: rendering argv: %w", t.Name, err)
		}
	}
	return global.QuoteArgv(words), nil
}

func expandOutsideTemplates(where, s string, scope *tmpl.Scope) (string, error) {
	var b strings.Builder
	for s != "" {
		open := strings.Index(s, "{{")
		if open < 0 {
			open = len(s)
		}
		plain, err := global.Expand(where, s[:open], scope.Get)
		if err != nil {
			return "", err
		}
		b.WriteString(plain)
		s = s[open:]
		if s == "" {
			break
		}
		end := strings.Index(s, "}}")
		if end < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:end+2])
		s = s[end+2:]
	}
	return b.String(), nil
}
