package global

import (
	"fmt"
	"net"
	"os/user"
	"strconv"
	"strings"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// DefaultPort is what a hop with no `port:` dials, as ssh does.
const DefaultPort = 22

// Resolved is a route after any choices in it have been made: the hops to
// travel, and how that was decided.
//
// Why is carried alongside rather than discarded, because a predicate that
// quietly always takes the same branch looks exactly like one that works —
// until the day it matters. `--verbose` prints it.
type Resolved struct {
	Name string
	Hops []chorefile.Hop
	Why  string
}

// Target is the hop a route ends at — what a caller outside chore would connect
// to — and Jump is everything before it, in the `-J` form ssh itself takes.
//
// Split this way because that is the split every consumer needs: ssh's own
// ProxyJump, Pulumi's connection spec, and a person reading the file all treat
// the destination and the way there as two different things.
func (r Resolved) Target() (chorefile.Hop, bool) {
	if len(r.Hops) == 0 {
		return chorefile.Hop{}, false
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
		parts = append(parts, fmt.Sprintf("%s@%s", h.User, net.JoinHostPort(h.Host, strconv.Itoa(h.Port))))
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
		"CHORE_ROUTE_PORT=" + strconv.Itoa(target.Port),
		"CHORE_ROUTE_USER=" + target.User,
		"CHORE_ROUTE_JUMP=" + r.Jump(),
	}
}

// Render renders a template in the scope of the task using a route.
type Render func(text string) (string, error)

// PrepareHops renders each hop's host, user and templated port, expands `$VAR`
// in them, and fills in what ssh would: port 22, and this user's name. Done
// before a hop is dialled or printed, so an error and a dry run name the address
// a connection will actually use rather than the blanks the file left.
func PrepareHops(route string, hops []chorefile.Hop, render Render, lookup Lookup) ([]chorefile.Hop, error) {
	me := ""
	if u, err := user.Current(); err == nil {
		me = u.Username
	}
	out := make([]chorefile.Hop, len(hops))
	for i, h := range hops {
		where := fmt.Sprintf("route %s, hop %d", route, i+1)
		field := func(name, text string) (string, error) {
			if render != nil && strings.Contains(text, "{{") {
				r, err := render(text)
				if err != nil {
					return "", fmt.Errorf("%s: %s: %w", where, name, err)
				}
				text = r
			}
			return Expand(where, text, lookup)
		}
		var err error
		if h.Host, err = field("host", h.Host); err != nil {
			return nil, err
		}
		if h.Host == "" {
			return nil, fmt.Errorf("%s: host rendered empty", where)
		}
		if h.User, err = field("user", h.User); err != nil {
			return nil, err
		}
		if h.PortText != "" {
			text, err := field("port", h.PortText)
			if err != nil {
				return nil, err
			}
			p, err := strconv.Atoi(strings.TrimSpace(text))
			if err != nil || p < 1 || p > 65535 {
				return nil, fmt.Errorf("%s: port %q is not a port", where, text)
			}
			h.Port, h.PortText = p, ""
		}
		if h.Port == 0 {
			h.Port = DefaultPort
		}
		if h.User == "" {
			h.User = me
		}
		out[i] = h
	}
	return out, nil
}
