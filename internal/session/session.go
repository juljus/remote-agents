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
	token, err := opts.Spec.Encode()
	if err != nil {
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

	keys := &keyWatcher{fd: fd, onInterrupt: func() { cancel(detached{syscall.SIGINT}) }, onError: func(err error) { cancel(err) }}
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
	obs := &observer{opts: opts, fd: fd, keys: keys, status: status}
	argv := sshrun.SessionArgv(opts.SSH, opts.Box, token)
	// ssh's stderr is only captured, so the session's output is the one other writer to the terminal.
	stdout := outputGate{w: opts.Stdout, outage: status}
	loop := reconnect.Loop{
		Attempt: func(ctx context.Context) reconnect.Result {
			return sshrun.Run(ctx, argv, sshrun.Stdio{Stdin: opts.Stdin, Stdout: stdout}, 2*time.Second)
		},
		Triggers:    opts.Triggers,
		Interval:    time.Second,
		After:       time.After,
		Observer:    obs,
		StableAfter: 10 * time.Second,
	}
	code, loopErr := loop.Run(ctx)
	keys.Pause()
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
	opts   Options
	fd     int
	keys   *keyWatcher
	status *outage
	// dropped records that the link failed at least once during the run.
	dropped bool
}

func (o *observer) AttemptStarting(int) {
	o.keys.Pause()
}

func (o *observer) AttemptEnded(_ int, r reconnect.Result) {
	if !r.Transport {
		return
	}
	// ssh restored the terminal to whatever it found; take raw mode back so
	// keystrokes during the outage are read here instead of echoed.
	if _, err := term.MakeRaw(o.fd); err != nil {
		o.opts.Logf("%s: restoring raw mode during the outage: %v", o.opts.Spec.Session, err)
	}
	o.dropped = true
	o.status.Down(r.Detail)
	o.keys.Resume()
}

func (o *observer) Triggered(string) {}

// keyWatcher reads the terminal between connection attempts, discarding
// input and detaching on Ctrl+C. Pause blocks until no read is in flight so
// it never steals input from the next ssh.
type keyWatcher struct {
	fd          int
	onInterrupt func()
	onError     func(error)
	mu          sync.Mutex
	active      atomic.Bool
}

func (w *keyWatcher) Pause() {
	w.active.Store(false)
	w.mu.Lock()
	defer w.mu.Unlock()
}

func (w *keyWatcher) Resume() { w.active.Store(true) }

func (w *keyWatcher) run(ctx context.Context) {
	buf := make([]byte, 512)
	for ctx.Err() == nil {
		if !w.active.Load() {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if err := w.poll(buf); err != nil {
			w.onError(fmt.Errorf("reading terminal: %w", err))
			return
		}
	}
}

func (w *keyWatcher) poll(buf []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active.Load() {
		return nil
	}
	fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 100)
	if errors.Is(err, unix.EINTR) || n == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	k, err := unix.Read(w.fd, buf)
	if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
		return nil
	}
	if err != nil {
		return err
	}
	if k == 0 {
		return io.EOF
	}
	if isInterrupt(buf[:k]) {
		w.onInterrupt()
	}
	return nil
}
