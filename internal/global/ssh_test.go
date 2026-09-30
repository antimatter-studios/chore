package global

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/antimatter-studios/chore/internal/chorefile"
	"github.com/antimatter-studios/chore/internal/global/sshtest"
	"github.com/antimatter-studios/chore/internal/shell"
)

// One hop is a route of length one, which is the claim the fold rests on: there
// is no separate "direct connection" path to get wrong.
func TestDialOneHopRunsACommand(t *testing.T) {
	sock, pub := sshtest.Agent(t)
	server := sshtest.NewServer(t, pub)
	d := Dialer{AgentSock: sock, KnownHosts: sshtest.KnownHosts(t, server)}

	client, err := d.Dial(context.Background(), "direct", []chorefile.Hop{sshtest.Hop(t, server)})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	var out, errOut bytes.Buffer
	if err := Exec(context.Background(), client, QuoteArgv([]string{"echo", "hello"}), nil, &out, &errOut); err != nil {
		t.Fatalf("Exec: %v (stderr %q)", err, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != `'echo' 'hello'` {
		t.Errorf("the far end received %q", got)
	}
}

// The headline: hop 2 is dialled FROM hop 1, not from this machine. The proof is
// that the second server is reachable only through the first — it is asked to
// open the TCP connection, and the second SSH handshake runs over that channel.
func TestDialFoldsThroughEveryHop(t *testing.T) {
	sock, pub := sshtest.Agent(t)
	first := sshtest.NewServer(t, pub)
	second := sshtest.NewServer(t, pub)
	d := Dialer{AgentSock: sock, KnownHosts: sshtest.KnownHosts(t, first, second)}

	client, err := d.Dial(context.Background(), "pi", []chorefile.Hop{sshtest.Hop(t, first), sshtest.Hop(t, second)})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	var out, errOut bytes.Buffer
	if err := Exec(context.Background(), client, QuoteArgv([]string{"kubectl", "get", "pods", "-A"}), nil, &out, &errOut); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	// The command ran at the END of the route and nowhere else. A fold that
	// stopped early, or one that dialled hop 2 from here, would put it on the
	// wrong server — and both are silent failures without this assertion.
	if len(second.Commands()) != 1 {
		t.Errorf("the last hop ran %d commands, want 1", len(second.Commands()))
	}
	if n := len(first.Commands()); n != 0 {
		t.Errorf("an intermediate hop ran %d commands, want 0 — it is a way through, not a destination", n)
	}
}

// A route commonly has more than one loopback in it meaning different machines,
// so an error that names only the address is not enough to act on.
func TestAFailedHopNamesItsNumber(t *testing.T) {
	sock, pub := sshtest.Agent(t)
	first := sshtest.NewServer(t, pub)

	// A port nothing is listening on, reached FROM the first hop.
	dead := chorefile.Hop{Host: "127.0.0.1", Port: closedPort(t), User: "tester"}
	d := Dialer{AgentSock: sock, KnownHosts: sshtest.KnownHosts(t, first)}

	_, err := d.Dial(context.Background(), "pi", []chorefile.Hop{sshtest.Hop(t, first), dead})
	if err == nil {
		t.Fatal("dialling a closed port must fail")
	}
	for _, want := range []string{"route pi", "hop 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// Host keys are checked against the same file ssh uses, and an unknown one stops
// the connection. The temptation to skip this while debugging a three-hop chain
// is exactly why it is pinned by a test.
func TestAnUnknownHostKeyIsRefused(t *testing.T) {
	sock, pub := sshtest.Agent(t)
	server := sshtest.NewServer(t, pub)
	other := sshtest.NewServer(t, pub) // in known_hosts; the one we dial is not

	d := Dialer{AgentSock: sock, KnownHosts: sshtest.KnownHosts(t, other)}
	_, err := d.Dial(context.Background(), "direct", []chorefile.Hop{sshtest.Hop(t, server)})
	if err == nil {
		t.Fatal("an unverified host key must refuse the connection")
	}
	if !strings.Contains(err.Error(), "known_hosts") {
		t.Errorf("error %q should say where the check came from", err)
	}
}

// Authentication is the agent and only the agent, so an environment without one
// says so rather than failing somewhere deeper with a confusing message.
func TestNoAgentIsAClearFailure(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	d := Dialer{KnownHosts: sshtest.KnownHosts(t)}
	_, err := d.Dial(context.Background(), "direct", []chorefile.Hop{{Host: "127.0.0.1", Port: 22, User: "x"}})
	if err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
		t.Fatalf("err = %v, want it to name SSH_AUTH_SOCK", err)
	}
}

// The far end's status is chore's status, so a caller can act on it.
func TestARemoteFailureCarriesItsExitStatus(t *testing.T) {
	sock, pub := sshtest.Agent(t)
	server := sshtest.NewServer(t, pub)
	d := Dialer{AgentSock: sock, KnownHosts: sshtest.KnownHosts(t, server)}

	client, err := d.Dial(context.Background(), "direct", []chorefile.Hop{sshtest.Hop(t, server)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var out, errOut bytes.Buffer
	err = Exec(context.Background(), client, QuoteArgv([]string{"please", "fail"}), nil, &out, &errOut)
	var exit *shell.ExitError
	if err == nil {
		t.Fatal("a failing remote command must be an error")
	}
	if !asExitError(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("err = %v, want exit status 7", err)
	}
}

// The SSH protocol carries a command as one string that the far end hands to a
// shell, so an argv only survives if chore quotes it. This is the assertion that
// an argument with a space, a quote or a `$` in it arrives whole.
func TestArgvSurvivesQuoting(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"a space", []string{"echo", "two words"}, `'echo' 'two words'`},
		{"a single quote", []string{"echo", "it's"}, `'echo' 'it'\''s'`},
		{"a dollar", []string{"echo", "$HOME"}, `'echo' '$HOME'`},
		{"a semicolon", []string{"echo", "a; rm -rf /"}, `'echo' 'a; rm -rf /'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := QuoteArgv(tc.argv); got != tc.want {
				t.Errorf("QuoteArgv = %s, want %s", got, tc.want)
			}
		})
	}
}

// The string form is passed through as written, so a pipe reaches the far end's
// shell intact — which the list form cannot express at all.
func TestAShellLineIsPassedThroughWhole(t *testing.T) {
	sock, pub := sshtest.Agent(t)
	server := sshtest.NewServer(t, pub)
	d := Dialer{AgentSock: sock, KnownHosts: sshtest.KnownHosts(t, server)}

	client, err := d.Dial(context.Background(), "direct", []chorefile.Hop{sshtest.Hop(t, server)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var out, errOut bytes.Buffer
	line := "kubectl get pods -A | wc -l"
	if err := Exec(context.Background(), client, line, nil, &out, &errOut); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != line {
		t.Errorf("the far end received %q, want it byte for byte", got)
	}
}

// A tunnel carries bytes from a local port to an address resolved at the FAR
// END, and stops when its context does — which is hand-written, because a
// tunnel is goroutines inside chore rather than a child in its own process
// group.
func TestForwardCarriesBytesAndStopsOnCancel(t *testing.T) {
	sock, pub := sshtest.Agent(t)
	server := sshtest.NewServer(t, pub)
	d := Dialer{AgentSock: sock, KnownHosts: sshtest.KnownHosts(t, server)}

	client, err := d.Dial(context.Background(), "direct", []chorefile.Hop{sshtest.Hop(t, server)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Something for the far end to reach: an echo service the test server can
	// dial on its own behalf.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); buf := make([]byte, 64); n, _ := c.Read(buf); _, _ = c.Write(buf[:n]) }()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	bound := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- RunForward(ctx, client, chorefile.Forward{Local: "127.0.0.1:0", Remote: echo.Addr().String()},
			func(addr string) { bound <- addr })
	}()

	var addr string
	select {
	case addr = <-bound:
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel never announced its address")
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dialling the tunnel: %v", err)
	}
	if _, err := conn.Write([]byte("through")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 7)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("reading back through the tunnel: %v", err)
	}
	if string(buf) != "through" {
		t.Errorf("read %q back, want %q", buf, "through")
	}
	_ = conn.Close()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel did not stop when its context was cancelled")
	}
}

// closedPort returns a port with nothing listening on it.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func asExitError(err error, target **shell.ExitError) bool {
	for err != nil {
		if e, ok := err.(*shell.ExitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Without `pty:` a remote command gets ordinary streams and no terminal, which
// is what keeps its output pipeable and its input a plain byte stream.
func TestExecAsksForNoTerminal(t *testing.T) {
	sock, pub := sshtest.Agent(t)
	server := sshtest.NewServer(t, pub)
	d := Dialer{AgentSock: sock, KnownHosts: sshtest.KnownHosts(t, server)}

	client, err := d.Dial(context.Background(), "direct", []chorefile.Hop{sshtest.Hop(t, server)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var out, errOut bytes.Buffer
	if err := Exec(context.Background(), client, QuoteArgv([]string{"hostname"}), nil, &out, &errOut); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := server.PTYRequests(); len(got) != 0 {
		t.Errorf("Exec asked for a terminal: %v", got)
	}
}

// With `pty: true` the far end is given a terminal before the command starts.
// With no local terminal to measure it is ssh's default 80x24, and a TERM the
// far end could do nothing useful with is replaced by one it can.
func TestExecPTYAsksForATerminal(t *testing.T) {
	for _, tc := range []struct{ name, term, want string }{
		{"TERM is passed through", "screen-256color", "screen-256color"},
		{"no TERM", "", "xterm-256color"},
		{"a dumb TERM", "dumb", "xterm-256color"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", tc.term)
			sock, pub := sshtest.Agent(t)
			server := sshtest.NewServer(t, pub)
			d := Dialer{AgentSock: sock, KnownHosts: sshtest.KnownHosts(t, server)}

			client, err := d.Dial(context.Background(), "direct", []chorefile.Hop{sshtest.Hop(t, server)})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()

			var out, errOut bytes.Buffer
			if err := ExecPTY(context.Background(), client, QuoteArgv([]string{"bash", "-l"}), nil, &out, &errOut); err != nil {
				t.Fatalf("ExecPTY: %v (stderr %q)", err, errOut.String())
			}
			want := sshtest.PTYReq{Term: tc.want, TermSize: sshtest.TermSize{Cols: 80, Rows: 24}}
			if got := server.PTYRequests(); len(got) != 1 || got[0] != want {
				t.Errorf("pty requests = %v, want [%v]", got, want)
			}
			if got := server.Commands(); len(got) != 1 || got[0] != `'bash' '-l'` {
				t.Errorf("the far end ran %q", got)
			}
		})
	}
}
