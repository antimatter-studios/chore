package global

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/antimatter-studios/chore/internal/chorefile"
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
		if err := validateRemote(project); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if prior, duplicate := set.Namespaces[name]; duplicate {
			return nil, fmt.Errorf("two global taskfiles are named %q: %s and %s", name, prior.Path, path)
		}
		set.Namespaces[name] = &Namespace{Name: name, Path: path, Project: project, Routes: project.Root.Routes}
	}
	return set, nil
}

func validateRemote(project *chorefile.Project) error {
	userName := ""
	if u, err := user.Current(); err == nil {
		userName = u.Username
	}
	for routeName, route := range project.Root.Routes {
		if len(route) == 0 {
			return fmt.Errorf("route %q has no hops", routeName)
		}
		for i := range route {
			hop := &route[i]
			if strings.TrimSpace(hop.Host) == "" {
				return fmt.Errorf("route %q, hop %d needs a `host:`", routeName, i+1)
			}
			if hop.Port == 0 {
				hop.Port = 22
			}
			if hop.Port < 1 || hop.Port > 65535 {
				return fmt.Errorf("route %q, hop %d has invalid port %d", routeName, i+1, hop.Port)
			}
			if hop.User == "" {
				hop.User = userName
			}
		}
	}
	for name, task := range project.Tasks {
		if task.Route == "" && len(task.Exec) == 0 && task.Forward == nil {
			continue
		}
		if err := validateRemoteTask(name, task); err != nil {
			return err
		}
		if task.Route == "" {
			return fmt.Errorf("task %q needs a `route:`", name)
		}
		if _, ok := project.Root.Routes[task.Route]; !ok {
			return fmt.Errorf("task %q names unknown route %q", name, task.Route)
		}
		if (len(task.Exec) > 0) == (task.Forward != nil) {
			return fmt.Errorf("task %q must set exactly one of `exec:` or `forward:`", name)
		}
		if task.Forward != nil {
			for _, side := range []struct{ name, address string }{{"remote", task.Forward.Remote}, {"local", task.Forward.Local}} {
				if _, _, err := net.SplitHostPort(side.address); err != nil {
					return fmt.Errorf("task %q forward %s %q is not host:port", name, side.name, side.address)
				}
			}
		}
	}
	return nil
}

func isTaskfile(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}
