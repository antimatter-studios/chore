package loader

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPreconditionTasksValidatedAtLoad(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"missing", "tasks:\n  work:\n    preconditions: [{task: missing}]\n", "not a task"},
		{"routed", "routes:\n  r: [{host: localhost}]\ntasks:\n  work:\n    preconditions: [{task: check}]\n  check:\n    route: r\n    cmd: 'true'\n", "must run on this machine"},
		{"cycle", "tasks:\n  a:\n    preconditions: [{task: b}]\n  b:\n    preconditions: [{task: a}]\n", "cycle"},
		{"dependency cycle", "tasks:\n  a:\n    preconditions: [{task: b}]\n  b:\n    deps: [a]\n", "cycle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{"chores.yml": tc.body})
			_, err := Load(filepath.Join(dir, "chores.yml"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("load = %v, want %s", err, tc.want)
			}
		})
	}
}
