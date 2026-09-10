package cli

import (
	"os"
	"strings"
	"syscall"

	"github.com/antimatter-studios/chore/internal/global"
	"github.com/antimatter-studios/chore/internal/run"
	"github.com/antimatter-studios/chore/internal/ui"
)

// globalPrefix is what makes an address global, and it is required rather than
// inferred. See the comment at its use in Main.
const globalPrefix = "global:"

// globalDirOverride lets a test point the loader at a temp directory without
// moving the user's real HOME around. Empty everywhere else.
var globalDirOverride string

// globalMain answers everything under `global:`. Three shapes, and which one it
// is falls out of the address rather than out of a flag:
//
//	chore global:                    the namespaces installed here
//	chore global:homelab:            the tasks in one
//	chore global:homelab:k3s:pods    run one
func globalMain(out, errUI *ui.UI, rest []string, opts options) int {
	address := strings.TrimPrefix(rest[0], globalPrefix)

	dir := globalDirOverride
	if dir == "" {
		d, err := global.Dir()
		if err != nil {
			errUI.Errorf("%v", err)
			return 1
		}
		dir = d
	}
	set, err := global.Load(dir)
	if err != nil {
		errUI.Errorf("%v", err)
		return 1
	}

	r := &global.Runner{Out: out.Writer(), Err: errUI.Writer(), In: os.Stdin}

	// A trailing colon, or nothing at all, is a request to LOOK rather than to
	// run — `chore global:homelab:` reads as "what is in here?" and answering it
	// with "no such task" would be answering a question nobody asked.
	switch {
	case address == "":
		r.ListNamespaces(set)
		return 0
	case strings.HasSuffix(address, ":") && strings.Count(strings.TrimSuffix(address, ":"), ":") == 0:
		if err := r.ListTasks(set, strings.TrimSuffix(address, ":")); err != nil {
			errUI.Errorf("%v", err)
			return 1
		}
		return 0
	}

	if opts.dry {
		return globalDryRun(errUI, out, set, address)
	}

	// The same signal handling a project run gets: SIGINT and SIGTERM cancel, and
	// a second one is not caught. A forwarded port needs it more than anything
	// else chore runs — it is goroutines inside this process rather than a child
	// in its own process group, so nothing else would stop it.
	ctx, stopSignals, interruptedBy := signalContext()
	defer stopSignals()

	if err := r.Run(ctx, set, address); err != nil {
		if sig := interruptedBy(); sig != 0 {
			verb := "interrupted"
			if sig == syscall.SIGTERM {
				verb = "terminated"
			}
			errUI.Errorf("%s: stopped %s", verb, rest[0])
			return 128 + int(sig)
		}
		errUI.Errorf("%v", err)
		return run.ExitCode(err)
	}
	return 0
}

// globalDryRun prints what the task would do without touching the network: the
// route it would travel, hop by hop, and the command or tunnel at the end of it.
//
// Worth having for these more than for a local task. A route is the part a
// reader most often gets wrong — every hop after the first is resolved from the
// hop before it — and this is the cheapest way to check the file says what its
// author meant before a machine is involved.
func globalDryRun(errUI, out *ui.UI, set *global.Set, address string) int {
	if err := set.DryRun(out.Writer(), address); err != nil {
		errUI.Errorf("%v", err)
		return 1
	}
	return 0
}
