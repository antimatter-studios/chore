package global

import (
	"fmt"
	"os"
	"strings"
)

// chore:manual global
// order: 20
//
// ## `$VAR` in a namespace file
//
// ```yaml
// routes:
//   pi: [ { host: $HOMELAB_HOST, user: $USER } ]
// tasks:
//   unlock:
//     cmd: [trove, unlock, $HOME/vault.kdbx, --export]
// ```
//
// Which machine's `$HOME`? The rule is one sentence:
//
// > Every value in the file is expanded HERE, when the file is read — except the
// > string form of `cmd:`, which is handed to a shell that expands it at
// > whichever end it runs.
//
// So `cmd: [ls, $HOME]` is this machine's home, and `cmd: 'ls $HOME'` on a task
// with a `route:` is the far end's. Both are useful and neither can be reached by
// accident.
//
// The escape hatch is the shell form; `$$` is a literal `$` for the rest.
//
// **An unset variable is an error, not an empty string.** A path that quietly
// became `/vault.kdbx` because `$HOME` was not set is the silent-wrong-target
// failure this whole program exists to remove — and these files are read on
// machines their author is not sitting at, which is exactly where a blank would
// go unnoticed.

// expand replaces $VAR and ${VAR} from chore's own environment.
//
// `$$` is a literal dollar. An unset variable is an error: see the manual block
// above for why a blank is the one answer this cannot give.
func expand(where, s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		name, width := varName(s[i+1:])
		if name == "" {
			// A lone `$`, or one followed by something that cannot be a name. Left
			// as written rather than guessed at.
			b.WriteByte(c)
			i++
			continue
		}
		value, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("%s: $%s is not set in this environment", where, name)
		}
		b.WriteString(value)
		i += 1 + width
	}
	return b.String(), nil
}

// varName reads a variable reference at the start of s, returning its name and
// how many bytes it occupied. Both forms, because `${HOME}x` and `$HOMEx` are
// different variables and a file should be able to say either.
func varName(s string) (name string, width int) {
	if s == "" {
		return "", 0
	}
	if s[0] == '{' {
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return "", 0
		}
		inner := s[1:end]
		if !isEnvName(inner) {
			return "", 0
		}
		return inner, end + 1
	}
	i := 0
	for i < len(s) {
		c := s[i]
		isLetter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if isLetter || (isDigit && i > 0) {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return "", 0
	}
	return s[:i], i
}

// expandNamespace rewrites every value chore itself consumes.
//
// Deliberately NOT the string form of `cmd:`: that goes to a shell, which does
// its own expansion at the end where it runs, and expanding it here would
// silently substitute this machine's values into a command meant for another.
func expandNamespace(n *Namespace) error {
	for name, route := range n.Routes {
		// A predicate is a shell line, so it keeps its own $VAR for the shell to
		// expand — the same rule the string form of `cmd:` follows.
		for i := range route.Hops {
			var err error
			where := fmt.Sprintf("%s: route %q, hop %d", n.Path, name, i+1)
			if route.Hops[i].Host, err = expand(where, route.Hops[i].Host); err != nil {
				return err
			}
			if route.Hops[i].User, err = expand(where, route.Hops[i].User); err != nil {
				return err
			}
		}
		n.Routes[name] = route
	}
	for name, t := range n.Tasks {
		if t == nil {
			continue
		}
		where := fmt.Sprintf("%s: task %q", n.Path, name)
		for i, arg := range t.Cmd.Argv {
			var err error
			if t.Cmd.Argv[i], err = expand(where, arg); err != nil {
				return err
			}
		}
		if t.Forward != nil {
			var err error
			if t.Forward.Remote, err = expand(where+": forward remote", t.Forward.Remote); err != nil {
				return err
			}
			if t.Forward.Local, err = expand(where+": forward local", t.Forward.Local); err != nil {
				return err
			}
		}
	}
	return nil
}
