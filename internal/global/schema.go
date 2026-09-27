// Package global is chore's machine-wide task surface: files in
// ~/.config/chore/global.d that describe things to run on OTHER machines,
// reachable from any directory.
//
// It is deliberately a separate world from a project's chores.yml, and the
// separation is the design rather than an accident of layout:
//
//   - A global task must work with no chores.yml anywhere. That is the whole
//     point — "check k3s from whatever machine I am sitting at" cannot depend on
//     standing in a particular directory.
//   - Its schema is a different shape. A project task runs a shell script here;
//     a global task names a ROUTE and an argv to run at the far end of it. Bolting
//     `route:`/`exec:` onto chorefile.Task would put fields on every task in every
//     project that can only ever mean something in one of them.
//
// What the two worlds share is the addressing: `global:homelab:k3s:pods` reads
// the same way `monitoring:prometheus:up` does, and the `global:` prefix is
// mandatory so that reading it tells you where it came from.
package global

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// chore:manual global
// title: Global tasks
// summary: machine-wide tasks that run over ssh, from any directory
// aliases: globals remote ssh routes homelab hops
// order: 10
//
// # Global tasks
//
// Tasks that belong to a MACHINE rather than to a project, kept in
// `~/.config/chore/global.d/*.yaml` — one namespace per file — and reachable
// from any directory, with or without a `chores.yml` in sight.
//
// ```
// chore global:                       the namespaces installed here
// chore global:homelab:               the tasks in one
// chore global:homelab:k3s:pods       run one
// ```
//
// `$XDG_CONFIG_HOME` is honoured when set, and `~/.config` is the fallback —
// which matters because the variable is unset on macOS by default, and these
// files are meant to arrive on both by the same dotfiles repository.
//
// ## The file
//
// ```yaml
// name: homelab
//
// routes:
//   pi:
//     - { host: s1.example.com, port: 10022, user: root }
//     - { host: 127.0.0.1, port: 2222, user: chris }
//
// tasks:
//   k3s:pods:
//     desc: pods across every namespace
//     route: pi
//     exec: [kubectl, get, pods, -A]
//
//   k3s:proxy:
//     desc: the cluster's API on this machine's 6443
//     route: pi
//     forward: { remote: 127.0.0.1:6443, local: 127.0.0.1:6443 }
// ```
//
// ## Each hop is resolved FROM THE PREVIOUS HOP
//
// This is the part worth reading twice. `127.0.0.1:2222` in the route above is
// not this machine's loopback — it is **s1's**, where a reverse tunnel already
// lands on the homelab. One file can therefore hold several `127.0.0.1`s that
// mean different machines:
//
// ```
// routes.pi[1].host   127.0.0.1  ->  s1's loopback
// forward.remote      127.0.0.1  ->  the homelab's loopback
// forward.local       127.0.0.1  ->  the machine you are sitting at
// ```
//
// Which is why a failure names the route and the hop by number rather than only
// the address it could not reach:
//
// ```
// chore: route pi, hop 2 (127.0.0.1:2222): connection refused
// ```
//
// It is also why the forwarding keys are `remote:` and `local:` rather than
// anything shorter.
//
// ## Rules
//
// - **`global:` is mandatory.** Not to resolve ambiguity — to make the call site
//   readable. `chore global:homelab:k3s:pods` says where it came from without the
//   reader knowing what is installed on that machine, and a project that defines
//   a `homelab:` namespace cannot silently shadow it. A README documenting the
//   command stays true on every machine.
// - **A task says where it goes.** There is no default route: a task with a
//   `route:` runs at the far end of it, and one without runs here.
// - **`exec:` and `forward:` are mutually exclusive**, by which key is present
//   rather than by a `mode:` field — so a task that is neither, or both, cannot be
//   written rather than merely being rejected.
// - **`exec:` is an argv list**, so chore does the shell quoting once, correctly,
//   instead of every task doing it. Note what that does NOT mean: the SSH protocol
//   carries a command as a single STRING which the far end hands to a login shell,
//   so quoting exists either way — chore just owns it.
// - **A key is never handled by chore.** Authentication is your ssh-agent, over
//   `$SSH_AUTH_SOCK`, so a secret manager keeps working without chore knowing it
//   exists. Host keys are checked against `~/.ssh/known_hosts`, the same file
//   `ssh` uses and with the same refusal to continue when one has changed.
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
//
//   pods: { route: home, cmd: [kubectl, get, pods, -A] }
// ```
//
// The same task often has two correct routes, and which one is correct depends
// on which network the laptop is on. `if:` picks between them.
//
// **`if:` names a TASK.** Not a shell line — there is no inline form. A command
// **succeeds** when it exits 0, and an `if:` is true when its task succeeds;
// that is the same word `ignore_error:`, `on_failure` and `status:` already
// mean. Naming the task buys four things: the question has a `desc:`, so a
// listing says what is being asked; it can be run on its own, which is how a
// route choosing the wrong branch gets debugged; nothing has to guess whether a
// string is a name or a script; and shell stays out of the routing table, which
// is read on machines its author is not sitting at. The cost is that a one-off
// predicate needs a named task, and naming it is usually the part worth keeping.
//
// **Do not mark a predicate `internal: true`.** It would still work, but
// `internal:` refuses a task from the command line, and running the predicate is
// the only way to find out which branch a route will take — `--dry` will not say,
// because it refuses to run anything. A predicate is a question with an answer,
// which is a reasonable thing to offer; a helper step like an unlock is not.
//
// **`else:` is required.** The case this exists for is not "do something extra"
// — it is that both answers are real. A missing branch would be a task that
// silently does nothing somewhere. It is also why no `not:` is needed: negation
// is swapping the two names.
//
// **A predicate may not have a `route:`.** Refused at load. See above on what a
// hop costs.
//
// **A predicate exiting non-zero has ANSWERED.** It is not a failed run,
// nothing is reported, and `else:` is taken. The answer is remembered for the
// rest of the run, so two routes sharing a predicate ask it once.
//
// **`--dry` resolves nothing.** A predicate is an ordinary task and may modify
// data, so "nothing happens except the parts chore judged safe" is not a
// promise `--dry` can make. It prints both branches instead, each reachable
// route indented under the branch that leads to it. To learn which branch you
// are on, run the predicate.
//
// ## Naming another namespace's task
//
// `deps:` and `if:` follow the rule the command line follows: a bare name is
// this file's task, and `global:<namespace>:<task>` is another's.
//
// ```yaml
// tasks:
//   pods:
//     route: pi
//     deps: [global:ssh:unlock]
//     cmd: [kubectl, get, pods, -A]
// ```
//
// That is what makes an unlock step a machine-wide capability rather than a
// copy in every file: what `global:ssh:unlock` does is the machine's business —
// trove here, 1Password or `keepassxc-cli` elsewhere — and the task that needs
// a key just says so.
//
// Checked once every namespace has loaded, since one file cannot see another.
// One dependency written both ways is one dependency. A task may not be NAMED
// with the `global:` prefix: it could never be reached, and it would make a
// `deps:` entry ambiguous.
//
// - **`forward:` holds the terminal.** It binds, prints the address, and stays up
//   until Ctrl-C. Daemonising would mean a stop verb, a registry, pid files, and a
//   story for a tunnel that died — a lot of machinery to save one terminal tab.

