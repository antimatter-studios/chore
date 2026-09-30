// Package sshtest is a real ssh server, a real ssh-agent, and real host keys,
// for tests anywhere in chore that need a route to travel: the transport's own,
// the runner's, and the command line's.
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// A real ssh server, a real ssh-agent, and real host keys.
//
// Nothing here is mocked, and that is the point: the fold in Dial is the whole
// feature, and the things most likely to be wrong about it — that a later hop is
// dialled FROM the hop before it, that agent auth reaches every hop, that a host
// key is actually checked — are exactly the things a fake transport would assert
// nothing about. Two of these servers chained together is the shape of the
// user's own route.

// Server is an ssh server that can run "commands" and open direct-tcpip
// channels, which is all a route needs of an intermediate hop.
type Server struct {
	t        testing.TB
	Addr     string
	HostKey  ssh.PublicKey
	listener net.Listener

	mu       sync.Mutex
	commands []string   // every command string the server was asked to exec
	ptys     []PTYReq   // every terminal a session asked for
	resizes  []TermSize // every window-change a session sent

	// Shell runs every command with `sh -c` on this machine and returns its real
	// output and status, instead of echoing the command string back. For a test
	// about what a routed step DOES rather than what crossed the wire.
	Shell bool

	// held is signalled when a "hold" command starts. The session then stays
	// open until the client resizes its terminal, so a test can look at the
	// local side while a remote command is running.
	held chan struct{}
}

// TermSize is a terminal's size in characters, as the SSH protocol carries it.
type TermSize struct{ Cols, Rows uint32 }

// PTYReq is what a "pty-req" asked for: a terminal type and its size.
type PTYReq struct {
	Term string
	TermSize
}

// NewServer starts one on a loopback port, accepting the given public key.
func NewServer(t testing.TB, authorized ssh.PublicKey) *Server {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("unknown key")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{t: t, Addr: ln.Addr().String(), HostKey: signer.PublicKey(), listener: ln, held: make(chan struct{}, 1)}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, cfg)
		}
	}()
	return s
}

func (s *Server) serve(nConn net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session":
			go s.session(newChan)
		case "direct-tcpip":
			// This is what makes a hop a hop: the client asks THIS server to open
			// a TCP connection to somewhere it can reach, and the next SSH
			// handshake runs over it.
			go s.directTCPIP(newChan)
		default:
			_ = newChan.Reject(ssh.UnknownChannelType, newChan.ChannelType())
		}
	}
}

func (s *Server) session(newChan ssh.NewChannel) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	holding := false
	for req := range reqs {
		switch req.Type {
		case "exec":
		case "pty-req":
			var p struct {
				Term                      string
				Cols, Rows, Width, Height uint32
				Modes                     string
			}
			if err := ssh.Unmarshal(req.Payload, &p); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			s.mu.Lock()
			s.ptys = append(s.ptys, PTYReq{Term: p.Term, TermSize: TermSize{Cols: p.Cols, Rows: p.Rows}})
			s.mu.Unlock()
			_ = req.Reply(true, nil)
			continue
		case "window-change":
			var w struct{ Cols, Rows, Width, Height uint32 }
			if err := ssh.Unmarshal(req.Payload, &w); err != nil {
				continue
			}
			s.mu.Lock()
			s.resizes = append(s.resizes, TermSize{Cols: w.Cols, Rows: w.Rows})
			s.mu.Unlock()
			if holding {
				fmt.Fprintf(ch, "resized to %dx%d\n", w.Cols, w.Rows)
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				return
			}
			continue
		default:
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			continue
		}
		_ = req.Reply(true, nil)

		s.mu.Lock()
		s.commands = append(s.commands, payload.Command)
		s.mu.Unlock()

		if payload.Command == "'hold'" {
			holding = true
			select {
			case s.held <- struct{}{}:
			default:
			}
			continue
		}

		if s.Shell {
			s.runShell(ch, payload.Command)
			return
		}

		// Not a shell: enough of one to prove the command arrived intact. The
		// command is echoed back so a test can assert on the exact string that
		// crossed the wire, which is where quoting either survives or does not.
		status := 0
		switch {
		case strings.Contains(payload.Command, "fail"):
			status = 7
			fmt.Fprintln(ch.Stderr(), "it failed")
		default:
			fmt.Fprintln(ch, payload.Command)
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
		return
	}
}

func (s *Server) directTCPIP(newChan ssh.NewChannel) {
	var payload struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if err := ssh.Unmarshal(newChan.ExtraData(), &payload); err != nil {
		_ = newChan.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	target := net.JoinHostPort(payload.Host, fmt.Sprint(payload.Port))
	remote, err := net.Dial("tcp", target)
	if err != nil {
		_ = newChan.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := newChan.Accept()
	if err != nil {
		_ = remote.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { _, _ = io.Copy(ch, remote); _ = ch.Close() }()
	go func() { _, _ = io.Copy(remote, ch); _ = remote.Close() }()
}

// runShell runs a command for real, the way an sshd hands it to a login shell.
func (s *Server) runShell(ch ssh.Channel, command string) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdout, cmd.Stderr = ch, ch.Stderr()
	status := 0
	if err := cmd.Run(); err != nil {
		status = 255
		if exit, ok := err.(*exec.ExitError); ok {
			status = exit.ExitCode()
		}
	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
}

// Held is signalled when a "hold" command starts.
func (s *Server) Held() <-chan struct{} { return s.held }

// Commands returns every command string this server was asked to exec.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// PTYRequests returns every terminal a session on this server asked for.
func (s *Server) PTYRequests() []PTYReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PTYReq(nil), s.ptys...)
}

// WindowChanges returns every terminal resize a session on this server was sent.
func (s *Server) WindowChanges() []TermSize {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TermSize(nil), s.resizes...)
}

// Agent runs an in-process ssh-agent on a unix socket, holding one key.
//
// Real, rather than an ssh.AuthMethod handed straight to the dialler, because
// "chore never handles a key itself, it asks your agent" is a promise about the
// code path — and a test that skipped the socket would not be testing it.
func Agent(t testing.TB) (sock string, pub ssh.PublicKey) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}

	// macOS caps a unix socket path at 104 bytes and t.TempDir() under
	// /var/folders is long enough to matter, so this lives somewhere short.
	dir, err := os.MkdirTemp("", "ca")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock = filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = agent.ServeAgent(keyring, conn) }()
		}
	}()
	return sock, signer.PublicKey()
}

// KnownHosts writes a known_hosts naming each server by its address, the way
// ssh would have written it after a first connection.
func KnownHosts(t testing.TB, servers ...*Server) string {
	t.Helper()
	var b strings.Builder
	for _, s := range servers {
		host, port, err := net.SplitHostPort(s.Addr)
		if err != nil {
			t.Fatal(err)
		}
		// The bracketed form is what ssh uses for a non-22 port, and every port
		// here is non-22.
		b.WriteString(fmt.Sprintf("[%s]:%s %s\n", host, port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.HostKey)))))
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Hop is a route hop addressing a test server.
func Hop(t testing.TB, s *Server) chorefile.Hop {
	t.Helper()
	host, port, err := net.SplitHostPort(s.Addr)
	if err != nil {
		t.Fatal(err)
	}
	p := 0
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
		t.Fatal(err)
	}
	return chorefile.Hop{Host: host, Port: p, User: "tester"}
}
