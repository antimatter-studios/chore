package global

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
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
func (d Dialer) Dial(ctx context.Context, routeName string, hops []Hop) (*ssh.Client, error) {
	auth, err := d.agentAuth()
	if err != nil {
		return nil, err
	}
	hostKey, err := d.hostKeyCallback()
	if err != nil {
		return nil, err
	}

	var client *ssh.Client
	for i, hop := range hops {
		cfg := &ssh.ClientConfig{
			User:            hop.User,
			Auth:            []ssh.AuthMethod{auth},
			HostKeyCallback: hostKey,
			Timeout:         dialTimeout,
			// Ask for the key types known_hosts actually holds for this host. See
			// knownAlgorithms: without it a server that offers several types hands
			// over whichever x/crypto prefers, which is rarely the one recorded.
			HostKeyAlgorithms: knownAlgorithms(hostKey, hop.Addr()),
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
	return withoutPortFallback(cb), nil
}

// withoutPortFallback adds the lookup rule OpenSSH has and x/crypto does not:
// when there is no entry for `[host]:port`, try the bare `host`.
//
// Reading the same FILE is not the same as doing the same LOOKUP, and the
// difference is not academic. Measured against a real route: a host had been
// recorded on port 22, the route reaches it on 10022, `ssh` connected happily —
// its own debug output says `found matching key w/out port` — and chore refused,
// claiming a key was unknown that ssh had just accepted from the very same file.
// A tool that says it checks what ssh checks has to answer the same way, or the
// promise is worse than useless: it teaches the user their file is wrong.
//
// The fallback is deliberately narrow. It applies only when there is NO entry for
// the address at all, never when one exists and disagrees — a changed key stays a
// refusal, because that is the case the check exists for.
func withoutPortFallback(cb ssh.HostKeyCallback) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) || len(keyErr.Want) > 0 {
			// Either a different kind of failure, or — the important one — an
			// entry that exists and does not match. Never fall back past that.
			return err
		}
		host, _, splitErr := net.SplitHostPort(hostname)
		if splitErr != nil {
			return err
		}
		// Port 22 rather than a bare host, and the difference is not cosmetic:
		// x/crypto's check() calls net.SplitHostPort on whatever it is given and
		// fails outright on an address without one, so a bare host never reaches
		// the lookup at all. Port 22 is what its Normalize turns into the
		// unbracketed entry — which is exactly the line OpenSSH means by "found
		// matching key w/out port".
		if bare := cb(net.JoinHostPort(host, "22"), remote, key); bare == nil {
			return nil
		}
		return err
	}
}

// knownAlgorithms returns the host key types known_hosts holds for an address,
// in the order to ask for them.
//
// This is what OpenSSH does and x/crypto does not, and leaving it out produces a
// failure that reads as an attack. A server offers several host key types;
// x/crypto picks by its own fixed preference (ECDSA first), while OpenSSH asks
// for the type it has already recorded. So against a host recorded as ed25519,
// chore was handed an ECDSA key, found the ed25519 entry, and reported that the
// host key had CHANGED — for a host `ssh` connects to happily from the same file,
// with nothing wrong anywhere.
//
// The lookup is done by asking the callback about a key it cannot possibly match
// and reading the entries back out of the KeyError it returns. x/crypto exposes
// no way to enumerate the file, and reimplementing its parser — hashed entries,
// certificate authorities, revocations, wildcards — to answer one question would
// be a second source of truth for the thing that must not have one.
func knownAlgorithms(cb ssh.HostKeyCallback, address string) []string {
	// Any key the file cannot contain will do; a fresh one cannot be in it.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil
	}
	probe, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil
	}
	// The callback splits this before it looks at anything else, so it has to
	// parse — its value is never used, since a non-empty address wins over it.
	unused := &net.TCPAddr{IP: net.IPv4zero, Port: 0}

	var known []string
	for _, addr := range []string{address, portTwentyTwo(address)} {
		if addr == "" {
			continue
		}
		var keyErr *knownhosts.KeyError
		if errors.As(cb(addr, unused, probe.PublicKey()), &keyErr) {
			for _, want := range keyErr.Want {
				known = append(known, expandRSA(want.Key.Type())...)
			}
		}
		if len(known) > 0 {
			// The exact address wins outright: falling through to the bare host
			// would mix in types recorded for a DIFFERENT port on the same host.
			break
		}
	}
	return dedupe(known)
}

