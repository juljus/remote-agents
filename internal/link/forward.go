package link

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"remote-agents/internal/cloexec"
	"remote-agents/internal/spec"
	"remote-agents/internal/sshrun"
)

// ForwardLifetime bounds how long a login callback port stays forwarded.
const ForwardLifetime = 10 * time.Minute

// ForwardProbeEvery is how often a forward checks the box still listens.
const ForwardProbeEvery = 15 * time.Second

// maxTunnels bounds the ssh processes one forward runs at once.
const maxTunnels = 16

// SSHForwarder listens on the laptop's localhost:port and carries each
// connection to the box's localhost:port over its own ssh -W, so a forward
// needs no long-lived connection that a network change could break. A
// forward ends after Lifetime, or once the box refuses connections to the
// port: a login server stops listening when its callback has arrived.
type SSHForwarder struct {
	SSH        string
	Box        sshrun.Box
	Lifetime   time.Duration
	ProbeEvery time.Duration
	Clock      Clock
	Logf       func(format string, args ...any)

	mu     sync.Mutex
	active map[uint16]*forward
	closed bool
	wg     sync.WaitGroup
}

var _ PortForwarder = (*SSHForwarder)(nil)

// Clock schedules forward lifetimes.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

type Timer interface {
	Reset(d time.Duration) bool
	Stop() bool
}

type SystemClock struct{}

func (SystemClock) Now() time.Time                            { return time.Now() }
func (SystemClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

type forward struct {
	reserved  []*os.File
	listeners []net.Listener
	timer     Timer
	expires   time.Time
	cancel    context.CancelFunc
	// tunnelEnded asks for a probe of the box listener.
	tunnelEnded chan struct{}
}

// Forward starts forwarding port. A running forward of it restarts its
// lifetime, since a login retried at once needs the full time again.
func (f *SSHForwarder) Forward(_ context.Context, port uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("link daemon is shutting down")
	}
	if fw, ok := f.active[port]; ok {
		fw.expires = f.Clock.Now().Add(f.Lifetime)
		fw.timer.Reset(f.Lifetime)
		f.Logf("forward localhost:%d restarted for %s", port, f.Lifetime)
		return nil
	}
	reserved, err := reserveWildcards(port)
	if err != nil {
		return err
	}
	listeners, err := listenLoopback(port)
	if err != nil {
		closeFiles(reserved)
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	fw := &forward{reserved: reserved, listeners: listeners, expires: f.Clock.Now().Add(f.Lifetime), cancel: cancel, tunnelEnded: make(chan struct{}, 1)}
	fw.timer = f.Clock.AfterFunc(f.Lifetime, func() { f.expire(port, fw) })
	if f.active == nil {
		f.active = make(map[uint16]*forward)
	}
	f.active[port] = fw
	slots := make(chan struct{}, maxTunnels)
	for _, ln := range listeners {
		f.wg.Add(1)
		go f.accept(ctx, ln, port, fw, slots)
	}
	f.wg.Add(1)
	go f.watch(ctx, port, fw)
	f.Logf("forwarding localhost:%d to the box for %s", port, f.Lifetime)
	return nil
}

// reserveWildcards binds 0.0.0.0:port and [::]:port without listening. A
// laptop program that binds the wildcard while the forward holds loopback
// would otherwise share its localhost connections with the box, so its bind
// has to fail instead. A wildcard someone already holds counts as the port
// being taken, for the same reason.
func reserveWildcards(port uint16) ([]*os.File, error) {
	var files []*os.File
	for _, addr := range []netip.Addr{netip.IPv4Unspecified(), netip.IPv6Unspecified()} {
		file, err := cloexec.BindTCP(netip.AddrPortFrom(addr, port))
		if err != nil {
			closeFiles(files)
			return nil, fmt.Errorf("laptop port %d is in use: %w", port, err)
		}
		files = append(files, file)
	}
	return files, nil
}

func closeFiles(files []*os.File) {
	for _, file := range files {
		// Closing a socket nobody else holds only fails if it is already closed.
		_ = file.Close()
	}
}

// listenLoopback binds both loopback families, since a browser may try
// either for localhost. Its connections must not leak into the ssh processes
// the forward starts, or closing one would leave the browser waiting.
func listenLoopback(port uint16) ([]net.Listener, error) {
	var listeners []net.Listener
	for _, addr := range []netip.Addr{loopback4, netip.IPv6Loopback()} {
		ln, err := cloexec.ListenTCP(netip.AddrPortFrom(addr, port))
		if err != nil {
			closeAll(listeners)
			return nil, fmt.Errorf("laptop port %d is in use: %w", port, err)
		}
		listeners = append(listeners, ln)
	}
	return listeners, nil
}

func closeAll(listeners []net.Listener) {
	for _, ln := range listeners {
		// Closing a listener nobody else holds only fails if it is already closed.
		_ = ln.Close()
	}
}

func (f *SSHForwarder) expire(port uint16, fw *forward) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A restart that raced the timer firing has re-armed it.
	if f.active[port] != fw || f.Clock.Now().Before(fw.expires) {
		return
	}
	f.drop(port, fw, "closed after "+f.Lifetime.String())
}

