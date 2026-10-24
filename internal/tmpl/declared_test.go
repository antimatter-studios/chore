package tmpl

import (
	"maps"
	"testing"
)

// Declared is every layer above the process environment, flattened: what the
// chorefile and the command line said, and nothing the shell happened to carry.
func TestDeclaredLeavesOutTheEnvironment(t *testing.T) {
	s := New([]string{"HOME=/home/x", "SHLVL=3", "STACK=from-env"}).
		Push(map[string]string{"STACK": "file", "REGION": "eu"}).
		Push(map[string]string{"STACK": "arg"})

	want := map[string]string{"STACK": "arg", "REGION": "eu"}
	if got := s.Declared(); !maps.Equal(got, want) {
		t.Errorf("Declared() = %v, want %v", got, want)
	}
	// The environment is still there for everything else.
	if v, _ := s.Get("HOME"); v != "/home/x" {
		t.Errorf("Get(HOME) = %q", v)
	}
}

func TestDeclaredOfAnEnvironmentOnlyScopeIsEmpty(t *testing.T) {
	if got := New([]string{"HOME=/home/x"}).Declared(); len(got) != 0 {
		t.Errorf("Declared() = %v, want nothing", got)
	}
}
