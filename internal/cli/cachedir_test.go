package cli

import (
	"path/filepath"
	"testing"
)

// The XDG rule on every platform: an absolute XDG_CACHE_HOME, else
// $HOME/.cache, else nowhere (and no task is replayed).
func TestCacheDirFollowsXDG(t *testing.T) {
	for _, c := range []struct {
		name, xdg, home, want string
	}{
		{"absolute XDG_CACHE_HOME", "/var/cache/me", "/home/me", "/var/cache/me"},
		{"a relative one is ignored", "cache", "/home/me", filepath.Join("/home/me", ".cache")},
		{"HOME alone", "", "/home/me", filepath.Join("/home/me", ".cache")},
		{"neither", "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", c.xdg)
			t.Setenv("HOME", c.home)
			if got := cacheDir(); got != c.want {
				t.Errorf("cacheDir() = %q, want %q", got, c.want)
			}
		})
	}
}