// watch ends the forward once nothing listens on the box's port, checking
// periodically and whenever a tunnel ends.
func (f *SSHForwarder) watch(ctx context.Context, port uint16, fw *forward) {
	defer f.wg.Done()
	ticker := time.NewTicker(f.ProbeEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-fw.tunnelEnded:
		}
		if f.boxListening(ctx, port) {
			continue
		}
		f.mu.Lock()
		if f.active[port] == fw {
			f.drop(port, fw, "closed: the box stopped listening")
		}
		f.mu.Unlock()
		return
	}
}

// boxListening is false only when the box reports nothing listening on the
// port; a link that is down says nothing about the listener.
func (f *SSHForwarder) boxListening(ctx context.Context, port uint16) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res := sshrun.Run(ctx, sshrun.ListeningArgv(f.SSH, f.Box, port),
		sshrun.Stdio{Stderr: lineLogger{prefix: fmt.Sprintf("forward localhost:%d: checking the box: ", port), logf: f.Logf}}, 2*time.Second)
	switch {
	case res.Err != nil:
		f.Logf("forward localhost:%d: checking the box: %v", port, res.Err)
	case res.Transport || res.ExitCode == 0:
	case res.ExitCode == spec.ExitNotListening:
		return false
	default:
		f.Logf("forward localhost:%d: checking the box: remote-agents-box listening exited with status %d", port, res.ExitCode)
	}
	return true
}

// drop must be called with f.mu held.
func (f *SSHForwarder) drop(port uint16, fw *forward, why string) {
	delete(f.active, port)
	fw.timer.Stop()
	fw.cancel()
	closeAll(fw.listeners)
	closeFiles(fw.reserved)
	f.Logf("forward localhost:%d %s", port, why)
}

func (f *SSHForwarder) accept(ctx context.Context, ln net.Listener, port uint16, fw *forward, slots chan struct{}) {
	defer f.wg.Done()
	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			// Like net/http: errors such as EMFILE pass, and the forward lives out its
			// lifetime. The backoff also bounds this log to a line a second.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			f.Logf("forward localhost:%d: accept: %v; retrying in %s", port, err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		select {
		case slots <- struct{}{}:
		default:
			f.Logf("forward localhost:%d: refusing a connection, %d already open", port, maxTunnels)
			conn.Close()
			continue
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer func() { <-slots }()
			f.tunnel(ctx, conn, port)
			select {
			case fw.tunnelEnded <- struct{}{}:
			default:
			}
		}()
	}
}

func (f *SSHForwarder) tunnel(ctx context.Context, conn net.Conn, port uint16) {
	defer conn.Close()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		f.Logf("forward localhost:%d: creating pipe: %v", port, err)
		return
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		f.Logf("forward localhost:%d: creating pipe: %v", port, err)
		return
	}
	// The copies end when either side of the tunnel does; Run reports how.
	go func() {
		_, _ = io.Copy(stdinW, conn)
		stdinW.Close()
	}()
	// Pipes rather than conn itself keep Run from waiting on a client that stopped reading.
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(conn, stdoutR)
		stdoutR.Close()
		close(copied)
	}()
	res := sshrun.Run(ctx, sshrun.TunnelArgv(f.SSH, f.Box.Host, port),
		sshrun.Stdio{Stdin: stdinR, Stdout: stdoutW, Stderr: lineLogger{prefix: fmt.Sprintf("forward localhost:%d: box: ", port), logf: f.Logf}}, 2*time.Second)
	stdinR.Close()
	stdoutW.Close()
	if res.Err != nil {
		f.Logf("forward localhost:%d: %v", port, res.Err)
	}
	// Deliver what ssh wrote last, unless the forward is ending; closing conn then unblocks the copy.
	select {
	case <-copied:
	case <-ctx.Done():
	}
}

// Close ends every forward and waits for its connections and their ssh processes.
func (f *SSHForwarder) Close() {
	f.mu.Lock()
	f.closed = true
	for port, fw := range f.active {
		f.drop(port, fw, "closed: link daemon stopping")
	}
	f.mu.Unlock()
	f.wg.Wait()
}