// Namespace is one file in global.d: a name, the routes it can travel, and the
// tasks that travel them.
type Namespace struct {
	// Name is the word between `global:` and the task's own name. Required, and
	// not inferred from the filename: one addressable thing should have one
	// spelling, in the file that defines it, rather than a name that changes when
	// somebody renames a file.
	Name string `yaml:"name"`
	// Routes are the paths to a machine, each a list of hops. Named, because a
	// task refers to one by name and a reader should be able to look it up.
	Routes map[string]Route `yaml:"routes"`
	// Tasks are what can be run over them, keyed by the name that follows the
	// namespace: `k3s:pods` is reached as `global:homelab:k3s:pods`.
	Tasks map[string]*Task `yaml:"tasks"`

	// Path is the file this came from, filled in by the loader. Kept so an error
	// about two namespaces claiming one name can name both files.
	Path string `yaml:"-"`
}

// Route is either an ordered list of hops, or a choice between two other routes.
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
// The second is if/else and not if. `else:` is required, because the case this
// exists for is not "do something extra" — it is that the same task has two
// correct routes and which one is correct depends on where the laptop is. A
// missing branch would mean a task that silently does nothing somewhere.
//
// # Why the predicate may not travel
//
// Measured on the route this was built for: 326, 360 and 377 ms per connection
// through the tunnel against 0.65 ms on the LAN — about 500x. That is nothing for
// a task that runs once and everything for a tool making hundreds of small ssh
// round trips.
//
// Which is also why the obvious predicate is the wrong one. Probing the far end
// answers the question exactly, and costs a timeout on EVERY connection — paid
// hundreds of times by the thing that needed the fast route in the first place.
// An interface check touches no network and is instant, so a predicate with a
// `route:` is refused at load: it would pay a connection to answer a question
// about this machine, and that reads as slowness rather than as a mistake.
type Route struct {
	// Hops is set for the list form.
	Hops []Hop
	// If, Then and Else are set for the selector form. If names a TASK in this
	// namespace whose exit status is the answer; Then and Else name other routes
	// in the same namespace.
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
		return n.Decode(&r.Hops)
	case yaml.MappingNode:
		// The outer decoder's KnownFields does not reach in here, so unknown keys
		// are checked by hand. Without this a typo is silently dropped and the
		// route merely stops being a selector — which surfaces as "has no hops",
		// naming neither the key nor the mistake.
		for i := 0; i+1 < len(n.Content); i += 2 {
			switch key := n.Content[i].Value; key {
			case "if", "then", "else":
			default:
				return fmt.Errorf("line %d: a route has no `%s:` — a selector is `if:`/`then:`/`else:`", n.Content[i].Line, key)
			}
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

// Addr is the hop's dial address. Not a URL and not a template: a host and a
// port, resolved by whoever is doing the dialling — which for every hop after
// the first is the machine before it.
func (h Hop) Addr() string { return net.JoinHostPort(h.Host, strconv.Itoa(h.Port)) }

// Task is one runnable thing at the end of a route.
type Task struct {
	Desc string `yaml:"desc"`
	// Route names one of the namespace's routes. Required — see the manual on why
	// there is no default.
	Route string `yaml:"route"`
	// Cmd is what to run. WHERE it runs is not its business: a task with a
	// `route:` runs at the far end of it, and a task without one runs here.
	//
	// That split is deliberate and it replaced three keys. `exec:` (there),
	// `local:` (here) and a shell form would all have meant "run a thing", leaving
	// a reader to work out which of them to reach for — and the only difference
	// between them was a question another field already answers. One key for what,
	// one key for where, and no rule to remember.
	//
	// The local step this exists for is the one that has to happen before a route
	// can be travelled at all: unlocking a vault so the ssh-agent has the key.
	// That cannot be done at the far end by definition.
	Cmd Cmd `yaml:"cmd"`
	// Forward is a port to bring back to this machine. It is not a command, which
	// is why it is a key of its own rather than another spelling of Cmd.
	Forward *Forward `yaml:"forward"`
	// WithRoute resolves a route and hands its connection details to a LOCAL
	// command as environment, instead of travelling it.
	//
	// It exists for the consumer chore cannot route: a tool that does its own ssh.
	// Pulumi shells out to `ssh` for every one of a hundred-odd resources, so chore
	// has nothing to carry for it — what it can do is answer "which route is
	// correct from here, right now" and let the tool pass that on as its own
	// arguments:
	//
	//	pulumi:up:
	//	  with_route: homelab
	//	  cmd: 'pulumi up --config host=$CHORE_ROUTE_HOST --config jump=$CHORE_ROUTE_JUMP'
	//
	// The child is given CHORE_ROUTE, _HOST, _PORT, _USER and _JUMP — the target
	// hop, and everything before it in the `-J` form ssh itself takes. Passing them
	// down as arguments is the point: a value arriving as configuration and being
	// handed on is not the same thing as a transport reaching into the environment
	// to decide how to behave.
	WithRoute string `yaml:"with_route"`
	// Exports absorbs `KEY=value` lines from a local command's STDOUT into the
	// environment of everything that runs after it — `eval "$(…)"` written as a
	// declaration.
	//
	// This is what makes an unlock step useful rather than merely noisy. Tools that
	// hand credentials to a shell do it by printing exports, because a child cannot
	// reach into its parent's environment: `trove unlock --export` prints the
	// socket of the agent now holding the key, and without absorbing it chore would
	// dial whatever stale socket the shell was started with — which is exactly the
	// failure this was written to fix.
	//
	// Opt-in per task and never a default, because a command whose OUTPUT silently
	// sets variables is a surface worth declaring. stdout is captured while stderr
	// still streams, so the tool's own messages are not swallowed with it.
	Exports bool `yaml:"exports"`
	// Deps are tasks in the same namespace to run first, in order.
	//
	// Named rather than implied, and ordered rather than concurrent: the reason a
	// task has one is almost always that it must happen first, and a dependency
	// that ran in parallel with the thing needing it would be no dependency at
	// all.
	Deps []string `yaml:"deps"`
	// Internal hides the task from listings and refuses it from the command line,
	// exactly as it does for a project task: a helper is part of an
	// implementation, not part of the surface.
	Internal bool `yaml:"internal"`

	// Name and Namespace are filled in by the loader, so an error or a listing can
	// name the task the way a person would type it.
	Name      string `yaml:"-"`
	Namespace string `yaml:"-"`
}

// Address is how the task is typed: `global:homelab:k3s:pods`.
func (t *Task) Address() string { return "global:" + t.Namespace + ":" + t.Name }

// Cmd is a command in either of the two forms that mean different things.
//
//	cmd: [kubectl, get, pods, -A]        an argv
//	cmd: 'kubectl get pods -A | wc -l'   a shell line
//
// Not a shorthand for each other. An argv is quoted by chore, so an argument
// containing a space or a quote arrives whole and there is nothing to get wrong;
// a shell line is passed through, so it can pipe, redirect and expand — and its
// quoting is yours. The list form CANNOT express a pipe, which is the whole
// reason both exist.
//
// Worth knowing about the far end either way: the SSH protocol has no argv. Its
// exec request carries one string, which the far end hands to a login shell. So
// there is always a shell over there; the argv form is chore doing that quoting
// once, correctly, rather than every task doing it by hand.
type Cmd struct {
	// Argv is set for the list form, Line for the string form. Exactly one.
	Argv []string
	Line string
}

// Empty reports whether the task declared no command at all.
func (c Cmd) Empty() bool { return len(c.Argv) == 0 && strings.TrimSpace(c.Line) == "" }

// String renders the command the way it will be handed to a shell — which is
// what a listing, a dry run and an error should show, since it is what actually
// runs.
func (c Cmd) String() string {
	if len(c.Argv) > 0 {
		return quoteArgv(c.Argv)
	}
	return c.Line
}

// UnmarshalYAML accepts either form.
func (c *Cmd) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&c.Line)
	case yaml.SequenceNode:
		return n.Decode(&c.Argv)
	default:
		return fmt.Errorf("line %d: `cmd:` is a list (an argv) or a string (a shell line)", n.Line)
	}
}

