package global

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// dialTimeout bounds ONE hop. A route of five hops is therefore bounded by five
// of these and not by one budget shared between them, which is the right shape:
// a hop that is slow because it is far away should not spend the budget of the
// hop after it.
const dialTimeout = 20 * time.Second

// Dialer holds what every hop needs and nothing about any particular one.
//
// The two fields exist to be overridden by tests, which stand up a real ssh
// server and a real agent rather than faking either — the fold below is the
// whole feature, and a test that mocked the transport would be testing nothing.
type Dialer struct {
	// AgentSock is $SSH_AUTH_SOCK when empty.
	AgentSock string
	// KnownHosts is ~/.ssh/known_hosts when empty.
	KnownHosts string
}

// Dial walks a route and returns a client at the far end.
//
// The fold is the whole thing: hop 1 is dialled from this machine, and every hop
// after it is dialled THROUGH the client before it — `prev.Dial` opens a TCP
// connection from that machine, and ssh.NewClientConn speaks SSH over it. One hop
// and five are the same loop, and a direct connection is a route of length one.
//
// This is also why `127.0.0.1` in hop 2 means hop 1's loopback and not ours.
func (d Dialer) Dial(ctx context.Context, routeName string, route Route) (*ssh.Client, error) {
	auth, err := d.agentAuth()
	if err != nil {
		return nil, err
	}
	hostKey, err := d.hostKeyCallback()
	if err != nil {
		return nil, err
	}

	var client *ssh.Client
	for i, hop := range route {
		cfg := &ssh.ClientConfig{
			User:            hop.User,
			Auth:            []ssh.AuthMethod{auth},
			HostKeyCallback: hostKey,
			Timeout:         dialTimeout,
		}
		var conn net.Conn
		if client == nil {
			// The first hop is the only one this machine dials itself.
			dialer := net.Dialer{Timeout: dialTimeout}
			conn, err = dialer.DialContext(ctx, "tcp", hop.Addr())
		} else {
			// And every later one is dialled from the hop before it. Note the
			// absence of ctx: x/crypto/ssh has no context-aware Dial, so a hop that
			// hangs is bounded by cfg.Timeout below rather than by cancellation.
			conn, err = client.Dial("tcp", hop.Addr())
		}
		if err != nil {
			closeQuietly(client)
			return nil, hopError(routeName, i, hop, err)
		}
		sshConn, chans, reqs, err := ssh.NewClientConn(conn, hop.Addr(), cfg)
		if err != nil {
			_ = conn.Close()
			closeQuietly(client)
			return nil, hopError(routeName, i, hop, err)
		}
		client = ssh.NewClient(sshConn, chans, reqs)
	}
	return client, nil
}

// hopError names the route and the hop by NUMBER as well as by address.
//
// Which is not fussiness: a route commonly has more than one 127.0.0.1 in it
// meaning different machines — one being some earlier hop's loopback — so "dial
// 127.0.0.1:2222 failed" leaves the reader unable to tell which machine could
// not be reached, or from where.
func hopError(routeName string, i int, hop Hop, err error) error {
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) {
		return fmt.Errorf("route %s, hop %d (%s@%s): %s", routeName, i+1, hop.User, hop.Addr(), explainKeyError(hop, keyErr))
	}
	return fmt.Errorf("route %s, hop %d (%s@%s): %w", routeName, i+1, hop.User, hop.Addr(), err)
}

// explainKeyError says which of the two very different host-key failures this is,
// because the answers are opposites: one wants a line added, the other wants
// somebody to stop and think.
func explainKeyError(hop Hop, err *knownhosts.KeyError) string {
	if len(err.Want) > 0 {
		return fmt.Sprintf("the host key CHANGED — known_hosts has a different key for %s."+
			" Either the machine was rebuilt, or this is not the machine you think it is."+
			" chore will not continue past this, exactly as ssh would not", hop.Addr())
	}
	return fmt.Sprintf("host key not in known_hosts. Connect once with ssh so the key is recorded" +
		" — chore checks the same file and will not invent an exception." +
		" Note that a hop deeper in a route is a loopback ON THE HOP BEFORE IT," +
		" so its known_hosts entry is written by an ssh that went the same way")
}

