package tmpl

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

func TestValueStringForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    Value
		want string
	}{
		{"string", StringValue("x y"), "x y"},
		{"list", ListValue([]string{"web1", "web2"}), "web1 web2"},
		{"empty list", ListValue(nil), ""},
		{"map", MapValue([]string{"web", "db"}, []string{"80", "5432"}), "web=80 db=5432"},
	}
	for _, tc := range cases {
		if got := tc.v.String(); got != tc.want {
			t.Errorf("%s: String() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A list keeps its items through Push and Lookup, and is seen as its string form
// by Get, All and Render — templates never meet the collection itself.
func TestScopeKeepsCollections(t *testing.T) {
	t.Parallel()
	s := New(nil).PushValues(map[string]Value{
		"HOSTS": ListValue([]string{"a", "b"}),
		"PORTS": MapValue([]string{"web"}, []string{"80"}),
	}).Push(map[string]string{"OTHER": "x"})

	v, ok := s.Lookup("HOSTS")
	if !ok || v.Kind != List || !reflect.DeepEqual(v.Items, []string{"a", "b"}) {
		t.Fatalf("Lookup(HOSTS) = %+v, %v; want the list", v, ok)
	}
	m, _ := s.Lookup("PORTS")
	if m.Kind != Map || !reflect.DeepEqual(m.Keys, []string{"web"}) || !reflect.DeepEqual(m.Items, []string{"80"}) {
		t.Errorf("Lookup(PORTS) = %+v; want the map", m)
	}
	if got, _ := s.Get("HOSTS"); got != "a b" {
		t.Errorf("Get(HOSTS) = %q, want %q", got, "a b")
	}
	if got := s.All()["PORTS"]; got != "web=80" {
		t.Errorf("All()[PORTS] = %q, want %q", got, "web=80")
	}
	out, err := s.Render("for h in {{.HOSTS}}; do :; done")
	if err != nil || out != "for h in a b; do :; done" {
		t.Errorf("Render = %q, %v", out, err)
	}
	if _, ok := s.Lookup("MISSING"); ok {
		t.Error("Lookup(MISSING) reported a value")
	}
}

// A string set above a list shadows it, as any higher layer does: a value typed
// on the command line replaces the file's list.
func TestAStringShadowsAList(t *testing.T) {
	t.Parallel()
	s := New(nil).PushValues(map[string]Value{"HOSTS": ListValue([]string{"a", "b"})}).
		Push(map[string]string{"HOSTS": "c d e"})
	v, _ := s.Lookup("HOSTS")
	if v.Kind != String || v.String() != "c d e" {
		t.Errorf("Lookup(HOSTS) = %+v, want the string from the higher layer", v)
	}

	// And SetValue writes a collection into the top layer.
	s.SetValue("HOSTS", ListValue([]string{"z"}))
	if v, _ := s.Lookup("HOSTS"); v.Kind != List {
		t.Errorf("after SetValue, Lookup(HOSTS) = %+v, want a list", v)
	}
	var zero Scope
	zero.SetValue("X", ListValue([]string{"1"}))
	if got, _ := zero.Get("X"); got != "1" {
		t.Errorf("zero Scope SetValue/Get = %q, want 1", got)
	}
}

func TestResolveValuesCollections(t *testing.T) {
	t.Parallel()
	base := New(nil).Push(map[string]string{"ENV": "prod"})
	vars := map[string]chorefile.Var{
		// Items are templates, and may read another var of the same block —
		// which orders resolution exactly as it would for a string var.
		"HOSTS":  {Kind: chorefile.VarList, List: []string{"{{.PREFIX}}-1", "{{.PREFIX}}-2"}},
		"PREFIX": {Value: "{{.ENV}}"},
		"PORTS": {Kind: chorefile.VarMap, Map: []chorefile.MapEntry{
			{Key: "web", Value: "80"}, {Key: "api", Value: "{{.API_PORT}}"},
		}},
		"API_PORT": {Value: "8080"},
		"EMPTY":    {Kind: chorefile.VarList, List: []string{}},
		// A string var reading a list sees its string form.
		"ALL": {Value: "[{{.HOSTS}}]"},
	}
	got, err := base.ResolveValues(context.Background(), vars, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := got["HOSTS"]; v.Kind != List || !reflect.DeepEqual(v.Items, []string{"prod-1", "prod-2"}) {
		t.Errorf("HOSTS = %+v", v)
	}
	if v := got["PORTS"]; v.Kind != Map || !reflect.DeepEqual(v.Keys, []string{"web", "api"}) ||
		!reflect.DeepEqual(v.Items, []string{"80", "8080"}) {
		t.Errorf("PORTS = %+v", v)
	}
	if v := got["EMPTY"]; v.Kind != List || len(v.Items) != 0 {
		t.Errorf("EMPTY = %+v, want an empty list", v)
	}
	if v := got["ALL"]; v.String() != "[prod-1 prod-2]" {
		t.Errorf("ALL = %q", v.String())
	}

	// Resolve flattens the same block to string forms.
	flat, err := base.Resolve(context.Background(), vars, nil)
	if err != nil {
		t.Fatal(err)
	}
	if flat["HOSTS"] != "prod-1 prod-2" || flat["PORTS"] != "web=80 api=8080" {
		t.Errorf("Resolve flattened to %v", flat)
	}
}

func TestResolveValuesCollectionErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		vars map[string]chorefile.Var
		want string
	}{
		{"bad list item", map[string]chorefile.Var{
			"L": {Kind: chorefile.VarList, List: []string{"{{.X"}},
		}, "var L"},
		{"bad map value", map[string]chorefile.Var{
			"M": {Kind: chorefile.VarMap, Map: []chorefile.MapEntry{{Key: "k", Value: "{{if}}"}}},
		}, "var M"},
		{"a cycle through a list", map[string]chorefile.Var{
			"A": {Kind: chorefile.VarList, List: []string{"{{.B}}"}},
			"B": {Value: "{{.A}}"},
		}, "cycle"},
	}
	for _, tc := range cases {
		_, err := New(nil).ResolveValues(context.Background(), tc.vars, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}
