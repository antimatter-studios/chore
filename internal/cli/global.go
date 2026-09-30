package cli

import (
	"io"
	"strings"

	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/global"
	"github.com/antimatter-studios/chore/internal/ui"
)

// globalDirOverride lets a test point the loader at a temp directory without
// moving the user's real HOME around. Empty everywhere else.
var globalDirOverride string

// dialerOverride lets a test route tasks through its own ssh server and agent.
var dialerOverride *global.Dialer

// loadGlobals reads every file in global.d. Called for EVERY run, project or
// global, so anything declared there can be named from anywhere.
func loadGlobals() (*global.Set, error) {
	dir := globalDirOverride
	if dir == "" {
		d, err := global.Dir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	return global.Load(dir)
}

// globalMain answers everything under `global:`. The prefix only says which
// file a task is in; once that file is found the task runs exactly as a
// project's does, through runProject — arguments, flags, --help, --dry, `--`.
//
//	chore global:                   the files installed here
//	chore global:homelab:           the tasks in one
//	chore global:homelab:k3s:pods   run one
func globalMain(stdout, stderr io.Writer, out, errUI *ui.UI, rest []string, opts options) int {
	set, err := loadGlobals()
	if err != nil {
		errUI.Errorf("%v", err)
		return 1
	}
	address := strings.TrimPrefix(rest[0], chorefile.GlobalPrefix)
	ns, task, _ := strings.Cut(address, ":")

	// A trailing colon, or nothing at all, is a request to LOOK rather than to
	// run — `chore global:homelab:` reads as "what is in here?".
	switch {
	case address == "":
		set.ListNamespaces(out.Writer())
		return 0
	case task == "":
		p, err := set.Project(ns)
		if err != nil {
			errUI.Errorf("%v", err)
			return 1
		}
		if !strings.HasSuffix(address, ":") {
			errUI.Errorf("global:%s names a file, not a task — `chore global:%s:` lists what is in it", ns, ns)
			return 1
		}
		out.List(listing(p))
		return 0
	}

	p, err := set.Project(ns)
	if err != nil {
		errUI.Errorf("%v", err)
		return 1
	}
	if _, ok := p.Tasks[rest[0]]; !ok {
		errUI.Errorf("no task %q in global:%s: (%s)%s", task, ns, p.Root.Path, globalSuggest(set, ns))
		return 1
	}
	return runProject(p, rest, opts, stdout, stderr, out, errUI)
}

func globalSuggest(set *global.Set, ns string) string {
	tasks, _ := set.Tasks(ns)
	if len(tasks) == 0 {
		return ""
	}
	names := make([]string, len(tasks))
	for i, t := range tasks {
		names[i] = strings.TrimPrefix(t.Name, chorefile.GlobalPrefix+ns+":")
	}
	return " — it has: " + strings.Join(names, ", ")
}