// agentAuth is the only authentication chore offers, and that is the point: a
// key never passes through this program, so whatever manages the user's secrets
// keeps working without chore knowing it exists.
func (d Dialer) agentAuth() (ssh.AuthMethod, error) {
	sock := d.AgentSock
	if sock == "" {
		sock = os.Getenv("SSH_AUTH_SOCK")
	}
	if sock == "" {
		return nil, errors.New("no SSH_AUTH_SOCK: chore authenticates through your ssh-agent and never handles a key itself — start an agent and add the key you would use for `ssh`")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("connecting to the ssh-agent at %s: %w", sock, err)
	}
	return ssh.PublicKeysCallback(agent.NewClient(conn).Signers), nil
}

// hostKeyCallback verifies against the same file ssh does.
//
// Not InsecureIgnoreHostKey, and the temptation to reach for it will be real
// while debugging a three-hop chain — which is exactly when it matters, because a
// route's later hops are reached through a machine you have already trusted and
// an unverified one there is a place to stand. Skipping the check would be a
// downgrade from what plain ssh already gives, for a feature whose entire value
// is being able to trust it from a machine you are only visiting.
func (d Dialer) hostKeyCallback() (ssh.HostKeyCallback, error) {
	path := d.KnownHosts
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("no home directory to find known_hosts in: %w", err)
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s does not exist: chore checks host keys against the same file ssh does, so connect once with ssh first", path)
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return cb, nil
}

// Exec runs argv at the far end of a client and streams its output.
//
// The argv is quoted into one string here, and it is worth being clear about
// why, because the shape of the field suggests otherwise: the SSH protocol's
// exec request carries a single COMMAND STRING, which the far end hands to the
// login shell. There is no argv on the wire and no way to avoid quoting — the
// choice is only whether chore does it once, correctly, or whether every task
// does it by hand. Taking argv in the file is chore doing it.
func Exec(ctx context.Context, client *ssh.Client, argv []string, in *os.File, out, errOut interface{ Write([]byte) (int, error) }) error {
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("opening a session: %w", err)
	}
	defer session.Close()

	session.Stdout, session.Stderr = out, errOut
	if in != nil {
		session.Stdin = in
	}

	if err := session.Start(quoteArgv(argv)); err != nil {
		return fmt.Errorf("starting %s: %w", argv[0], err)
	}

	// Wait in a goroutine so an interrupt can close the session out from under it:
	// Session.Wait has no context, and a remote command that never ends would
	// otherwise ignore Ctrl-C entirely.
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		return translateExit(err)
	case <-ctx.Done():
		// Ask the far end to stop, then stop waiting for it. SIGINT is best
		// effort — OpenSSH's server has historically ignored signal requests —
		// so closing the session is what actually ends this.
		_ = session.Signal(ssh.SIGINT)
		_ = session.Close()
		return ctx.Err()
	}
}

// quoteArgv renders an argv as one POSIX shell word list: every argument in
// single quotes, with an embedded quote written the only way a shell accepts it.
// A round trip through `sh -c` therefore yields the argv that went in.
func quoteArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// ExitError is a remote command that ran and failed, carrying its status so
// chore can exit with the same one. The method name is the contract
// internal/shell's ExitCode reads: an error that knows its own status answers
// for it.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }
func (e *ExitError) ExitCode() int { return e.Code }
func (e *ExitError) Unwrap() error { return e.Err }

// translateExit turns a session's ending into chore's terms: a command that ran
// and failed carries its status, a command killed by a signal reports 128+signal
// as every shell does, and anything else is an operational failure passed
// through.
func translateExit(err error) error {
	if err == nil {
		return nil
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		if sig := exit.Signal(); sig != "" {
			if n, ok := signalNumbers[ssh.Signal(sig)]; ok {
				return &ExitError{Code: 128 + n, Err: err}
			}
		}
		return &ExitError{Code: exit.ExitStatus(), Err: err}
	}
	var missing *ssh.ExitMissingError
	if errors.As(err, &missing) {
		return fmt.Errorf("the remote command ended without reporting a status: %w", err)
	}
	return err
}

// signalNumbers maps the names the SSH protocol uses to the numbers a shell
// reports as 128+n. Only the ones a remote command realistically dies of.
var signalNumbers = map[ssh.Signal]int{
	ssh.SIGHUP: 1, ssh.SIGINT: 2, ssh.SIGQUIT: 3, ssh.SIGILL: 4,
	ssh.SIGABRT: 6, ssh.SIGFPE: 8, ssh.SIGKILL: 9, ssh.SIGSEGV: 11,
	ssh.SIGPIPE: 13, ssh.SIGALRM: 14, ssh.SIGTERM: 15,
}

func closeQuietly(c *ssh.Client) {
	if c != nil {
		_ = c.Close()
	}
}
