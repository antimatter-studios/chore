package global

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Forward binds f.Local on this machine and carries every connection to
// f.Remote, resolved at the FAR END of the route. It blocks until ctx is done.
//
// Foreground and blocking on purpose. Daemonising would mean a stop verb, a
// registry of running tunnels, pid files, and an answer for a tunnel that died
// while nobody was looking — a lot of machinery to save one terminal tab, and
// every piece of it is state that can disagree with reality.
//
// Cancellation is written out by hand here, and that is worth knowing: every
// other long-running thing chore does is a child process in its own process
// group, so an interrupt kills it whether or not chore is paying attention. A
// tunnel is goroutines inside chore, so it gets none of that for free — the
// listener is closed on ctx.Done, which unblocks Accept, and the open
// connections are closed behind it.
func RunForward(ctx context.Context, client *ssh.Client, f Forward, announce func(string)) error {
	listener, err := net.Listen("tcp", f.Local)
	if err != nil {
		return fmt.Errorf("binding %s on this machine: %w", f.Local, err)
	}

	// The bound address rather than the requested one: `local: 127.0.0.1:0` is a
	// legitimate way to ask for any free port, and then the only way to learn
	// which one is to be told.
	if announce != nil {
		announce(listener.Addr().String())
	}

	var wg sync.WaitGroup
	var conns sync.Map // net.Conn -> struct{}, so cancellation can close what is open

	// Closing the listener is what unblocks Accept below; there is no
	// AcceptContext to select on.
	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		conns.Range(func(k, _ any) bool {
			_ = k.(net.Conn).Close()
			return true
		})
		close(stopped)
	}()

	for {
		local, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				// The listener was closed by the cancellation above, which is a
				// tidy end rather than a failure. Wait for the connections that
				// were in flight so nothing is left half-copied after chore says
				// it has stopped.
				<-stopped
				wg.Wait()
				return ctx.Err()
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				wg.Wait()
				return nil
			}
			return fmt.Errorf("accepting on %s: %w", f.Local, err)
		}
		conns.Store(local, struct{}{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conns.Delete(local)
			defer local.Close()
			// Dialled from the far end of the route, which is what makes
			// `remote: 127.0.0.1:6443` mean the cluster's loopback and not this
			// machine's.
			remote, err := client.Dial("tcp", f.Remote)
			if err != nil {
				return
			}
			defer remote.Close()
			pipe(local, remote)
		}()
	}
}

// pipe copies in both directions and returns when either side is done, so a
// connection closed at one end takes the other with it rather than leaking a
// goroutine per connection for the life of the tunnel.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
