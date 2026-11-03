package accountui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"remote-agents/internal/accounts"
)

// Backend does on the box what the picker's keys ask for.
type Backend interface {
	List() (accounts.Listing, error)
	// Do runs an account verb that needs no terminal, such as use or
	// rename, and returns what the box printed.
	Do(args ...string) (string, error)
	// Add signs a new account in, the terminal its own meanwhile.
	Add(name string) error
	// Start sends the account a request, to start its weekly window.
	Start(name string) error
}

// screen draws the picker in place: each draw replaces the last.
type screen struct {
	out   io.Writer
	width func() (int, error)
	color bool
	now   func() time.Time
	drawn int
}

func (s *screen) draw(m Model) error {
	width, err := s.width()
	if err != nil {
		return err
	}
	lines := m.View(width, s.now(), s.color)
	_, err = io.WriteString(s.out, s.rewind()+strings.Join(lines, "\r\n"))
	s.drawn = len(lines)
	return err
}

func (s *screen) clear() error {
	if s.drawn == 0 {
		return nil
	}
	_, err := io.WriteString(s.out, s.rewind())
	s.drawn = 0
	return err
}

// rewind returns to the first line drawn, and erases from there down.
func (s *screen) rewind() string {
	up := ""
	if s.drawn > 1 {
		up = fmt.Sprintf("\x1b[%dA", s.drawn-1)
	}
	return "\r" + up + "\x1b[J"
}

// picker runs the picker until a key leaves it, and returns what the box
// said when the picker switched it to another account.
type picker struct {
	src     byteSource
	scr     *screen
	backend Backend
	// suspend runs fn with the terminal as it was before the picker; its
	// error is the terminal's, which ends the picker.
	suspend func(fn func()) error
}

func (p picker) run() (string, error) {
	said, err := p.loop()
	if err != nil {
		return "", errors.Join(err, p.scr.clear())
	}
	return said, p.scr.clear()
}

func (p picker) loop() (string, error) {
	listing, err := p.backend.List()
	if err != nil {
		return "", err
	}
	m := New(listing)
	for {
		if err := p.scr.draw(m); err != nil {
			return "", err
		}
		k, err := readKey(p.src)
		if err != nil {
			return "", err
		}
		a := m.Key(k)
		switch a.Kind {
		case ActQuit:
			return "", nil
		case ActNone:
		default:
			said, switched, err := p.act(&m, a)
			if err != nil || switched {
				return said, err
			}
		}
	}
}

// act does what a asks. A switch that worked ends the picker; any other
// outcome shows on the status line, over the accounts listed again.
func (p picker) act(m *Model, a Action) (said string, switched bool, err error) {
	var failed error
	focus := a.Name
	switch a.Kind {
	case ActSwitch:
		said, failed = p.backend.Do("use", a.Name)
		if failed == nil {
			return said, true, nil
		}
	case ActAdd:
		if err := p.suspend(func() { failed = p.backend.Add(a.Name) }); err != nil {
			return "", false, err
		}
		said = "saved " + a.Name
	case ActRename:
		said, failed = p.backend.Do("rename", a.Name, a.NewName)
		focus = a.NewName
	case ActSave:
		said, failed = p.backend.Do("save", a.Name)
	case ActRemove:
		said, failed = p.backend.Do("remove", a.Name)
	case ActStart:
		m.Notify(false, "sending %s a request to start its week…", a.Name)
		if err := p.scr.draw(*m); err != nil {
			return "", false, err
		}
		failed = p.backend.Start(a.Name)
	}
	listing, listFailed := p.backend.List()
	if err := p.drain(); err != nil {
		return "", false, err
	}
	switch {
	case listFailed != nil && failed != nil:
		m.Notify(true, "%v; listing the accounts again failed too: %v", failed, listFailed)
	case listFailed != nil:
		m.Notify(true, "%s, but listing the accounts again failed: %v", line(said), listFailed)
	case failed != nil:
		m.SetListing(listing, "")
		m.Notify(true, "%v", failed)
	case a.Kind == ActStart:
		m.SetListing(listing, focus)
		p.started(m, a.Name)
	default:
		m.SetListing(listing, focus)
		m.Notify(false, "%s", line(said))
	}
	return "", false, nil
}

