package cloexec

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// closesEveryConnection reports how many of n clients failed to see EOF
// promptly after the server closed their connection, while other goroutines
// keep starting child processes.
func closesEveryConnection(t *testing.T, ln net.Listener, n int) int {
	t.Helper()
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	var spawners sync.WaitGroup
	for range 4 {
		spawners.Add(1)
		go func() {
			defer spawners.Done()
			for ctx.Err() == nil {
				// The child outlives the test's EOF wait, so an inherited socket shows.
				cmd := exec.Command("sleep", "2")
				if err := cmd.Start(); err != nil {
					t.Error(err)
					return
				}
				go cmd.Wait()
			}
		}()
	}
	defer func() {
		cancel()
		spawners.Wait()
	}()
	hung := 0
	for range n {
		conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		_, err = conn.Read(make([]byte, 1))
		conn.Close()
		switch {
		case err == io.EOF:
		case errors.Is(err, os.ErrDeadlineExceeded):
			hung++
		default:
			t.Fatalf("read: %v", err)
		}
	}
	return hung
}

func TestAcceptedSocketsNeverLeakIntoChildren(t *testing.T) {
	ln, err := ListenTCP(netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	if hung := closesEveryConnection(t, ln, leakProbes); hung != 0 {
		t.Fatalf("%d of %d closed connections stayed open in a child", hung, leakProbes)
	}
}

func TestPlainAcceptLeaksIntoChildren(t *testing.T) {
	if os.Getenv("RA_CLOEXEC_PLAIN") != "1" {
		t.Skip("demonstrates the darwin race; set RA_CLOEXEC_PLAIN=1")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("net.Listen: %d of %d closed connections stayed open in a child", closesEveryConnection(t, ln, leakProbes), leakProbes)
}

func TestListenerBasics(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		ln, err := ListenTCP(netip.MustParseAddrPort(addr))
		if err != nil {
			t.Fatal(err)
		}
		accepted := make(chan error, 1)
		go func() {
			conn, err := ln.Accept()
			if err == nil {
				_, err = conn.Write([]byte("hi"))
				conn.Close()
			}
			accepted <- err
		}()
		conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
		if err != nil {
			t.Fatalf("dial %s: %v", ln.Addr(), err)
		}
		data, err := io.ReadAll(conn)
		conn.Close()
		if err != nil || string(data) != "hi" {
			t.Fatalf("read %q, %v", data, err)
		}
		if err := <-accepted; err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := ln.Accept()
			done <- err
		}()
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close: %v, want net.ErrClosed", err)
		}
	}
	if _, err := ListenTCP(netip.MustParseAddrPort("127.0.0.1:0")); err != nil {
		t.Fatal(err)
	}
	busy, err := ListenTCP(netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if _, err := ListenTCP(netip.MustParseAddrPort(busy.Addr().String())); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("second listen on %s: %v, want EADDRINUSE", busy.Addr(), err)
	}
}

func TestUnixListenerRemovesItsSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ra-cx-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")
	ln, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
	}()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read: %v, want EOF", err)
	}
	conn.Close()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket file after Close: %v", err)
	}
}
