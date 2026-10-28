package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// inner is the terminal one attempt's ssh reads instead of the user's, so
// this process sees every keystroke first. ssh puts it in raw mode, as it
// would the user's, and takes its size from it.
type inner struct {
	// master is non-blocking, so closing it ends a write or read in flight.
	master *os.File
	slave  *os.File
	// drained is closed once nothing reads master any more.
	drained chan struct{}
}

// openInner makes an inner terminal of outer's size. Nothing but ssh's own
// use of /dev/tty writes to it; that output goes to out.
func openInner(outer *os.File, out io.Writer, logf func(format string, args ...any)) (*inner, error) {
	m, slave, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("opening a terminal for ssh: %w", err)
	}
	// os.File reads a descriptor that is non-blocking when wrapped through the poller.
	fd, err := unix.Dup(int(m.Fd()))
	if err = errors.Join(err, m.Close()); err != nil {
		return nil, errors.Join(fmt.Errorf("duplicating terminal: %w", err), slave.Close())
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, errors.Join(fmt.Errorf("making terminal non-blocking: %w", err), unix.Close(fd), slave.Close())
	}
	t := &inner{master: os.NewFile(uintptr(fd), "ptmx"), slave: slave, drained: make(chan struct{})}
	go func() {
		defer close(t.drained)
		// Closing either end ends the copy with one of these.
		if _, err := io.Copy(out, t.master); err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, syscall.EIO) {
			logf("relaying ssh's terminal output: %v", err)
		}
	}()
	// Raw before ssh starts, so input that arrives first is neither echoed nor edited.
	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		return nil, errors.Join(fmt.Errorf("making ssh's terminal raw: %w", err), t.Close())
	}
	if err := pty.InheritSize(outer, slave); err != nil {
		return nil, errors.Join(fmt.Errorf("sizing ssh's terminal: %w", err), t.Close())
	}
	return t, nil
}

func (t *inner) Write(p []byte) (int, error) { return t.master.Write(p) }

// Close ends the terminal once ssh has exited.
func (t *inner) Close() error {
	err := errors.Join(t.slave.Close(), t.master.Close())
	<-t.drained
	return err
}

// current is the inner terminal of the running attempt, which resizes follow.
type current struct {
	mu    sync.Mutex
	outer *os.File
	t     *inner
	logf  func(format string, args ...any)
}

func (c *current) set(t *inner) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// resize gives the running attempt's terminal the outer one's size; the
// kernel then signals ssh, which tells the box.
func (c *current) resize() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.t == nil {
		return
	}
	if err := pty.InheritSize(c.outer, c.t.slave); err != nil {
		c.logf("resizing ssh's terminal: %v", err)
	}
}