// drain drops the keys typed while the picker waited on the box, so that
// none acts on a table that has changed under it.
func (p picker) drain() error {
	for {
		_, ok, err := p.src.next(0)
		if err != nil || !ok {
			return err
		}
	}
}

// started tells whether the request started name's weekly window.
func (p picker) started(m *Model, name string) {
	row, ok := m.Row(name)
	switch {
	case !ok:
		m.Notify(true, "%s is gone from the box's accounts", name)
	case row.Error != "":
		m.Notify(true, "sent %s a request, but reading its usage failed: %s", name, row.Error)
	case row.NotStarted():
		m.Notify(true, "sent %s a request, but its week still has not started", name)
	case row.Weekly == nil:
		m.Notify(true, "sent %s a request; it has no weekly limit now", name)
	default:
		m.Notify(false, "started %s's week; it resets %s", name, row.Weekly.ResetsAt.In(p.scr.now().Location()).Format("Mon 15:04"))
	}
}

// line is what the box printed, on one line.
func line(said string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(said), "\n", " · ")), " ")
}

// Run runs the picker on the terminal in and out, and returns what the box
// said when the picker switched it to another account; "" when the picker
// was left.
func Run(in, out *os.File, backend Backend) (said string, err error) {
	fd := int(in.Fd())
	t := &tty{fd: fd, out: out}
	if err := t.raw(); err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, t.restore()) }()
	defer restoreOnSignal(t)()
	p := picker{
		src: fdSource{fd: fd},
		scr: &screen{
			out: out,
			width: func() (int, error) {
				w, _, err := term.GetSize(int(out.Fd()))
				return w, err
			},
			color: true,
			now:   time.Now,
		},
		backend: backend,
	}
	p.suspend = func(fn func()) error {
		if err := p.scr.clear(); err != nil {
			return err
		}
		if err := t.restore(); err != nil {
			return err
		}
		fn()
		return t.raw()
	}
	return p.run()
}

// restoreOnSignal puts the terminal back before a signal that ends the
// process takes its default course; the returned func stops watching.
func restoreOnSignal(t *tty) (stop func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-sigs:
			// The process ends with the signal either way; a terminal that
			// cannot be put back has nothing more to tell.
			_ = t.restore()
			signal.Reset(syscall.SIGTERM, syscall.SIGHUP)
			_ = syscall.Kill(os.Getpid(), sig.(syscall.Signal))
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(done)
	}
}

// tty switches a terminal between raw mode, with the cursor hidden, and
// the mode it was in.
type tty struct {
	fd  int
	out io.Writer
	mu  sync.Mutex
	// saved is the mode to restore while the terminal is raw.
	saved *term.State
}

func (t *tty) raw() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	saved, err := term.MakeRaw(t.fd)
	if err != nil {
		return err
	}
	t.saved = saved
	_, err = io.WriteString(t.out, "\x1b[?25l")
	return err
}

func (t *tty) restore() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.saved == nil {
		return nil
	}
	_, err := io.WriteString(t.out, "\x1b[?25h")
	err = errors.Join(err, term.Restore(t.fd, t.saved))
	t.saved = nil
	return err
}

// fdSource reads a terminal's fd directly, a byte at a time, with no
// goroutine left reading it: a sign-in that suspends the picker has the
// terminal to itself.
type fdSource struct {
	fd int
}

func (s fdSource) next(timeout time.Duration) (byte, bool, error) {
	ms := -1
	if timeout >= 0 {
		ms = int(timeout / time.Millisecond)
	}
	fds := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, ms)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		if n == 0 {
			return 0, false, nil
		}
		break
	}
	var buf [1]byte
	for {
		n, err := unix.Read(s.fd, buf[:])
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		if n == 0 {
			return 0, false, io.EOF
		}
		return buf[0], true, nil
	}
}
