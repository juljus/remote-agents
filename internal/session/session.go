// Package session is the laptop side of one interactive agent session: it
// keeps an ssh connection to the box's shpool session alive, reconnecting
// whenever the link drops, until the agent exits or the user detaches.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"remote-agents/internal/control"
	"remote-agents/internal/linkproto"
	"remote-agents/internal/paste"
	"remote-agents/internal/reconnect"
	"remote-agents/internal/spec"
	"remote-agents/internal/sshrun"
)

// Registration makes this terminal the one the session's hooks report to.
type Registration struct {
	Control control.Client
	// RunHook runs a hook the box relays for this session.
	RunHook func(context.Context, linkproto.HookRequest) (linkproto.HookResult, error)
}

type Options struct {
	SSH  string
	Box  sshrun.Box
	Spec spec.Spec
	// ReattachHint is the command that brings this session back, shown on detach.
	ReattachHint string
	// ExitHint, called once the agent exits, gives a line to print then, or "".
	ExitHint func() string
	// Registration, when set, keeps the session's terminal registered with the link daemon.
	Registration *Registration
	Triggers     <-chan string
	Stdin        *os.File
	Stdout       *os.File
	Stderr       *os.File
	// Logf records problems that must not be drawn over the agent's screen.
	Logf func(format string, args ...any)
}

// resetModes undoes terminal modes an agent may have left enabled when its own
// cleanup never reached this terminal: kitty keyboard flags, modifyOtherKeys,
// focus events, bracketed paste, mouse reporting, hidden cursor and colours.
const resetModes = "\x1b[<99u\x1b[>4;0m\x1b[?1004l\x1b[?2004l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?25h\x1b[0m"

type detached struct{ sig os.Signal }

func (d detached) Error() string { return "detached by " + d.sig.String() }

// Run returns the agent's exit code, or 128+signal when the user detached.
func Run(ctx context.Context, opts Options) (int, error) {
	fd := int(opts.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return 0, errors.New("an interactive session needs a terminal on stdin")
	}
	// shpool cannot restore a 0x0 screen; such a "terminal" would leave a broken session behind.
	if cols, rows, err := term.GetSize(fd); err != nil {
		return 0, fmt.Errorf("reading terminal size: %w", err)
	} else if cols == 0 || rows == 0 {
		return 0, fmt.Errorf("terminal reports size %dx%d", cols, rows)
	}
	original, err := term.GetState(fd)
	if err != nil {
		return 0, fmt.Errorf("reading terminal state: %w", err)
	}
	if err := opts.Spec.Validate(); err != nil {
		return 0, err
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		select {
		case sig := <-sigs:
			cancel(detached{sig})
		case <-ctx.Done():
		}
	}()

	if opts.Registration != nil {
		held, err := opts.Registration.Control.Register(ctx, opts.Spec.Session)
		if err != nil {
			return 0, err
		}
		go keepRegistered(ctx, opts, held)
	}

	keys := &input{fd: fd, logf: opts.Logf, onInterrupt: func() { cancel(detached{syscall.SIGINT}) }, onError: func(err error) { cancel(err) }}
	go keys.run(ctx)

	status := &outage{
		out: opts.Stderr,
		width: func() (int, error) {
			cols, _, err := term.GetSize(fd)
			return cols, err
		},
		now:      time.Now,
		host:     opts.Box.Host,
		interval: statusInterval,
		logf:     opts.Logf,
	}
	obs := &observer{keys: keys, status: status}
	// ssh's stderr is only captured, so the session's output is the one other writer to the terminal.
	stdout := newScreenOut(opts.Stdout, status, status.width, opts.Logf)
	defer stdout.close()
	// ssh reads a terminal of its own, so the terminal stays raw throughout and resizes are passed on.
	if _, err := term.MakeRaw(fd); err != nil {
		return 0, fmt.Errorf("making the terminal raw: %w", err)
	}
	attached := &current{outer: opts.Stdin, logf: opts.Logf}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for {
			select {
			case <-winch:
				attached.resize()
			case <-ctx.Done():
				return
			}
		}
	}()
	attempts := 0
	loop := reconnect.Loop{
		// The loop runs one attempt at a time; each is numbered so the box can tell a late one from its successor.
		Attempt: func(ctx context.Context) reconnect.Result {
			attempts++
			s := opts.Spec
			s.Attempt = attempts
			token, err := s.Encode()
			if err != nil {
				return reconnect.Result{Err: err}
			}
			res := runAttempt(ctx, opts, keys, attached, stdout, token)
			if !res.Transport && res.ExitCode == spec.ExitStaleAttempt {
				res.Transport, res.Detail = true, "a newer connection attempt holds the session"
			}
			return res
		},
		Triggers:    opts.Triggers,
		Interval:    time.Second,
		After:       time.After,
		Observer:    obs,
		StableAfter: 10 * time.Second,
	}
	code, loopErr := loop.Run(ctx)
	keys.stop()
	statusShown := status.End()
	var d detached
	isDetach := loopErr != nil && errors.As(context.Cause(ctx), &d)
	sig, _ := d.sig.(syscall.Signal)
	if isDetach && sig == syscall.SIGHUP {
		// The terminal is gone; there is nothing to restore or tell.
		return 128 + int(sig), nil
	}
	if err := term.Restore(fd, original); err != nil {
		return 0, fmt.Errorf("restoring terminal: %w", err)
	}
	if loopErr == nil {
		// After an outage or a takeover the agent's exit sequences went to another attachment.
		if obs.dropped || code == spec.ExitTakenOver {
			fmt.Fprint(opts.Stderr, resetModes)
		}
		if code != spec.ExitTakenOver && opts.ExitHint != nil {
			if hint := opts.ExitHint(); hint != "" {
				fmt.Fprintf(opts.Stderr, "%s\r\n", hint)
			}
		}
		return code, nil
	}
	if !isDetach {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			return 0, cause
		}
		return 0, loopErr
	}
	// A cleared status line is already a fresh line.
	lead := "\r\n"
	if statusShown {
		lead = ""
	}
	fmt.Fprintf(opts.Stderr, "%s%s[remote-agents] detached; %s keeps running on %s\r\n[remote-agents] reattach with: %s\r\n",
		resetModes, lead, opts.Spec.Session, opts.Box.Host, opts.ReattachHint)
	return 128 + int(sig), nil
}

