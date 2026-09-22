package global

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// Resolved is a route after any choices in it have been made: the hops to
// travel, and how that was decided.
//
// Why is carried alongside rather than discarded, because a predicate that
// quietly always takes the same branch looks exactly like one that works —
// until the day it matters. `--dry` prints it, so the choice can be seen without
// running the task.
type Resolved struct {
	Name string
	Hops []Hop
	Why  string
}

// Predicate answers whether a named task succeeded.
//
// Passed in rather than called from here, because running a task is the
// Runner's job and resolving a route must be possible without one: `--dry`
// resolves nothing and never calls this at all.
type Predicate func(ctx context.Context, task string) bool

// Resolve walks a route name to actual hops, asking a predicate on the way.
//
// The predicate is a TASK on THIS machine, and its exit status is the whole
// answer: 0 takes `then:`, anything else takes `else:`. Not stdout, and not a
// parsed value — a command already ends in an exit status, so anything else
// would be a second convention to remember.
func (n *Namespace) Resolve(ctx context.Context, name string, ask Predicate) (Resolved, error) {
	var trail []string
	seen := map[string]bool{}
	for {
		route, ok := n.Routes[name]
		if !ok {
			return Resolved{}, fmt.Errorf("no route %q in %s", name, n.Path)
		}
		if !route.Conditional() {
			return Resolved{Name: name, Hops: route.Hops, Why: strings.Join(trail, " ")}, nil
		}
		if seen[name] {
			return Resolved{}, fmt.Errorf("route %q leads back to itself", name)
		}
		seen[name] = true

		taken, branch := route.Else, "else"
		if ask(ctx, route.If) {
			taken, branch = route.Then, "then"
		}
		trail = append(trail, fmt.Sprintf("%s: %s -> %s", name, branch, taken))
		name = taken
	}
}

// Target is the hop a route ends at — what a caller outside chore would connect
// to — and Jump is everything before it, in the `-J` form ssh itself takes.
//
// Split this way because that is the split every consumer needs: ssh's own
// ProxyJump, Pulumi's connection spec, and a person reading the file all treat
// the destination and the way there as two different things.
func (r Resolved) Target() (Hop, bool) {
	if len(r.Hops) == 0 {
		return Hop{}, false
	}
	return r.Hops[len(r.Hops)-1], true
}

// Jump renders the intermediate hops as ssh's -J argument: user@host:port,
// comma separated, in order. Empty when the route is direct.
func (r Resolved) Jump() string {
	if len(r.Hops) < 2 {
		return ""
	}
	var parts []string
	for _, h := range r.Hops[:len(r.Hops)-1] {
		parts = append(parts, fmt.Sprintf("%s@%s", h.User, net.JoinHostPort(h.Host, itoa(h.Port))))
	}
	return strings.Join(parts, ",")
}

// Env is the resolved route as environment for a child process: the values a
// program that does its own ssh needs in order to make the same connection.
//
// This exists because the tool that most needs the fast route does not go
// through chore at all — Pulumi shells out to ssh itself. chore cannot route it,
// so it hands over the details instead and the child passes them on as its own
// arguments.
func (r Resolved) Env() []string {
	target, ok := r.Target()
	if !ok {
		return nil
	}
	return []string{
		"CHORE_ROUTE=" + r.Name,
		"CHORE_ROUTE_HOST=" + target.Host,
		"CHORE_ROUTE_PORT=" + itoa(target.Port),
		"CHORE_ROUTE_USER=" + target.User,
		"CHORE_ROUTE_JUMP=" + r.Jump(),
	}
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
