package chorefile

import (
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"

	"gopkg.in/yaml.v3"
)

// chore:manual routes
// title: Routes
// summary: running a task's steps over ssh, forwarding a port, and choosing a route
// aliases: ssh ssh-tasks remote hops forward tunnel with_route exports pty
// order: 10
//
// # Routes
//
// Any task, in any file, can run its steps on another machine. A file declares
// `routes:` — named paths of ssh hops — and a task names one with `route:`.
// Nothing else about the task changes: `args:`, `vars:`, `deps:`, hooks and
// `--dry` mean what they mean everywhere else.
//
// ```yaml
// routes:
//   pi:
//     - { host: s1.example.com, port: 10022, user: root }
//     - { host: 127.0.0.1, port: 2222, user: chris }
//
// tasks:
//   pods:
//     desc: pods in one namespace, or all of them
//     args: [namespace]
//     vars: { namespace: "" }
//     route: pi
//     cmd: kubectl get pods {{if .NAMESPACE}}-n {{.NAMESPACE}}{{else}}-A{{end}}
//
//   proxy:
//     desc: the cluster's API on this machine's 6443
//     route: pi
//     forward: { remote: 127.0.0.1:6443, local: 127.0.0.1:6443 }
//
//   shell:
//     desc: an interactive login shell on the homelab
//     route: pi
//     pty: true
//     cmd: [bash, -l]
// ```
//
// `cmd:` is a one-step `cmds:`. A step is a shell line, or an argv list, which
// chore quotes, so an argument containing a space or a quote arrives whole. The
// list form cannot pipe, and that is the only reason both exist.
//
// ## What runs where
//
// With `route:`, the task's `cmd:`/`cmds:` steps — and its `defer:` steps — run at
// the far end, one ssh session each over one connection. Everything else runs
// here: `deps:`, hooks, `status:`, `sources:`, and any `- task:` step, which runs
// wherever the task it names runs.
//
// Templates are rendered HERE, before the step travels, so `{{.NAMESPACE}}`
// is the argument you passed. A shell line's `$VAR` is expanded by the shell at
// the far end, so `$HOME` there is the far end's. The task's declared arguments,
// and `CLI_ARGS` when you passed `-- words`, are exported at the far end too, so
// `$NAMESPACE` works in a routed step the same as in a local one.
//
// In an argv step, `$VAR` is expanded here, from the task's variables and chore's
// environment, and an unset one is an error rather than an empty string. `$$` is
// a literal `$`. The same rule applies to hop hosts and users and to `forward:`
// addresses.
//
// ## Each hop is resolved FROM THE PREVIOUS HOP
//
// `127.0.0.1:2222` in the route above is not this machine's loopback — it is
// **s1's**, where a reverse tunnel lands on the homelab. One file can therefore
// hold several `127.0.0.1`s that mean different machines:
//
// ```
// routes.pi[1].host   127.0.0.1  ->  s1's loopback
// forward.remote      127.0.0.1  ->  the homelab's loopback
// forward.local       127.0.0.1  ->  the machine you are sitting at
// ```
//
// Which is why a failure names the route and the hop by number:
//
// ```
// chore: route pi, hop 2 (chris@127.0.0.1:2222): connection refused
// ```
//
// A hop with no `port:` dials 22, and one with no `user:` is your username, as
// ssh does.
//
// ## Rules
//
// - **A key is never handled by chore.** Authentication is your ssh-agent, over
//   `$SSH_AUTH_SOCK`. Host keys are checked against `~/.ssh/known_hosts`, with
//   the same refusal to continue when one has changed.
// - **`forward:` is the task's body.** It binds, prints the address, and holds
//   the terminal until Ctrl-C. A task with `forward:` has no steps.
// - **`pty: true` allocates a remote terminal**, for an interactive shell. The
//   local terminal goes into raw mode for the session and is restored on exit;
//   resizes are forwarded.
// - **`with_route:` travels nothing.** It resolves a route and hands it to the
//   task's LOCAL steps as `CHORE_ROUTE`, `CHORE_ROUTE_HOST`, `_PORT`, `_USER` and
//   `_JUMP` (the earlier hops, in ssh's `-J` form) — for a tool that does its own
//   ssh, such as Pulumi.
// - **`exports: true`** reads `KEY=value` lines (with or without `export`) from
//   the task's stdout and sets them for everything after it in this run — how an
//   unlock step hands the next one its `SSH_AUTH_SOCK`. stdout is captured;
//   stderr still streams. Refused with `route:`, where the variables would be set
//   at the far end.
//
// ## A route can be a choice
//
// ```yaml
// routes:
//   lan:  [ { host: 192.168.0.47, user: chris } ]
//   pi:   [ { host: s1.example.com, port: 10022, user: root } ]
//   home: { if: on-lan, then: lan, else: pi }
//
// tasks:
//   on-lan:
//     desc: this machine is on the home network
//     cmd: ifconfig | grep -q 'inet 192\.168\.0\.'
// ```
//
// **`if:` names a TASK**, and it is true when that task succeeds. The predicate
// has a `desc:`, can be run on its own to debug a route taking the wrong branch,
// and keeps shell out of the routing table. `else:` is required: the case this
// is for is two real answers, not an optional extra.
//
// A predicate runs on this machine — one with a `route:` is refused at load,
// since it would pay a connection to answer a question about here. It exiting
// non-zero is an ANSWER, not a failure: nothing is reported and `else:` is taken.
// Its answer is remembered for the rest of the run. A predicate that must not
// run twice in one invocation, like an unlock it depends on, wants `run: once`.
//
// **`--dry` resolves nothing.** A predicate is an ordinary task and may change
// something, so `--dry` prints both branches instead.
//
// ## Naming things in another file
//
// `route:`, `with_route:`, a selector's `then:`/`else:` and `if:`, `deps:` and
// `- task:` all read a name the same way: a bare name is in THIS file, `:name`
// is in the root file of this one's tree (an include reaching its project's
// route), and `global:<ns>:<name>` is in `global.d/<ns>.yaml`. So a project task
// can travel a route declared once for the machine:
//
// ```yaml
// tasks:
//   deploy:
//     route: global:homelab:pi
//     deps: [global:ssh:unlock]
//     cmd: ./deploy.sh {{.ENV}}
// ```