// Forward is a tunnel: a port at the far end, bound here.
//
// The two fields are spelled out rather than shortened because a route commonly
// has more than one 127.0.0.1 in it meaning different machines — `remote` is
// resolved at the end of the route, `local` on the machine you are sitting at,
// and a reader has no way to tell them apart from the addresses alone.
type Forward struct {
	Remote string `yaml:"remote"`
	Local  string `yaml:"local"`
}

// Validate checks a namespace the way chore checks a taskfile: everything that
// cannot work is refused where it is written, not when somebody runs it.
func (n *Namespace) Validate() error {
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("%s: needs a `name:` — it is the word between `global:` and the task, as in `global:homelab:k3s:pods`", n.Path)
	}
	if strings.ContainsAny(n.Name, ": \t") {
		return fmt.Errorf("%s: name %q cannot contain a colon or a space: the colon separates a namespace from its task", n.Path, n.Name)
	}
	// A task cannot be called `global:…`, because that is how an ADDRESS is
	// written. One would be unreachable — `global:x:global:y:z` has no reading
	// that finds it — and it would make `deps: [global:y:z]` ambiguous between a
	// local task and another namespace's.
	for _, name := range sortedKeys(n.Tasks) {
		if strings.HasPrefix(name, "global:") {
			return fmt.Errorf("%s: task %q is named with the `global:` prefix, which is how an address is written —"+
				" it could never be reached, and it would make `deps: [%s]` ambiguous", n.Path, name, name)
		}
	}
	for name, route := range n.Routes {
		if route.Conditional() {
			if err := n.validateChoice(name, route); err != nil {
				return err
			}
			continue
		}
		if len(route.Hops) == 0 {
			return fmt.Errorf("%s: route %q has no hops — a route is at least one machine to reach", n.Path, name)
		}
		for i, h := range route.Hops {
			if strings.TrimSpace(h.Host) == "" {
				return fmt.Errorf("%s: route %q, hop %d: needs a `host:`", n.Path, name, i+1)
			}
			if h.Port < 0 || h.Port > 65535 {
				return fmt.Errorf("%s: route %q, hop %d: port %d is not a port", n.Path, name, i+1, h.Port)
			}
		}
	}
	for name, t := range n.Tasks {
		if t == nil {
			return fmt.Errorf("%s: task %q is empty", n.Path, name)
		}
		if err := t.validate(n, name); err != nil {
			return err
		}
	}
	return nil
}

