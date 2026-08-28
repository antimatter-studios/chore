package manual

import (
	"strings"
	"testing"
)

// These run against the EMBEDDED pages, not a fixture, so they also assert that
// what the generator committed is loadable — a hand-edit to a generated file
// shows up here rather than as a blank page at the terminal.

func TestAllTopicsAreComplete(t *testing.T) {
	topics := All()
	if len(topics) == 0 {
		t.Fatal("no topics embedded")
	}
	for _, top := range topics {
		if err := ValidTopic(top.Name); err != nil {
			t.Errorf("%v", err)
		}
		if top.Title == "" || top.Summary == "" {
			t.Errorf("%s: title=%q summary=%q — both are required", top.Name, top.Title, top.Summary)
		}
		if strings.TrimSpace(top.Body) == "" {
			t.Errorf("%s: empty body", top.Name)
		}
		// The front matter must not survive into what a reader sees.
		if strings.HasPrefix(top.Body, "---") || strings.Contains(top.Body, "<!-- Generated") {
			t.Errorf("%s: front matter leaked into the body:\n%s", top.Name, top.Body[:min(200, len(top.Body))])
		}
		if len(top.Sources) == 0 {
			t.Errorf("%s: no recorded sources", top.Name)
		}
	}
}

func TestLookupAcceptsHyphensUnderscoresAndAliases(t *testing.T) {
	for _, name := range []string{"hooks", "HOOKS", " hooks ", "lifecycle-hooks", "lifecycle_hooks", "lifecycle"} {
		got, ok := Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) missed", name)
			continue
		}
		if got.Name != "hooks" {
			t.Errorf("Lookup(%q) = %q, want hooks", name, got.Name)
		}
	}
	// up-to-date is the topic whose canonical name carries a hyphen, so it is the
	// one that proves the normalisation runs on both sides of the comparison.
	if got, ok := Lookup("up_to_date"); !ok || got.Name != "up-to-date" {
		t.Errorf(`Lookup("up_to_date") = %q, %v`, got.Name, ok)
	}
	if _, ok := Lookup("nothing-like-this"); ok {
		t.Error("Lookup must not invent a topic")
	}
}

func TestSuggestFindsTheNearMiss(t *testing.T) {
	if got := Suggest("hook"); len(got) == 0 {
		t.Error(`Suggest("hook") found nothing`)
	} else if got[0] != "hooks" {
		t.Errorf(`Suggest("hook") = %v, want hooks first`, got)
	}
	if got := Suggest("zzzz"); len(got) != 0 {
		t.Errorf("Suggest on a word with no relation = %v, want nothing", got)
	}
}

func TestValidTopic(t *testing.T) {
	for _, ok := range []string{"hooks", "up-to-date", "a1", "one_two"} {
		if err := ValidTopic(ok); err != nil {
			t.Errorf("ValidTopic(%q) = %v, want nil", ok, err)
		}
	}
	// A leading or trailing separator would produce a name nobody types the same
	// way twice, and an uppercase one would not match what a reader writes.
	for _, bad := range []string{"", "Hooks", "-hooks", "hooks-", "with space", "with.dot"} {
		if err := ValidTopic(bad); err == nil {
			t.Errorf("ValidTopic(%q) = nil, want an error", bad)
		}
	}
}

// The hooks page is assembled from two packages. If ordering or concatenation
// breaks, the page silently loses half of itself rather than failing.
func TestHooksPageIsWholeAndOrdered(t *testing.T) {
	top, ok := Lookup("hooks")
	if !ok {
		t.Fatal("no hooks topic")
	}
	fields := strings.Index(top.Body, "## The four task fields")
	deferSec := strings.Index(top.Body, "## `defer:` — the positional one")
	if fields < 0 || deferSec < 0 {
		t.Fatalf("hooks page is missing a section: fields=%d defer=%d", fields, deferSec)
	}
	if deferSec < fields {
		t.Error("the defer section (order: 20) must follow the task fields (order: 10)")
	}
	if len(top.Sources) < 2 {
		t.Errorf("hooks should be built from at least two blocks, got %v", top.Sources)
	}
}
