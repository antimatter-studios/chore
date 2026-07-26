package run

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// parseDotenv reads a KEY=VALUE file. Deliberately small: the files this program
// loads are generated, so the format is the boring subset — comments, blank
// lines, optional quotes, no interpolation and no `export` cleverness. Anything
// stranger should be an error rather than a guess, because a silently
// mis-parsed value is how a stack ends up pointing at the wrong host.
func parseDotenv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	s := bufio.NewScanner(f)
	line := 0
	for s.Scan() {
		line++
		text := strings.TrimSpace(s.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		k, v, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE, got %q", path, line, text)
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("%s:%d: empty variable name", path, line)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				if unquoted, err := strconv.Unquote(v); err == nil {
					v = unquoted
				} else {
					v = v[1 : len(v)-1]
				}
			}
		}
		out[k] = v
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}
