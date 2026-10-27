package link

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"remote-agents/internal/cloexec"
	"remote-agents/internal/control"
	"remote-agents/internal/laptoppaths"
	"remote-agents/internal/linkproto"
	"remote-agents/internal/reconnect"
	"remote-agents/internal/sshrun"
)

type Daemon struct {
	SSH      string
	Box      sshrun.Box
	Paths    laptoppaths.Paths
	Handler  linkproto.Handler
	Registry *control.Registry
	Triggers <-chan string
	Logf     func(format string, args ...any)

	link *connState
}

// connState tracks one ssh connection and logs its transitions.
type connState struct {
	name string
	logf func(format string, args ...any)
	mu   sync.Mutex
	st   control.Status
}

func (c *connState) get() control.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st
}

func (c *connState) set(up bool, detail string) {
	c.mu.Lock()
	changed := c.st.LinkUp != up || c.st.Detail != detail
	c.st = control.Status{LinkUp: up, Detail: detail}
	c.mu.Unlock()
	if changed {
		state := "down"
		if up {
			state = "up"
		}
		c.logf("%s %s: %s", c.name, state, detail)
	}
}

func (c *connState) AttemptStarting(int)                    {}
func (c *connState) AttemptEnded(_ int, r reconnect.Result) { c.set(false, r.Detail) }
func (c *connState) Triggered(reason string)                { c.logf("%s: trigger: %s", c.name, reason) }

func (d *Daemon) Status() control.Status { return d.link.get() }

// Run serves the control socket and keeps the link up until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	d.link = &connState{name: "link", logf: d.Logf}
	ln, err := d.listen(ctx)
	if err != nil {
		return err
	}
	errs := make(chan error, 2)
	go func() {
		errs <- control.Serve(ctx, ln, d.Registry, d.Status, func(err error) { d.Logf("control: %v", err) })
	}()
	go func() {
		loop := reconnect.Loop{
			Attempt:     d.attempt,
			Triggers:    d.Triggers,
			Interval:    time.Second,
			After:       time.After,
			Observer:    d.link,
			StableAfter: 10 * time.Second,
		}
		_, err := loop.Run(ctx)
		errs <- err
	}()
	return <-errs
}

func (d *Daemon) listen(ctx context.Context) (net.Listener, error) {
	if err := os.MkdirAll(d.Paths.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating state dir: %w", err)
	}
	if err := os.Chmod(d.Paths.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("securing state dir: %w", err)
	}
	sock := d.Paths.ControlSocket()
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := (control.Client{Socket: sock}).Status(probe); err == nil {
		return nil, errors.New("another link daemon is already running")
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing stale control socket: %w", err)
	}
	// Wrapper connections must not leak into the ssh processes the daemon starts.
	ln, err := cloexec.ListenUnix(sock)
	if err != nil {
		return nil, fmt.Errorf("listening on control socket: %w", err)
	}
	return ln, nil
}

// attempt holds one link connection. The link never finishes on its own, so
// every ending is reported as a transport failure to be retried.
func (d *Daemon) attempt(ctx context.Context) reconnect.Result {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return reconnect.Result{Err: fmt.Errorf("creating pipe: %w", err)}
	}
	defer stdinW.Close()
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		return reconnect.Result{Err: fmt.Errorf("creating pipe: %w", err)}
	}
	defer stdoutR.Close()

	sshCtx, sshCancel := context.WithCancel(ctx)
	defer sshCancel()
	muxErr := make(chan error, 1)
	go func() {
		onReady := func() { d.link.set(true, "connected to "+d.Box.Host) }
		logf := func(err error) { d.Logf("relay: %v", err) }
		muxErr <- linkproto.ServeMux(sshCtx, stdoutR, stdinW, limit(linkproto.DispatchTo(d.Handler), maxConcurrent), linkproto.HeartbeatInterval, onReady, logf)
		// A broken relay must not leave a connected but useless link behind.
		sshCancel()
	}()
	res := sshrun.Run(sshCtx, sshrun.LinkArgv(d.SSH, d.Box),
		sshrun.Stdio{Stdin: stdinR, Stdout: stdoutW, Stderr: lineLogger{prefix: "box: ", logf: d.Logf}}, 2*time.Second)
	stdinR.Close()
	stdoutW.Close()
	merr := <-muxErr
	if merr != nil && errors.Is(merr, context.Canceled) {
		merr = nil
	}
	if res.Err != nil || res.Transport {
		if merr != nil {
			res.Detail = merr.Error()
		}
		return res
	}
	detail := fmt.Sprintf("link holder exited with status %d", res.ExitCode)
	if merr != nil {
		detail += ": " + merr.Error()
	}
	return reconnect.Result{Transport: true, Detail: detail}
}

// maxConcurrent bounds the requests the box can have in flight on the laptop.
const maxConcurrent = 32

func limit(dispatch linkproto.Dispatch, n int) linkproto.Dispatch {
	slots := make(chan struct{}, n)
	return func(ctx context.Context, req linkproto.Request) linkproto.Response {
		select {
		case slots <- struct{}{}:
		default:
			return linkproto.Refused(fmt.Sprintf("more than %d requests in flight", n))
		}
		defer func() { <-slots }()
		return dispatch(ctx, req)
	}
}

type lineLogger struct {
	prefix string
	logf   func(format string, args ...any)
}

func (l lineLogger) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			l.logf("%s%s", l.prefix, line)
		}
	}
	return len(p), nil
}
