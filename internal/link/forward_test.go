package link

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"remote-agents/internal/sshrun"
)

// echoSSH stands in for ssh: -W records its argv and echoes stdin back, and
// the box's listening check, logged to a probes file next to calls, fails
// once a file named down sits there too.
func echoSSH(t *testing.T) (ssh, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	ssh = filepath.Join(dir, "ssh")
	script := `#!/bin/sh
case "$*" in
  *" listening "*) echo probe >> ` + filepath.Join(dir, "probes") + `; [ -e ` + filepath.Join(dir, "down") + ` ] && exit 3; exit 0 ;;
esac
echo "$*" >> ` + calls + `
exec cat
`
	if err := os.WriteFile(ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return ssh, calls
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

func newForwarder(t *testing.T, lifetime time.Duration) (*SSHForwarder, string, *logSink) {
	t.Helper()
	ssh, calls := echoSSH(t)
	logs := &logSink{}
	f := &SSHForwarder{SSH: ssh, Box: sshrun.Box{Host: "box", Install: "ra-test"}, Lifetime: lifetime, ProbeEvery: time.Hour, Clock: SystemClock{}, Logf: logs.logf}
	t.Cleanup(f.Close)
	return f, calls, logs
}

// fakeClock fires timers only when Advance passes them.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clock  *fakeClock
	at     time.Time
	fn     func()
	active bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, fn func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{clock: c, at: c.now.Add(d), fn: fn, active: true}
	c.timers = append(c.timers, timer)
	return timer
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.at, t.active = t.clock.now.Add(d), true
	return was
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

// Advance moves time on and runs the timers that came due, before returning.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	for _, timer := range c.timers {
		if timer.active && !timer.at.After(c.now) {
			timer.active = false
			due = append(due, timer.fn)
		}
	}
	c.mu.Unlock()
	for _, fn := range due {
		fn()
	}
}

func lineCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// waitFor polls cond, which other goroutines make true, for up to within.
func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func echoes(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo through %s = %q, %v", addr, line, err)
	}
}

// current reads the forwarder's own state: once a forward closes, its port
// is free, and another test process may already listen on it.
func current(f *SSHForwarder, port uint16) *forward {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active[port]
}

// assertTornDown checks that a closed forward's own sockets are closed.
func assertTornDown(t *testing.T, fw *forward) {
	t.Helper()
	if fw == nil {
		t.Fatal("no forward to check")
	}
	for _, ln := range fw.listeners {
		if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("listener %s after close: Accept err = %v", ln.Addr(), err)
		}
	}
	for _, file := range fw.reserved {
		if err := file.Close(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("reservation %s after close: Close err = %v", file.Name(), err)
		}
	}
}

func TestForwardTunnelsBothLoopbackFamilies(t *testing.T) {
	f, calls, _ := newForwarder(t, time.Minute)
	port := freePort(t)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	echoes(t, fmt.Sprintf("127.0.0.1:%d", port))
	echoes(t, fmt.Sprintf("[::1]:%d", port))
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("-W localhost:%d box", port)
	if !strings.Contains(string(data), want) {
		t.Fatalf("want ssh %q, calls:\n%s", want, data)
	}
}

func TestForwardRefusesBusyPort(t *testing.T) {
	for _, tc := range []struct{ network, host string }{
		{"tcp4", "127.0.0.1"}, {"tcp6", "::1"}, {"tcp4", "0.0.0.0"}, {"tcp6", "::"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			f, _, _ := newForwarder(t, time.Minute)
			port := freePort(t)
			busy, err := net.Listen(tc.network, net.JoinHostPort(tc.host, fmt.Sprint(port)))
			if err != nil {
				t.Fatal(err)
			}
			defer busy.Close()
			err = f.Forward(context.Background(), port)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("laptop port %d", port)) {
				t.Fatalf("Forward on a port held on %s: %v", tc.host, err)
			}
			if f.active[port] != nil {
				t.Fatal("a refused forward must not stay registered")
			}
		})
	}
}

// A login retried at once gets the full lifetime again.
func TestReopenRestartsTheLifetime(t *testing.T) {
	f, _, logs := newForwarder(t, 2*time.Second)
	clock := &fakeClock{now: time.Unix(0, 0)}
	f.Clock = clock
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	fw := current(f, port)
	echoes(t, addr)
	clock.Advance(1500 * time.Millisecond)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	clock.Advance(time.Second)
	echoes(t, addr)
	clock.Advance(time.Second)
	if current(f, port) != nil {
		t.Fatal("forward still open after its restarted lifetime")
	}
	assertTornDown(t, fw)
	if !logs.contains(fmt.Sprintf("forward localhost:%d closed after 2s", port)) {
		t.Fatalf("expiry not logged: %v", logs.snapshot())
	}
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatalf("forwarding again after expiry: %v", err)
	}
	echoes(t, addr)
}

