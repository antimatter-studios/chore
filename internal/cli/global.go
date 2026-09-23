package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/antimatter-studios/chore/internal/buildinfo"
	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/global"
	"github.com/antimatter-studios/chore/internal/run"
	"github.com/antimatter-studios/chore/internal/ui"
)

const globalPrefix = "global:"

// globalDirOverride lets CLI tests point the loader at a temporary directory.
var globalDirOverride string

func globalMain(stdout, stderr io.Writer, out, errUI *ui.UI, rest []string, opts options) int {
	dir := globalDirOverride
	if dir == "" {
		var err error
		dir, err = global.Dir()
		if err != nil {
			errUI.Errorf("%v", err)
			return 1
		}
	}
	set, err := global.Load(dir)
	if err != nil {
		errUI.Errorf("%v", err)
		return 1
	}
	address := strings.TrimPrefix(rest[0], globalPrefix)
	if address == "" {
		if len(set.Namespaces) == 0 {
			out.Raw(fmt.Sprintf("no global task namespaces in %s\n", dir))
			return 0
		}
		out.Raw("global task namespaces:\n")
		for _, name := range set.Names() {
			out.Raw(fmt.Sprintf("  global:%s:\n", name))
		}
		return 0
	}

	namespaceName, taskName, hasTask := strings.Cut(address, ":")
	namespace, ok := set.Namespaces[namespaceName]
	if !ok {
		_, _, _, err := set.Lookup(global.Address(namespaceName, "_"))
		errUI.Errorf("%v", err)
		return 1
	}
	if !hasTask || taskName == "" {
		listOpts := opts
		listOpts.list = true
		return runProject(namespace.Project, nil, listOpts, stdout, stderr, out, errUI)
	}
	if _, ok := namespace.Project.Tasks[taskName]; !ok {
		_, _, _, err := set.Lookup(global.Address(namespaceName, taskName))
		errUI.Errorf("%v", err)
		return 1
	}

	// Strip only the namespace prefix. Colons inside the actual task name retain
	// their ordinary meaning to chore's namespaced task resolver.
	normalized := append([]string{taskName}, rest[1:]...)
	if task := namespace.Project.Tasks[taskName]; global.IsRemote(task) {
		if opts.list || opts.help || wantsHelp(rest[1:]) {
			return runProject(namespace.Project, normalized, opts, stdout, stderr, out, errUI)
		}
		return runRemoteTask(stdout, stderr, errUI, namespace, task, rest[1:], opts)
	}
	return runProject(namespace.Project, normalized, opts, stdout, stderr, out, errUI)
}

func runRemoteTask(stdout, stderr io.Writer, errUI *ui.UI, namespace *global.Namespace, task *chorefile.Task, args []string, opts options) int {
	if len(args) > 0 || len(opts.cliArgs) > 0 {
		errUI.Errorf("%s takes no command-line arguments; its remote argv is declared in the global taskfile", task.Name)
		return 2
	}
	if err := checkChoreVersion(namespace.Project, buildinfo.Get(Version, BuildDate)); err != nil {
		errUI.Errorf("%v", err)
		return 1
	}
	if opts.dry {
		(&global.Set{}).DryRunRemote(stdout, namespace, task)
		return 0
	}
	ctx, stopSignals, interruptedBy := signalContext()
	defer stopSignals()
	runner := &global.RemoteRunner{Out: stdout, Err: stderr, In: os.Stdin}
	err := runner.Run(ctx, namespace, task)
	if err == nil {
		return 0
	}
	if sig := interruptedBy(); sig != 0 {
		verb := "interrupted"
		if sig == syscall.SIGTERM {
			verb = "terminated"
		}
		errUI.Errorf("%s: stopped %s", verb, global.Address(namespace.Name, task.Name))
		return 128 + int(sig)
	}
	errUI.Errorf("%v", err)
	return run.ExitCode(err)
}
