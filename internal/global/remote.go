package global

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// chore:manual ssh-tasks
// title: SSH tasks and port forwarding
// summary: optional remote execution and local TCP forwarding for global tasks
// aliases: remote-tasks port-forwarding
// order: 20
//
// # SSH tasks and port forwarding
//
// SSH is an optional execution form for a global task. Ordinary global tasks
// remain ordinary chore tasks. A task using `route:` names a route declared in
// the same global taskfile and uses exactly one of `exec:` or `forward:`:
//
// ```yaml
// name: homelab
// routes:
//   pi:
//     - { host: s1.example.com, port: 10022, user: root }
//     - { host: 127.0.0.1, port: 2222, user: chris }
// tasks:
//   k3s:pods:
//     route: pi
//     exec: [kubectl, get, pods, -A]
//   k3s:proxy:
//     route: pi
//     forward: { remote: 127.0.0.1:6443, local: 127.0.0.1:6443 }
// ```
//
// Every hop after the first is dialled from the previous hop. SSH authentication
// uses `$SSH_AUTH_SOCK`; host keys are checked against `~/.ssh/known_hosts`.
// `exec:` is quoted as one POSIX shell command because SSH carries a command
// string, not an argv array. `forward:` binds locally and stays in the foreground
// until interrupted. `--dry` prints the route and remote action without dialing.
//
// Remote tasks accept `desc:`, `aliases:`, `internal:`, `route:`, and one of
// `exec:`/`forward:`. Other task behavior belongs to ordinary tasks, and is
// refused for this execution form.

// RemoteRunner runs the SSH execution form of a global task.
type RemoteRunner struct {
	Out    io.Writer
	Err    io.Writer
	In     *os.File
	Dialer Dialer
}

func IsRemote(task *chorefile.Task) bool {
	return task != nil && (task.Route != "" || len(task.Exec) > 0 || task.Forward != nil)
}

func (r *RemoteRunner) Run(ctx context.Context, namespace *Namespace, task *chorefile.Task) error {
	route := namespace.Routes[task.Route]
	client, err := r.Dialer.Dial(ctx, task.Route, route)
	if err != nil {
		return err
	}
	defer client.Close()
	if task.Forward != nil {
		return RunForward(ctx, client, *task.Forward, func(addr string) {
			fmt.Fprintf(r.Err, "chore: %s -> %s via route %s. Ctrl-C to stop.\n", addr, task.Forward.Remote, task.Route)
		})
	}
	return Exec(ctx, client, task.Exec, r.In, r.Out, r.Err)
}

func (s *Set) DryRunRemote(w io.Writer, namespace *Namespace, task *chorefile.Task) {
	fmt.Fprintf(w, "%s  (%s)\n", Address(namespace.Name, task.Name), namespace.Path)
	fmt.Fprintf(w, "route %s:\n", task.Route)
	for i, hop := range namespace.Routes[task.Route] {
		from := "this machine"
		if i > 0 {
			from = fmt.Sprintf("hop %d (%s)", i, namespace.Routes[task.Route][i-1].Host)
		}
		addr := net.JoinHostPort(hop.Host, strconv.Itoa(hop.Port))
		fmt.Fprintf(w, "  hop %d  %s@%s\n", i+1, hop.User, addr)
		fmt.Fprintf(w, "         dialled from %s\n", from)
	}
	if task.Forward != nil {
		fmt.Fprintf(w, "forward: %s on this machine -> %s at the far end\n", task.Forward.Local, task.Forward.Remote)
		return
	}
	fmt.Fprintf(w, "exec:    %s\n", quoteArgv(task.Exec))
}

func validateRemoteTask(name string, task *chorefile.Task) error {
	if len(task.Cmds) > 0 || len(task.Deps) > 0 || len(task.Args) > 0 || len(task.Before) > 0 ||
		len(task.OnSuccess) > 0 || len(task.OnFailure) > 0 || len(task.After) > 0 || len(task.OnTimeout) > 0 ||
		task.Dir != "" || task.Vars != nil || task.Env != nil || task.Dotenv != nil || task.Timeout > 0 ||
		task.Run != "" || len(task.Requires) > 0 || len(task.Platforms) > 0 || task.Status != nil ||
		task.Sources != nil || task.Generates != nil || task.ChildHooks != nil || task.Summary != "" ||
		task.Silent || task.Verbose || task.Interactive || task.IgnoreError {
		return fmt.Errorf("remote task %q currently accepts only desc, aliases, internal, route, exec and forward", name)
	}
	if task.Route == "" {
		return fmt.Errorf("remote task %q needs a `route:`", name)
	}
	if len(task.Exec) == 0 && task.Forward == nil || len(task.Exec) > 0 && task.Forward != nil {
		return fmt.Errorf("remote task %q must set exactly one of `exec:` or `forward:`", name)
	}
	if task.Forward != nil && (strings.TrimSpace(task.Forward.Local) == "" || strings.TrimSpace(task.Forward.Remote) == "") {
		return fmt.Errorf("remote task %q needs both forward addresses", name)
	}
	return nil
}