func TestForwardEndsWhenTheBoxStopsListening(t *testing.T) {
	f, calls, logs := newForwarder(t, time.Minute)
	f.ProbeEvery = 50 * time.Millisecond
	probes := filepath.Join(filepath.Dir(calls), "probes")
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	fw := current(f, port)
	waitFor(t, "two probes while the box listens", 5*time.Second, func() bool { return lineCount(t, probes) >= 2 })
	echoes(t, addr)
	if err := os.WriteFile(filepath.Join(filepath.Dir(calls), "down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the forward to close", 5*time.Second, func() bool { return current(f, port) == nil })
	assertTornDown(t, fw)
	if !logs.contains(fmt.Sprintf("forward localhost:%d closed: the box stopped listening", port)) {
		t.Fatalf("close reason not logged: %v", logs.snapshot())
	}
}

// The tunnel that carried the callback ending is the moment a login server stops.
func TestForwardChecksTheBoxWhenATunnelEnds(t *testing.T) {
	f, calls, _ := newForwarder(t, time.Minute)
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	fw := current(f, port)
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("echo = %q, %v", line, err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(calls), "down"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	waitFor(t, "the forward to close", 5*time.Second, func() bool { return current(f, port) == nil })
	assertTornDown(t, fw)
}

// A dead link says nothing about the box's listener.
func TestForwardOutlivesAnUnreachableBox(t *testing.T) {
	dir := t.TempDir()
	ssh := filepath.Join(dir, "ssh")
	probes := filepath.Join(dir, "probes")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\necho probe >> "+probes+"\necho 'ssh: connect to host box port 22: Network is unreachable' >&2\nexit 255\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &SSHForwarder{SSH: ssh, Box: sshrun.Box{Host: "box", Install: "ra-test"}, Lifetime: time.Minute, ProbeEvery: 20 * time.Millisecond, Clock: SystemClock{}, Logf: (&logSink{}).logf}
	defer f.Close()
	port := freePort(t)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	// Each check after the first starts only once the one before has ended.
	waitFor(t, "three failed checks", 5*time.Second, func() bool { return lineCount(t, probes) >= 3 })
	if current(f, port) == nil {
		t.Fatal("an unreachable box ended the forward")
	}
}

func TestCloseEndsForwardsAndTheirTunnels(t *testing.T) {
	f, _, _ := newForwarder(t, time.Minute)
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	fw := current(f, port)
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if line, err := reader.ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("echo = %q, %v", line, err)
	}
	f.Close()
	if err := conn.SetReadDeadline(time.Now().Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("open tunnel after Close: read err = %v, want EOF", err)
	}
	if current(f, port) != nil {
		t.Fatal("still forwarding after Close")
	}
	assertTornDown(t, fw)
	if err := f.Forward(context.Background(), port); err == nil {
		t.Fatal("Forward after Close should fail")
	}
}

func TestForwardBoundsOpenTunnels(t *testing.T) {
	dir := t.TempDir()
	starts := filepath.Join(dir, "starts")
	ssh := filepath.Join(dir, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\necho start >> "+starts+"\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	logs := &logSink{}
	f := &SSHForwarder{SSH: ssh, Box: sshrun.Box{Host: "box", Install: "ra-test"}, Lifetime: time.Minute, ProbeEvery: time.Hour, Clock: SystemClock{}, Logf: logs.logf}
	defer f.Close()
	port := freePort(t)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for range maxTunnels {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
	}
	// The kernel may hand connections to accept out of dial order, so the extra one waits for all tunnels.
	waitFor(t, "every tunnel to start", 10*time.Second, func() bool { return lineCount(t, starts) == maxTunnels })
	extra, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	if err := extra.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := extra.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("connection beyond the bound: read err = %v, want EOF; logs: %v", err, logs.snapshot())
	}
	if !logs.contains(fmt.Sprintf("%d already open", maxTunnels)) {
		t.Fatalf("refusal not logged: %v", logs.snapshot())
	}
	if n := lineCount(t, starts); n != maxTunnels {
		t.Fatalf("%d tunnels started, want %d", n, maxTunnels)
	}
}

// A program binding the wildcard later would share localhost connections with
// the forward, so its bind has to fail instead.
func TestForwardKeepsLaterWildcardBindsOut(t *testing.T) {
	f, _, _ := newForwarder(t, time.Minute)
	port := freePort(t)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []struct{ network, host string }{{"tcp", ""}, {"tcp4", "0.0.0.0"}, {"tcp6", "::"}, {"tcp4", "127.0.0.1"}, {"tcp6", "::1"}} {
		ln, err := net.Listen(addr.network, net.JoinHostPort(addr.host, fmt.Sprint(port)))
		if err == nil {
			ln.Close()
			t.Fatalf("%s %q:%d bound while forwarded", addr.network, addr.host, port)
		}
	}
	// The reservation must not leak into children, or one could hold the port forever.
	child := exec.Command("sleep", "5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		child.Process.Kill()
		child.Wait()
	}()
	f.Close()
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("port still held after Close: %v", err)
	}
	ln.Close()
}

