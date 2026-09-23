package global

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/antimatter-studios/chore/internal/loader"
)

// Dir returns the directory global task namespaces are read from.
func Dir() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "chore", "global.d"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to read global tasks from: %w", err)
	}
	return filepath.Join(home, ".config", "chore", "global.d"), nil
}

// Load reads every namespace taskfile in dir. Missing directories are the
// ordinary state of a user without global tasks; present but invalid files are
// reported so a broken taskfile cannot silently disappear.
func Load(dir string) (*Set, error) {
	set := &Set{Dir: dir, Namespaces: map[string]*Namespace{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return set, nil
		}
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !isTaskfile(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		name, project, err := loader.LoadGlobal(path)
		if err != nil {
			return nil, err
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("%s: global taskfile needs a `name:` namespace", path)
		}
		if strings.ContainsAny(name, ": \t") {
			return nil, fmt.Errorf("%s: namespace name %q cannot contain a colon or space", path, name)
		}
		if prior, duplicate := set.Namespaces[name]; duplicate {
			return nil, fmt.Errorf("two global taskfiles are named %q: %s and %s", name, prior.Path, path)
		}
		set.Namespaces[name] = &Namespace{Name: name, Path: path, Project: project}
	}
	return set, nil
}

func isTaskfile(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}
