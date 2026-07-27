package tmpl

import (
	"maps"
	"testing"
)

func TestNewFromEnviron(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  []string
		want map[string]string
	}{
		{name: "nil", env: nil, want: map[string]string{}},
		{name: "empty", env: []string{}, want: map[string]string{}},
		{
			name: "plain entries",
			env:  []string{"HOME=/root", "USER=chris"},
			want: map[string]string{"HOME": "/root", "USER": "chris"},
		},
		{
			// Only the FIRST "=" separates: values legitimately contain more.
			name: "value contains equals",
			env:  []string{"OPTS=a=1,b=2"},
			want: map[string]string{"OPTS": "a=1,b=2"},
		},
		{
			name: "empty value is a real entry",
			env:  []string{"EMPTY="},
			want: map[string]string{"EMPTY": ""},
		},
		{
			// Malformed inherited data is skipped, never fatal.
			name: "malformed entries skipped",
			env:  []string{"NO_EQUALS", "=novalue", "OK=yes"},
			want: map[string]string{"OK": "yes"},
		},
		{
			name: "later entry wins",
			env:  []string{"DUP=first", "DUP=second"},
			want: map[string]string{"DUP": "second"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := New(tc.env).All()
			if !maps.Equal(got, tc.want) {
				t.Errorf("New(%q).All() = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// layered builds the full precedence stack from SPEC "Fixed semantics" #2,
// lowest priority first, and returns every intermediate scope so a test can
// assert what each level sees.
func layered() (env, dotenv, file, include, task, call, args *Scope) {
	env = New([]string{"SHARED=env", "ONLY_ENV=env"})
	dotenv = env.Push(map[string]string{"SHARED": "dotenv", "ONLY_DOTENV": "dotenv"})
	file = dotenv.Push(map[string]string{"SHARED": "file", "ONLY_FILE": "file"})
	include = file.Push(map[string]string{"SHARED": "include", "ONLY_INCLUDE": "include"})
	task = include.Push(map[string]string{"SHARED": "task", "ONLY_TASK": "task"})
	call = task.Push(map[string]string{"SHARED": "call", "ONLY_CALL": "call"})
	args = call.Push(map[string]string{"SHARED": "args", "ONLY_ARGS": "args"})
	return
}

func TestLayerPrecedence(t *testing.T) {
	t.Parallel()

	env, dotenv, file, include, task, call, args := layered()

	// Each level must see its own value for the contested key, and nothing from
	// a level above it.
	levels := []struct {
		name  string
		scope *Scope
		want  string
	}{
		{"env", env, "env"},
		{"dotenv", dotenv, "dotenv"},
		{"file", file, "file"},
		{"include", include, "include"},
		{"task", task, "task"},
		{"call", call, "call"},
		{"args", args, "args"},
	}
	for _, l := range levels {
		t.Run(l.name, func(t *testing.T) {
			got, ok := l.scope.Get("SHARED")
			if !ok {
				t.Fatalf("%s scope: SHARED not found", l.name)
			}
			if got != l.want {
				t.Errorf("%s scope: SHARED = %q, want %q", l.name, got, l.want)
			}
		})
	}

	// The top scope keeps every non-contested key from every layer below it.
	t.Run("uncontested keys survive", func(t *testing.T) {
		want := map[string]string{
			"SHARED":       "args",
			"ONLY_ENV":     "env",
			"ONLY_DOTENV":  "dotenv",
			"ONLY_FILE":    "file",
			"ONLY_INCLUDE": "include",
			"ONLY_TASK":    "task",
			"ONLY_CALL":    "call",
			"ONLY_ARGS":    "args",
		}
		if got := args.All(); !maps.Equal(got, want) {
			t.Errorf("args.All() = %v, want %v", got, want)
		}
	})
}

func TestPushDoesNotMutateParent(t *testing.T) {
	t.Parallel()

	parent := New([]string{"A=1"})

	t.Run("child shadowing leaves parent alone", func(t *testing.T) {
		child := parent.Push(map[string]string{"A": "2", "B": "b"})
		if got, _ := child.Get("A"); got != "2" {
			t.Errorf("child A = %q, want 2", got)
		}
		if got, _ := parent.Get("A"); got != "1" {
			t.Errorf("parent A = %q, want 1", got)
		}
		if _, ok := parent.Get("B"); ok {
			t.Error("parent gained B from the child")
		}
	})

	t.Run("child Set leaves parent alone", func(t *testing.T) {
		child := parent.Push(nil)
		child.Set("A", "overridden")
		child.Set("NEW", "n")
		if got, _ := parent.Get("A"); got != "1" {
			t.Errorf("parent A = %q, want 1", got)
		}
		if _, ok := parent.Get("NEW"); ok {
			t.Error("parent gained NEW from the child")
		}
	})

	t.Run("parent Set after Push does not leak into child", func(t *testing.T) {
		// The child is a snapshot, so later parent writes are invisible to it.
		p := New([]string{"A=1"})
		child := p.Push(nil)
		p.Set("A", "changed")
		p.Set("LATE", "late")
		if got, _ := child.Get("A"); got != "1" {
			t.Errorf("child A = %q, want 1", got)
		}
		if _, ok := child.Get("LATE"); ok {
			t.Error("child gained LATE from the parent after Push")
		}
	})

	t.Run("mutating the pushed map does not affect the scope", func(t *testing.T) {
		vars := map[string]string{"A": "2"}
		child := parent.Push(vars)
		vars["A"] = "3"
		vars["C"] = "c"
		if got, _ := child.Get("A"); got != "2" {
			t.Errorf("child A = %q, want 2", got)
		}
		if _, ok := child.Get("C"); ok {
			t.Error("child gained C from the caller's map")
		}
	})

	t.Run("siblings are independent", func(t *testing.T) {
		// Sibling scopes must not share a backing array: appending a layer to
		// one must not be visible through the other.
		a := parent.Push(map[string]string{"X": "a"})
		b := parent.Push(map[string]string{"X": "b"})
		a.Set("Y", "ya")
		if got, _ := a.Get("X"); got != "a" {
			t.Errorf("a X = %q, want a", got)
		}
		if got, _ := b.Get("X"); got != "b" {
			t.Errorf("b X = %q, want b", got)
		}
		if _, ok := b.Get("Y"); ok {
			t.Error("sibling b saw a's Set")
		}
	})
}

func TestSetAndGet(t *testing.T) {
	t.Parallel()

	t.Run("Set outranks lower layers", func(t *testing.T) {
		s := New([]string{"A=env"}).Push(map[string]string{"A": "task"})
		s.Set("A", "set")
		if got, _ := s.Get("A"); got != "set" {
			t.Errorf("A = %q, want set", got)
		}
	})

	t.Run("missing key", func(t *testing.T) {
		s := New(nil)
		got, ok := s.Get("NOPE")
		if ok || got != "" {
			t.Errorf("Get(NOPE) = %q, %v; want \"\", false", got, ok)
		}
	})

	t.Run("empty value is present", func(t *testing.T) {
		// "" and "absent" are different: `default` treats them the same, but
		// Get must not, or requires: could never tell them apart.
		s := New([]string{"E="})
		got, ok := s.Get("E")
		if !ok || got != "" {
			t.Errorf("Get(E) = %q, %v; want \"\", true", got, ok)
		}
	})

	t.Run("zero value scope is usable", func(t *testing.T) {
		s := &Scope{}
		s.Set("A", "1")
		if got, _ := s.Get("A"); got != "1" {
			t.Errorf("A = %q, want 1", got)
		}
		got, err := s.Render("{{.A}}")
		if err != nil || got != "1" {
			t.Errorf("Render = %q, %v; want 1, nil", got, err)
		}
	})
}

func TestAllReturnsACopy(t *testing.T) {
	t.Parallel()

	s := New([]string{"A=1"}).Push(map[string]string{"B": "2"})
	got := s.All()
	got["A"] = "tampered"
	delete(got, "B")

	if v, _ := s.Get("A"); v != "1" {
		t.Errorf("A = %q, want 1 — All() aliased a layer", v)
	}
	if v, ok := s.Get("B"); !ok || v != "2" {
		t.Errorf("B = %q, %v; want 2, true — All() aliased a layer", v, ok)
	}
}
