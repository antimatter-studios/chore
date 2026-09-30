package global

import (
	"fmt"
	"strings"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// Lookup answers a variable by name: the task's own scope, which already holds
// chore's environment beneath its variables.
type Lookup func(name string) (string, bool)

// Expand replaces $VAR and ${VAR} in a value chore itself consumes — an argv
// word, a hop's host or user, a forward address — where no shell will ever see
// the text to expand it.
//
// `$$` is a literal dollar. An unset variable is an error, not an empty string:
// a path that quietly became `/vault.kdbx` because $HOME was not set is the
// silent-wrong-target failure chore exists to remove, and these values are
// often read on machines their author is not sitting at.
func Expand(where, s string, lookup Lookup) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
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
		value, ok := lookup(name)
		if !ok {
			return "", fmt.Errorf("%s: $%s is not set", where, name)
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
		if end < 0 || !chorefile.IsName(s[1:end]) {
			return "", 0
		}
		return s[1:end], end + 1
	}
	i := 0
	for i < len(s) && chorefile.IsName(s[:i+1]) {
		i++
	}
	return s[:i], i
}
