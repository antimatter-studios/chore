package chorefile

import (
	"strings"
	"testing"
)

// The route fields are part of every task, so the decoder refuses a task that
// cannot work wherever it is written — a project's chores.yml included.
func TestDecodeRefusesARouteTaskThatCannotWork(t *testing.T) {
	const routes = "routes:\n  r: [{host: h}]\n"
	for _, tc := range []struct {
		name, body string
		want       string
	}{
		{"cmd and cmds", "tasks:\n  t: {cmd: a, cmds: [b]}\n", "both `cmd:` and `cmds:`"},
		{"forward and steps", routes + "tasks:\n  t: {route: r, cmd: a, forward: {remote: 'a:1', local: 'b:2'}}\n", "both steps and `forward:`"},
		{"forward without a route", "tasks:\n  t: {forward: {remote: 'a:1', local: 'b:2'}}\n", "names no `route:`"},
		{"forward not host:port", routes + "tasks:\n  t: {route: r, forward: {remote: nope, local: 'b:2'}}\n", "not host:port"},
		{"pty without a route", "tasks:\n  t: {pty: true, cmd: bash}\n", "`pty: true`"},
		{"route and with_route", routes + "tasks:\n  t: {route: r, with_route: r, cmd: a}\n", "both `route:` and `with_route:`"},
		{"exports with a route", routes + "tasks:\n  t: {route: r, exports: true, cmd: a}\n", "`exports:` and a `route:`"},
		{"an unknown route", routes + "tasks:\n  t: {route: q, cmd: a}\n", "not one of this file's routes: r"},
		{"no routes at all", "tasks:\n  t: {route: q, cmd: a}\n", "declares no routes"},
		{"a hop with no host", "routes:\n  r: [{user: me}]\n", "needs a `host:`"},
		{"a port out of range", "routes:\n  r: [{host: h, port: 70000}]\n", "not a port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// `cmd:` is one step: the decoder folds it into `cmds:`, so nothing after it has
// to ask which key a task used. Both forms of a step survive the fold.
func TestTaskLevelCmdIsOneStep(t *testing.T) {
	f, err := Decode([]byte("tasks:\n  line: {cmd: echo hi}\n  argv: {cmd: [echo, two words]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Tasks["line"].Cmds; len(got) != 1 || got[0].Cmd != "echo hi" || f.Tasks["line"].Single != nil {
		t.Errorf("line = %+v", got)
	}
	if got := f.Tasks["argv"].Cmds; len(got) != 1 || strings.Join(got[0].Argv, "|") != "echo|two words" {
		t.Errorf("argv = %+v", got)
	}
}

// References read the same way for every kind of name: bare is this file, a
// leading colon is the root of this file's tree, `global:` is absolute.
func TestReferencesReadTheSameEverywhere(t *testing.T) {
	root := &File{}
	inc := &File{Namespace: "db", Parent: root}
	g := &File{Namespace: "global:ssh", Global: true}
	gInc := &File{Namespace: "global:ssh:keys", Parent: g, Global: true}
	for _, tc := range []struct {
		f         *File
		ref, want string
	}{
		{root, "build", "build"},
		{inc, "up", "db:up"},
		{inc, ":build", "build"},
		{inc, "global:ssh:unlock", "global:ssh:unlock"},
		{g, "unlock", "global:ssh:unlock"},
		{gInc, ":unlock", "global:ssh:unlock"},
	} {
		if got := Reference(tc.f, tc.ref); got != tc.want {
			t.Errorf("Reference(%q, %q) = %q, want %q", tc.f.Namespace, tc.ref, got, tc.want)
		}
	}
	for _, tc := range []struct {
		f             *File
		ref, ns, name string
	}{
		{inc, "pi", "db", "pi"},
		{inc, ":pi", "", "pi"},
		{root, "global:homelab:pi", "global:homelab", "pi"},
	} {
		if ns, name := RouteRef(tc.f, tc.ref); ns != tc.ns || name != tc.name {
			t.Errorf("RouteRef(%q, %q) = %q %q, want %q %q", tc.f.Namespace, tc.ref, ns, name, tc.ns, tc.name)
		}
	}
}
