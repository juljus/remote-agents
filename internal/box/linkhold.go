package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/linkproto"
)

// LinkHold serves the box-wide laptop socket for as long as this link
// connection lives, relaying shim requests over stdin/stdout. A newer link
// takes the socket over; an older one never removes a newer one's socket.
func LinkHold(ctx context.Context, l boxlayout.Layout, stdin io.Reader, stdout io.Writer, heartbeatTimeout time.Duration, logf func(error)) (err error) {
	path := l.LaptopSocket()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating socket dir: %w", err)
	}
	ln, owned, err := takeOver(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release(path, owned)) }()
	return linkproto.Relay(ctx, ln, stdin, stdout, heartbeatTimeout, logf)
}

func lockPath(socket string) string { return socket + ".lock" }

func withLock(socket string, fn func() error) error {
	f, err := os.OpenFile(lockPath(socket), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening socket lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking socket: %w", err)
	}
	return fn()
}

func takeOver(path string) (net.Listener, os.FileInfo, error) {
	var ln *net.UnixListener
	var owned os.FileInfo
	err := withLock(path, func() error {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing previous socket: %w", err)
		}
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			return fmt.Errorf("listening on %s: %w", path, err)
		}
		l.SetUnlinkOnClose(false)
		info, err := os.Stat(path)
		if err != nil {
			l.Close()
			return fmt.Errorf("stat new socket: %w", err)
		}
		ln, owned = l, info
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return ln, owned, nil
}

func release(path string, owned os.FileInfo) error {
	return withLock(path, func() error {
		current, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stat socket: %w", err)
		}
		if !os.SameFile(current, owned) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing socket: %w", err)
		}
		return nil
	})
}