// portTwentyTwo rewrites an address to the port-22 form, which is how the
// unbracketed known_hosts entry is addressed. Empty when there is no port to
// replace, so the caller skips it.
func portTwentyTwo(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "22" {
		return ""
	}
	return net.JoinHostPort(host, "22")
}

// expandRSA turns the one type known_hosts records for an RSA key into the three
// algorithms a modern server may negotiate with it. The file says `ssh-rsa`
// whichever signature algorithm is used, so asking for only that name would
// refuse the SHA-2 signatures every current sshd prefers.
func expandRSA(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Exec runs argv at the far end of a client and streams its output.
//
// The argv is quoted into one string here, and it is worth being clear about
// why, because the shape of the field suggests otherwise: the SSH protocol's
// exec request carries a single COMMAND STRING, which the far end hands to the
// login shell. There is no argv on the wire and no way to avoid quoting — the
// choice is only whether chore does it once, correctly, or whether every task
// does it by hand. Taking argv in the file is chore doing it.
func Exec(ctx context.Context, client *ssh.Client, command Cmd, in *os.File, out, errOut interface{ Write([]byte) (int, error) }) error {
	return runSSHCommand(ctx, client, command, in, out, errOut, false)
}

// ExecPTY runs a remote command with an allocated terminal. It is intended for
// interactive shells: input is passed through in raw mode, terminal dimensions
// are kept in sync, and the caller's terminal settings are restored on return.
func ExecPTY(ctx context.Context, client *ssh.Client, command Cmd, in *os.File, out, errOut interface{ Write([]byte) (int, error) }) error {
	return runSSHCommand(ctx, client, command, in, out, errOut, true)
}

func runSSHCommand(ctx context.Context, client *ssh.Client, command Cmd, in *os.File, out, errOut interface{ Write([]byte) (int, error) }, pty bool) error {
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("opening a session: %w", err)
	}
	defer session.Close()

	session.Stdout, session.Stderr = out, errOut
	if in != nil {
		session.Stdin = in
	}

	var restoreTerminal func()
	if pty {
		width, height := 80, 24
		localTTY := in != nil && term.IsTerminal(int(in.Fd()))
		if localTTY {
			if w, h, sizeErr := term.GetSize(int(in.Fd())); sizeErr == nil {
				width, height = w, h
			}
		}
		termType := os.Getenv("TERM")
		if termType == "" || termType == "dumb" {
			termType = "xterm-256color"
		}
		modes := ssh.TerminalModes{
			ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400,
		}
		if err := session.RequestPty(termType, height, width, modes); err != nil {
			return fmt.Errorf("requesting remote PTY: %w", err)
		}
		if localTTY {
			state, rawErr := term.MakeRaw(int(in.Fd()))
			if rawErr != nil {
				return fmt.Errorf("putting local terminal in raw mode: %w", rawErr)
			}
			restoreTerminal = func() { _ = term.Restore(int(in.Fd()), state) }
			defer restoreTerminal()
			defer watchTerminalResize(session, in)()
		}
	}

	// One string either way, because that is all the protocol carries. An argv is
	// quoted so the far end's shell reconstructs it; a shell line is handed over
	// as written, which is what makes a pipe possible.
	if err := session.Start(command.String()); err != nil {
		return fmt.Errorf("starting %s: %w", command, err)
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

// watchTerminalResize forwards SIGWINCH to the remote PTY until the session
// context ends. A local terminal resized while an SSH shell is open should
// resize the far end too, as OpenSSH does.
func watchTerminalResize(session *ssh.Session, in *os.File) func() {
	resized := make(chan os.Signal, 1)
	signal.Notify(resized, syscall.SIGWINCH)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			case <-resized:
				width, height, err := term.GetSize(int(in.Fd()))
				if err == nil {
					_ = session.WindowChange(height, width)
				}
			}
		}
	}()
	return func() {
		signal.Stop(resized)
		close(done)
		<-stopped
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