// Route is either an ordered list of hops, or a choice between two other routes:
//
//	routes:
//	  lan: [ { host: 192.168.0.47, user: chris } ]
//	  pi:
//	    - { host: s1.example.com, port: 10022, user: root }
//	    - { host: 127.0.0.1, port: 2222, user: chris }
//	  homelab: { if: on-lan, then: lan, else: pi }
//
// The first form: hop 1 is dialled from this machine and every later hop FROM THE
// PREVIOUS HOP, which is what lets a route end at a machine this one cannot
// address at all.
//
// # Why the predicate may not travel
//
// Measured on the route this was built for: 326, 360 and 377 ms per connection
// through the tunnel against 0.65 ms on the LAN — about 500x. Probing the far end
// would answer the question exactly and pay a timeout on EVERY connection; an
// interface check touches no network. So a predicate with a `route:` is refused.
type Route struct {
	// Hops is set for the list form.
	Hops []Hop
	// If, Then and Else are set for the selector form. If names a TASK whose exit
	// status is the answer; Then and Else name other routes. All three are read
	// like any other reference: bare in this file, `global:<ns>:` in another.
	If   string
	Then string
	Else string
}

// Conditional reports which form this is.
func (r Route) Conditional() bool { return r.If != "" }

// UnmarshalYAML accepts either form.
func (r *Route) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		if err := checkNoNullElements(n, "a route"); err != nil {
			return err
		}
		for _, h := range n.Content {
			if h.Kind == yaml.MappingNode {
				if err := knownFields(h, "a hop", "host", "port", "user"); err != nil {
					return err
				}
			}
		}
		return n.Decode(&r.Hops)
	case yaml.MappingNode:
		// Checked by hand: a typo would otherwise be dropped and the route would
		// stop being a selector, surfacing as "has no hops" and naming neither the
		// key nor the mistake.
		if err := knownFields(n, "a route", "if", "then", "else"); err != nil {
			return err
		}
		var raw struct {
			If   string `yaml:"if"`
			Then string `yaml:"then"`
			Else string `yaml:"else"`
		}
		if err := n.Decode(&raw); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		r.If, r.Then, r.Else = raw.If, raw.Then, raw.Else
		if r.If == "" {
			return fmt.Errorf("line %d: a route is a list of hops, or `if:`/`then:`/`else:` — this one has no `if:`", n.Line)
		}
		return nil
	default:
		return fmt.Errorf("line %d: a route is a list of hops, or `if:`/`then:`/`else:`", n.Line)
	}
}

// Hop is one machine on the way.
type Hop struct {
	Host string `yaml:"host"`
	// Port defaults to 22, as ssh does.
	Port int `yaml:"port"`
	// User defaults to the local username, as ssh does.
	User string `yaml:"user"`
}

// Addr is the hop's dial address — resolved by whoever is dialling, which for
// every hop after the first is the machine before it.
func (h Hop) Addr() string { return net.JoinHostPort(h.Host, strconv.Itoa(h.Port)) }

