package examples

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain keeps the machine's own ~/.config/chore/global.d out of these tests.
// Every chore run loads every global file, so without this a test's result
// would depend on what happens to be installed where it runs.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "chore-test-config")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "no-config"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
