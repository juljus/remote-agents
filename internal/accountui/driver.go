package accountui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
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
	// suspend runs fn with the terminal as it was before the picker.
	suspend func(fn func() error) error
}

func (p picker) run() (string, error) {
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
			return "", p.scr.clear()
		case ActSwitch:
			said, err := p.backend.Do("use", a.Name)
			if err == nil {
				return said, p.scr.clear()
			}
			m.Notify(true, "%v", err)
		case ActNone:
		default:
			if err := p.act(&m, a); err != nil {
				return "", err
			}
		}
	}
}

// act does what a, which stays in the picker, asks, and shows its outcome
// on the status line.
func (p picker) act(m *Model, a Action) error {
	var said string
	var err error
	focus := a.Name
	switch a.Kind {
	case ActAdd:
		err = p.suspend(func() error { return p.backend.Add(a.Name) })
		said = "added " + a.Name
	case ActRename:
		said, err = p.backend.Do("rename", a.Name, a.NewName)
		focus = a.NewName
	case ActSave:
		said, err = p.backend.Do("save", a.Name)
	case ActRemove:
		said, err = p.backend.Do("remove", a.Name)
	case ActStart:
		m.Notify(false, "sending %s a request to start its week…", a.Name)
		if err := p.scr.draw(*m); err != nil {
			return err
		}
		err = p.backend.Start(a.Name)
	}
	if err != nil {
		m.Notify(true, "%v", err)
		return nil
	}
	listing, err := p.backend.List()
	if err != nil {
		m.Notify(true, "%s, but listing the accounts again failed: %v", line(said), err)
		return nil
	}
	m.SetListing(listing, focus)
	if a.Kind == ActStart {
		p.started(m, a.Name)
		return nil
	}
	m.Notify(false, "%s", line(said))
	return nil
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
func Run(in, out *os.File, backend Backend) (string, error) {
	fd := int(in.Fd())
	t := &tty{fd: fd, out: out}
	if err := t.raw(); err != nil {
		return "", err
	}
	defer t.restore()
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
	p.suspend = func(fn func() error) error {
		if err := p.scr.clear(); err != nil {
			return err
		}
		if err := t.restore(); err != nil {
			return err
		}
		ran := fn()
		return errors.Join(ran, t.raw())
	}
	return p.run()
}

// tty switches a terminal between raw mode, with the cursor hidden, and
// the mode it was in.
type tty struct {
	fd    int
	out   io.Writer
	saved *term.State
}

func (t *tty) raw() error {
	saved, err := term.MakeRaw(t.fd)
	if err != nil {
		return err
	}
	t.saved = saved
	_, err = io.WriteString(t.out, "\x1b[?25l")
	return err
}

func (t *tty) restore() error {
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