// validateChoice checks a conditional route: both branches named, both real, and
// no way round the loop back to itself.
func (n *Namespace) validateChoice(name string, route Route) error {
	// The predicate is a task, so a typo is caught here rather than surviving as
	// a route that silently always takes `else:` — which looks exactly like a
	// predicate that works.
	if !strings.HasPrefix(route.If, "global:") {
		p, ok := n.Tasks[route.If]
		if !ok {
			return fmt.Errorf("%s: route %q names %q as its `if:`, which is not a task in this namespace",
				n.Path, name, route.If)
		}
		// A predicate answers a question about THIS machine. One that travelled a
		// route would pay a connection to answer it — measured at 326-377 ms
		// against 0.65 ms on the LAN — which is the cost this whole shape exists
		// to avoid, and it would read as slowness rather than as a mistake.
		if p.Route != "" {
			return fmt.Errorf("%s: route %q names task %q as its `if:`, but that task has `route: %s` —"+
				" a predicate must run on this machine", n.Path, name, route.If, p.Route)
		}
	}
	for _, branch := range []struct{ key, value string }{{"then", route.Then}, {"else", route.Else}} {
		if branch.value == "" {
			return fmt.Errorf("%s: route %q has `if:` but no `%s:` — a choice needs both branches,"+
				" because the other one is a different route rather than doing nothing", n.Path, name, branch.key)
		}
		if _, ok := n.Routes[branch.value]; !ok {
			return fmt.Errorf("%s: route %q names %q as its `%s:`, which is not one of: %s",
				n.Path, name, branch.value, branch.key, strings.Join(sortedKeys(n.Routes), ", "))
		}
	}
	// A cycle is not checked here. It cannot be: a selector may name another
	// namespace's task, and one file cannot see another. Set.checkCycles walks
	// routes and tasks as one graph once every namespace is in.
	return nil
}

