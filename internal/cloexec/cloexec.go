// Package cloexec makes sockets that no child process can inherit.
// On darwin, net.Listener.Accept marks a new socket close-on-exec only after
// accept returns, without holding syscall.ForkLock, so a process started in
// that window keeps the socket open and closing the connection sends no FIN.
package cloexec

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"syscall"
)

// Listener accepts connections close-on-exec before any process can start.
type Listener struct {
	file   *os.File
	addr   net.Addr
	closed atomic.Bool
	// unlink is the unix socket path to remove on Close, as net does.
	unlink string
}

var _ net.Listener = (*Listener)(nil)

// BindTCP binds addr without listening, which keeps others from binding it
// while accepting nothing. A v6 address binds IPv6 only.
func BindTCP(addr netip.AddrPort) (*os.File, error) {
	fd, err := bindTCP(addr)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), addr.String()), nil
}

// ListenTCP listens on addr. A v6 address listens on IPv6 only.
func ListenTCP(addr netip.AddrPort) (*Listener, error) {
	fd, err := bindTCP(addr)
	if err != nil {
		return nil, err
	}
	bound, err := boundTCPAddr(fd)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return listen(fd, bound, "")
}

// ListenUnix listens on the unix socket path, which must not exist.
func ListenUnix(path string) (*Listener, error) {
	fd, err := socket(syscall.AF_UNIX)
	if err != nil {
		return nil, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind %s: %w", path, os.NewSyscallError("bind", err))
	}
	return listen(fd, &net.UnixAddr{Name: path, Net: "unix"}, path)
}

func socket(family int) (int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fd, err := syscall.Socket(family, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, os.NewSyscallError("socket", err)
	}
	syscall.CloseOnExec(fd)
	return fd, nil
}

func bindTCP(addr netip.AddrPort) (int, error) {
	family := syscall.AF_INET6
	var sa syscall.Sockaddr = &syscall.SockaddrInet6{Port: int(addr.Port()), Addr: addr.Addr().As16()}
	if addr.Addr().Is4() {
		family, sa = syscall.AF_INET, &syscall.SockaddrInet4{Port: int(addr.Port()), Addr: addr.Addr().As4()}
	}
	fd, err := socket(family)
	if err != nil {
		return -1, err
	}
	// SO_REUSEADDR, as net.Listen sets, so TIME_WAIT leftovers do not block the port.
	err = os.NewSyscallError("setsockopt", syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1))
	if err == nil && family == syscall.AF_INET6 {
		err = os.NewSyscallError("setsockopt", syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1))
	}
	if err == nil {
		err = os.NewSyscallError("bind", syscall.Bind(fd, sa))
	}
	if err != nil {
		syscall.Close(fd)
		return -1, fmt.Errorf("bind %s: %w", addr, err)
	}
	return fd, nil
}

func boundTCPAddr(fd int) (net.Addr, error) {
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		return nil, os.NewSyscallError("getsockname", err)
	}
	switch sa := sa.(type) {
	case *syscall.SockaddrInet4:
		return &net.TCPAddr{IP: net.IP(sa.Addr[:]), Port: sa.Port}, nil
	case *syscall.SockaddrInet6:
		return &net.TCPAddr{IP: net.IP(sa.Addr[:]), Port: sa.Port}, nil
	}
	return nil, fmt.Errorf("getsockname: unexpected %T", sa)
}

func listen(fd int, addr net.Addr, unlink string) (*Listener, error) {
	err := os.NewSyscallError("listen", syscall.Listen(fd, syscall.SOMAXCONN))
	// Non-blocking, so os.NewFile hands the socket to the runtime poller.
	if err == nil {
		err = os.NewSyscallError("setnonblock", syscall.SetNonblock(fd, true))
	}
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return &Listener{file: os.NewFile(uintptr(fd), addr.String()), addr: addr, unlink: unlink}, nil
}

func (l *Listener) Accept() (net.Conn, error) {
	conn, err := l.accept()
	// os reports a closed file in several forms; net's callers expect net.ErrClosed.
	if err != nil && l.closed.Load() {
		return nil, net.ErrClosed
	}
	return conn, err
}

func (l *Listener) accept() (net.Conn, error) {
	raw, err := l.file.SyscallConn()
	if err != nil {
		return nil, err
	}
	nfd := -1
	var acceptErr error
	err = raw.Read(func(fd uintptr) bool {
		for {
			syscall.ForkLock.RLock()
			nfd, _, acceptErr = syscall.Accept(int(fd))
			if acceptErr == nil {
				syscall.CloseOnExec(nfd)
			}
			syscall.ForkLock.RUnlock()
			if !errors.Is(acceptErr, syscall.EINTR) && !errors.Is(acceptErr, syscall.ECONNABORTED) {
				break
			}
		}
		// EAGAIN: nothing pending, wait until the socket is readable.
		return !errors.Is(acceptErr, syscall.EAGAIN)
	})
	if err != nil {
		return nil, err
	}
	if acceptErr != nil {
		return nil, os.NewSyscallError("accept", acceptErr)
	}
	file := os.NewFile(uintptr(nfd), "accepted from "+l.addr.String())
	defer file.Close()
	// FileConn duplicates the socket close-on-exec atomically.
	return net.FileConn(file)
}

func (l *Listener) Close() error {
	l.closed.Store(true)
	err := l.file.Close()
	if l.unlink != "" {
		if rmErr := os.Remove(l.unlink); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return errors.Join(err, rmErr)
		}
	}
	return err
}

func (l *Listener) Addr() net.Addr { return l.addr }