// keepRegistered serves hooks on held and registers again whenever the
// daemon goes away, until ctx ends.
func keepRegistered(ctx context.Context, opts Options, held *control.Held) {
	reg := opts.Registration
	session := opts.Spec.Session
	logErr := func(err error) { opts.Logf("%s: hook relay: %v", session, err) }
	for {
		if err := held.ServeHooks(ctx, reg.RunHook, logErr); ctx.Err() != nil {
			return
		} else if err != nil {
			logErr(err)
		}
		held.Close()
		opts.Logf("%s: link daemon went away; registering again", session)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			var err error
			if held, err = reg.Control.Register(ctx, session); err == nil {
				opts.Logf("%s: registered again", session)
				break
			}
			logErr(err)
		}
	}
}

// statusInterval paces the reconnect spinner.
const statusInterval = 100 * time.Millisecond

type observer struct {
	keys   *input
	status *outage
	// dropped records that the link failed at least once during the run.
	dropped bool
}

func (o *observer) AttemptStarting(int) {}

func (o *observer) AttemptEnded(_ int, r reconnect.Result) {
	if !r.Transport {
		return
	}
	o.dropped = true
	o.status.Down(r.Detail)
	o.keys.watch()
}

func (o *observer) Triggered(string) {}

// runAttempt runs one ssh connection on a terminal of its own, which the
// user's keystrokes reach through the paste filter.
func runAttempt(ctx context.Context, opts Options, keys *input, attached *current, out *screenOut, token string) reconnect.Result {
	t, err := openInner(opts.Stdin, out, opts.Logf)
	if err != nil {
		return reconnect.Result{Err: err}
	}
	attached.set(t)
	pasteCtx, cancelPaste := context.WithCancel(ctx)
	defer cancelPaste()
	keys.forward(&paste.Filter{W: t, Rewrite: pasteRewriter(pasteCtx, opts, out)})
	res := sshrun.Run(ctx, sshrun.SessionArgv(opts.SSH, opts.Box, token), sshrun.Stdio{Stdin: t.slave, Stdout: out, Terminal: true}, 2*time.Second)
	attached.set(nil)
	// An upload or a keystroke write in flight holds the input; both end here.
	cancelPaste()
	if err := t.Close(); err != nil && res.Err == nil {
		res.Err = fmt.Errorf("closing ssh's terminal: %w", err)
	}
	keys.stop()
	return res
}

// input reads the user's terminal while the session runs. During an attempt
// the keystrokes go to its terminal through the paste filter; between
// attempts they are dropped, and Ctrl+C detaches. stop blocks until no read
// is in flight, so input is never taken from whatever reads the terminal next.
type input struct {
	fd          int
	logf        func(format string, args ...any)
	onInterrupt func()
	onError     func(error)

	mu     sync.Mutex
	active atomic.Bool
	// filter is the running attempt's; nil between attempts.
	filter *paste.Filter
}

func (in *input) forward(f *paste.Filter) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.filter = f
	in.active.Store(true)
}

func (in *input) watch() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.filter = nil
	in.active.Store(true)
}

func (in *input) stop() {
	in.active.Store(false)
	in.mu.Lock()
	defer in.mu.Unlock()
	in.filter = nil
}

func (in *input) run(ctx context.Context) {
	buf := make([]byte, 32<<10)
	for ctx.Err() == nil {
		if !in.active.Load() {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if err := in.poll(buf); err != nil {
			in.onError(fmt.Errorf("reading terminal: %w", err))
			return
		}
	}
}

func (in *input) poll(buf []byte) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.active.Load() {
		return nil
	}
	wait := 100 * time.Millisecond
	if in.filter != nil && in.filter.HoldFor() > 0 {
		wait = in.filter.HoldFor()
	}
	fds := []unix.PollFd{{Fd: int32(in.fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(wait/time.Millisecond))
	if errors.Is(err, unix.EINTR) {
		return nil
	}
	if err != nil {
		return err
	}
	if n == 0 {
		if in.filter != nil && in.filter.HoldFor() > 0 {
			in.deliver(in.filter.Flush())
		}
		return nil
	}
	k, err := unix.Read(in.fd, buf)
	if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
		return nil
	}
	if err != nil {
		return err
	}
	if k == 0 {
		return io.EOF
	}
	if in.filter == nil {
		if isInterrupt(buf[:k]) {
			in.onInterrupt()
		}
		return nil
	}
	_, err = in.filter.Write(buf[:k])
	in.deliver(err)
	return nil
}

// deliver handles a failed write to the attempt's terminal: the attempt is
// ending, so the keystrokes go nowhere until the next one.
func (in *input) deliver(err error) {
	if err == nil {
		return
	}
	if !errors.Is(err, os.ErrClosed) {
		in.logf("passing keystrokes to ssh: %v", err)
	}
	in.filter = nil
}