func TestCloseWaitsForTunnelProcesses(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	ssh := filepath.Join(dir, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\necho $$ > "+pidFile+"\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &SSHForwarder{SSH: ssh, Box: sshrun.Box{Host: "box", Install: "ra-test"}, Lifetime: time.Minute, ProbeEvery: time.Hour, Clock: SystemClock{}, Logf: (&logSink{}).logf}
	port := freePort(t)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(pidFile)
		if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			pid = n
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tunnel ssh never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.Close()
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("tunnel ssh %d still running after Close", pid)
	}
}

type flakyListener struct {
	net.Listener
	failures int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures > 0 {
		l.failures--
		return nil, syscall.EMFILE
	}
	return l.Listener.Accept()
}

func TestForwardSurvivesTransientAcceptErrors(t *testing.T) {
	f, _, logs := newForwarder(t, time.Minute)
	port := freePort(t)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	fw := f.active[port]
	f.mu.Unlock()
	other, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	f.wg.Add(1)
	go f.accept(context.Background(), &flakyListener{Listener: other, failures: 3}, port, fw, make(chan struct{}, 1))
	echoes(t, other.Addr().String())
	if logged := strings.Count(strings.Join(logs.snapshot(), "\n"), "too many open files"); logged != 3 {
		t.Fatalf("want each of 3 accept errors logged: %v", logs.snapshot())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active[port] != fw {
		t.Fatal("a transient accept error dropped the forward")
	}
}

// A client that stops reading must not keep Close waiting on its tunnel.
func TestCloseDoesNotWaitOnAStalledClient(t *testing.T) {
	dir := t.TempDir()
	ssh := filepath.Join(dir, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nexec yes\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &SSHForwarder{SSH: ssh, Box: sshrun.Box{Host: "box", Install: "ra-test"}, Lifetime: time.Minute, ProbeEvery: time.Hour, Clock: SystemClock{}, Logf: (&logSink{}).logf}
	port := freePort(t)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Taking one byte proves the tunnel runs; yes then fills the socket buffers at once.
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		f.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a client that stopped reading")
	}
}

// Each tunnel starts an ssh; a connection accepted meanwhile must not leak
// into it, or closing that connection would leave its client waiting.
func TestEndedTunnelsCloseTheirClients(t *testing.T) {
	dir := t.TempDir()
	ssh := filepath.Join(dir, "ssh")
	// Lingering children keep any leaked socket open past the client's EOF wait.
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nsleep 2 >/dev/null 2>&1 </dev/null &\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &SSHForwarder{SSH: ssh, Box: sshrun.Box{Host: "box", Install: "ra-test"}, Lifetime: time.Minute, ProbeEvery: time.Hour, Clock: SystemClock{}, Logf: (&logSink{}).logf}
	defer f.Close()
	port := freePort(t)
	if err := f.Forward(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	const clients, each = 8, 100
	hung := make(chan int, clients)
	for range clients {
		go func() {
			n := 0
			for range each {
				conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Error(err)
					break
				}
				if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
					t.Error(err)
				}
				if _, err := conn.Read(make([]byte, 1)); errors.Is(err, os.ErrDeadlineExceeded) {
					n++
				}
				conn.Close()
			}
			hung <- n
		}()
	}
	total := 0
	for range clients {
		total += <-hung
	}
	if total != 0 {
		t.Fatalf("%d of %d connections stayed open after their tunnel ended", total, clients*each)
	}
}