func (t *Task) validate(n *Namespace, name string) error {
	hasCmd, hasForward := !t.Cmd.Empty(), t.Forward != nil
	switch {
	case hasCmd && hasForward:
		return fmt.Errorf("%s: task %q sets both `cmd:` and `forward:` —"+
			" a task either runs a command or holds a tunnel open", n.Path, name)
	case !hasCmd && !hasForward:
		return fmt.Errorf("%s: task %q needs `cmd:` (what to run) or `forward:` (a port to bring back)", n.Path, name)
	}
	if len(t.Cmd.Argv) > 0 && t.Cmd.Line != "" {
		return fmt.Errorf("%s: task %q gives `cmd:` as both a list and a string", n.Path, name)
	}

	for _, dep := range t.Deps {
		if strings.HasPrefix(dep, "global:") {
			// Another namespace's task, which is not visible from inside one
			// file. Checked by Set.crossCheck once every namespace has loaded.
			continue
		}
		if _, ok := n.Tasks[dep]; !ok {
			return fmt.Errorf("%s: task %q depends on %q, which is not a task in this namespace", n.Path, name, dep)
		}
		if dep == name {
			return fmt.Errorf("%s: task %q depends on itself", n.Path, name)
		}
	}

	if t.WithRoute != "" {
		if t.Route != "" {
			return fmt.Errorf("%s: task %q sets both `route:` and `with_route:` — one travels a route,"+
				" the other hands its details to something here that will travel it itself", n.Path, name)
		}
		if _, ok := n.Routes[t.WithRoute]; !ok {
			return fmt.Errorf("%s: task %q names `with_route: %s`, which is not one of: %s",
				n.Path, name, t.WithRoute, strings.Join(sortedKeys(n.Routes), ", "))
		}
	}

	if t.Exports && t.Route != "" {
		return fmt.Errorf("%s: task %q sets `exports:` and a `route:` — the variables would be set at the far"+
			" end, inside a session that ends with the command, so nothing here could ever see them", n.Path, name)
	}

	// No route means it runs here, which is a complete statement and needs no
	// further checking — except that a tunnel has to go somewhere.
	if t.Route == "" {
		if hasForward {
			return fmt.Errorf("%s: task %q forwards a port but names no `route:` — there is nowhere to forward from", n.Path, name)
		}
		return nil
	}

	switch {
	case len(n.Routes) == 0:
		return fmt.Errorf("%s: task %q names route %q, but the file declares no routes", n.Path, name, t.Route)
	}
	if _, ok := n.Routes[t.Route]; !ok {
		return fmt.Errorf("%s: task %q names route %q, which is not one of: %s",
			n.Path, name, t.Route, strings.Join(sortedKeys(n.Routes), ", "))
	}
	if hasForward {
		for _, side := range []struct{ what, addr string }{
			{"remote", t.Forward.Remote},
			{"local", t.Forward.Local},
		} {
			if strings.TrimSpace(side.addr) == "" {
				return fmt.Errorf("%s: task %q: forward needs `%s:` as host:port", n.Path, name, side.what)
			}
			if _, _, err := net.SplitHostPort(side.addr); err != nil {
				return fmt.Errorf("%s: task %q: forward %s is %q, which is not host:port", n.Path, name, side.what, side.addr)
			}
		}
	}
	return nil
}
