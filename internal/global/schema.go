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
// - **Every task names its route.** There is no default route, for the same
//   reason: the task says where it goes, or it does not say it anywhere.
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

// Route is an ordered list of hops. The first is dialled from this machine and
// every later one is dialled FROM THE PREVIOUS HOP, which is what lets a route
// end at a machine this one cannot address at all.
type Route []Hop

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
	// Exec is the argv to run at the far end. Exactly one of Exec and Forward is
	// set; the decoder refuses a task with both or neither.
	Exec []string `yaml:"exec"`
	// Forward is a port to bring back to this machine instead of a command.
	Forward *Forward `yaml:"forward"`
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
	for name, route := range n.Routes {
		if len(route) == 0 {
			return fmt.Errorf("%s: route %q has no hops — a route is at least one machine to reach", n.Path, name)
		}
		for i, h := range route {
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

func (t *Task) validate(n *Namespace, name string) error {
	switch {
	case t.Route == "":
		return fmt.Errorf("%s: task %q needs a `route:` — every task says where it goes, because there is no default", n.Path, name)
	case len(n.Routes) == 0:
		return fmt.Errorf("%s: task %q names route %q, but the file declares no routes", n.Path, name, t.Route)
	}
	if _, ok := n.Routes[t.Route]; !ok {
		return fmt.Errorf("%s: task %q names route %q, which is not one of: %s",
			n.Path, name, t.Route, strings.Join(sortedKeys(n.Routes), ", "))
	}
	hasExec, hasForward := len(t.Exec) > 0, t.Forward != nil
	switch {
	case hasExec && hasForward:
		return fmt.Errorf("%s: task %q sets both `exec:` and `forward:` — a task either runs a command or holds a tunnel open", n.Path, name)
	case !hasExec && !hasForward:
		return fmt.Errorf("%s: task %q needs `exec:` (a command to run) or `forward:` (a port to bring back)", n.Path, name)
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