// Forward is a tunnel: a port at the far end, bound here.
//
// Spelled out rather than shortened because a route commonly has more than one
// 127.0.0.1 in it meaning different machines — `remote` is resolved at the end
// of the route, `local` on the machine you are sitting at.
type Forward struct {
	Remote string `yaml:"remote"`
	Local  string `yaml:"local"`
}

// validateRoutes checks what one file can check about its own routes and the
// tasks that use them. References into another file — `global:<ns>:…` — are
// checked by the loader once every file is in.
func validateRoutes(f *File) error {
	for _, name := range slices.Sorted(maps.Keys(f.Routes)) {
		route := f.Routes[name]
		if route.Conditional() {
			for _, branch := range []struct{ key, value string }{{"then", route.Then}, {"else", route.Else}} {
				if branch.value == "" {
					return fmt.Errorf("taskfile: route %q has `if:` but no `%s:` — a choice needs both branches,"+
						" because the other one is a different route rather than doing nothing", name, branch.key)
				}
				if err := localRoute(f, branch.value); err != nil {
					return fmt.Errorf("taskfile: route %q names %q as its `%s:`: %w", name, branch.value, branch.key, err)
				}
			}
			continue
		}
		if len(route.Hops) == 0 {
			return fmt.Errorf("taskfile: route %q has no hops — a route is at least one machine to reach", name)
		}
		for i, h := range route.Hops {
			if h.Host == "" {
				return fmt.Errorf("taskfile: route %q, hop %d: needs a `host:`", name, i+1)
			}
			if h.Port < 0 || h.Port > 65535 {
				return fmt.Errorf("taskfile: route %q, hop %d: port %d is not a port", name, i+1, h.Port)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(f.Tasks)) {
		if err := validateTaskRoute(f, name, f.Tasks[name]); err != nil {
			return err
		}
	}
	return nil
}

func validateTaskRoute(f *File, name string, t *Task) error {
	hasSteps := len(t.Cmds) > 0
	if t.Forward != nil {
		if hasSteps {
			return fmt.Errorf("taskfile: task %q sets both steps and `forward:` —"+
				" a task either runs commands or holds a tunnel open", name)
		}
		if t.Route == "" {
			return fmt.Errorf("taskfile: task %q forwards a port but names no `route:` — there is nowhere to forward from", name)
		}
		for _, side := range []struct{ what, addr string }{{"remote", t.Forward.Remote}, {"local", t.Forward.Local}} {
			if side.addr == "" {
				return fmt.Errorf("taskfile: task %q: forward needs `%s:` as host:port", name, side.what)
			}
			// An address with a variable in it is checked once it is expanded.
			if !hasExpansion(side.addr) {
				if _, _, err := net.SplitHostPort(side.addr); err != nil {
					return fmt.Errorf("taskfile: task %q: forward %s is %q, which is not host:port", name, side.what, side.addr)
				}
			}
		}
	}
	if t.PTY && (t.Route == "" || !hasSteps) {
		return fmt.Errorf("taskfile: task %q sets `pty: true` but needs a routed `cmd:` — a PTY belongs to a remote command", name)
	}
	if t.WithRoute != "" && t.Route != "" {
		return fmt.Errorf("taskfile: task %q sets both `route:` and `with_route:` — one travels a route,"+
			" the other hands its details to something here that will travel it itself", name)
	}
	if t.Exports && t.Route != "" {
		return fmt.Errorf("taskfile: task %q sets `exports:` and a `route:` — the variables would be set at the far"+
			" end, inside a session that ends with the command, so nothing here could ever see them", name)
	}
	for _, ref := range []struct{ key, value string }{{"route", t.Route}, {"with_route", t.WithRoute}} {
		if ref.value == "" {
			continue
		}
		if err := localRoute(f, ref.value); err != nil {
			return fmt.Errorf("taskfile: task %q names `%s: %s`: %w", name, ref.key, ref.value, err)
		}
	}
	return nil
}

// localRoute checks a route reference that stays inside this file. Anything
// naming another file is left for the loader, which can see it.
func localRoute(f *File, ref string) error {
	if IsGlobalRef(ref) || len(ref) > 0 && ref[0] == ':' {
		return nil
	}
	if _, ok := f.Routes[ref]; ok {
		return nil
	}
	if len(f.Routes) == 0 {
		return fmt.Errorf("the file declares no routes")
	}
	return fmt.Errorf("not one of this file's routes: %s", joinSorted(f.Routes))
}

func hasExpansion(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '$' || (s[i] == '{' && i+1 < len(s) && s[i+1] == '{') {
			return true
		}
	}
	return false
}
