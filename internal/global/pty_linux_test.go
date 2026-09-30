//go:build linux

package global

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The local half of `pty: true`, against a real terminal rather than a pipe:
// the far end is told the terminal's actual size, the terminal is raw while the
// session is open — so a keypress reaches the remote shell as it is typed, not a
// line at a time — a resize reaches the far end, and the terminal is put back
// exactly as it was found. Linux only because opening a pty pair by hand is
// platform-specific; the protocol side is covered everywhere by
// TestExecPTYAsksForATerminal.
func TestExecPTYDrivesALocalTerminal(t *testing.T) {
	tty := openPTY(t)
	setSize(t, tty, 50, 150)
	before := termios(t, tty)

	sock, pub := testAgent(t)
	server := newTestServer(t, pub)
	d := Dialer{AgentSock: sock, KnownHosts: knownHostsFor(t, server)}
	client, err := d.Dial(context.Background(), "direct", []Hop{hopTo(t, server)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- ExecPTY(ctx, client, Cmd{Argv: []string{"hold"}}, tty, &out, &errOut) }()

	select {
	case <-server.held:
	case err := <-done:
		t.Fatalf("ExecPTY returned before the command started: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the remote command never started")
	}

	want := ptyReq{Term: "screen", termSize: termSize{Cols: 150, Rows: 50}}
	if got := server.ptyRequests(); len(got) != 1 || got[0] != want {
		t.Errorf("pty requests = %v, want [%v]", got, want)
	}
	if during := termios(t, tty); during.Lflag&(unix.ECHO|unix.ICANON) != 0 {
		t.Errorf("the local terminal is not raw during the session (lflag %#x)", during.Lflag)
	}

	// What a terminal emulator does when its window is dragged: change the size
	// and signal. The signal is sent by hand because this process is not the
	// terminal's foreground process group, so the kernel would not send it.
	setSize(t, tty, 40, 120)
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ExecPTY: %v (stderr %q)", err, errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the resize never reached the far end")
	}
	if got, want := server.windowChanges(), (termSize{Cols: 120, Rows: 40}); len(got) != 1 || got[0] != want {
		t.Errorf("window changes = %v, want [%v]", got, want)
	}
	if !strings.Contains(out.String(), "resized to 120x40") {
		t.Errorf("stdout = %q", out.String())
	}
	if after := termios(t, tty); after != before {
		t.Errorf("the local terminal was not restored:\nbefore %+v\nafter  %+v", before, after)
	}
}

// openPTY opens a pseudo-terminal pair and returns its terminal end, the file a
// program sees as stdin when it runs in a terminal emulator. TERM is set to
// something other than the fallback so the test can tell it was passed through.
func openPTY(t *testing.T) *os.File {
	t.Helper()
	t.Setenv("TERM", "screen")

	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals here: %v", err)
	}
	// Closing the controller end is what unblocks the session's stdin copy, which
	// is still reading the terminal when ExecPTY returns.
	t.Cleanup(func() { _ = ptmx.Close() })
	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlocking the pty: %v", err)
	}
	n, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("naming the pty: %v", err)
	}
	tty, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("opening the terminal end: %v", err)
	}
	t.Cleanup(func() { _ = tty.Close() })
	return tty
}

func setSize(t *testing.T, tty *os.File, rows, cols uint16) {
	t.Helper()
	if err := unix.IoctlSetWinsize(int(tty.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		t.Fatalf("setting the terminal size: %v", err)
	}
}

func termios(t *testing.T, tty *os.File) unix.Termios {
	t.Helper()
	state, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("reading terminal settings: %v", err)
	}
	return *state
}
