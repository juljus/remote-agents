// Package control is the laptop-local protocol between session wrappers and
// the link daemon. A wrapper registers its session and keeps the connection
// open; the daemon forwards the session's hook requests over it, so hooks run
// inside the wrapper's terminal, where cmux accepts them. The registration
// ends when the connection does.
package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"remote-agents/internal/jsonline"
	"remote-agents/internal/linkproto"
)

const maxMessage = 1 << 20

var (
	ErrDaemonDown    = errors.New("remote-agents link daemon is not running")
	ErrNotRegistered = errors.New("no terminal registered for session")
)

type RegisterRequest struct {
	Session string `json:"session"`
}

type Request struct {
	Register *RegisterRequest `json:"register,omitempty"`
	Status   *struct{}        `json:"status,omitempty"`
}

type Status struct {
	LinkUp bool   `json:"link_up"`
	Detail string `json:"detail"`
}

type Response struct {
	Error  string  `json:"error,omitempty"`
	Status *Status `json:"status,omitempty"`
}

// Registry maps sessions to the wrapper currently showing them.
type Registry struct {
	mu       sync.Mutex
	sessions map[string]*linkproto.Caller
}

func NewRegistry() *Registry {
	return &Registry{sessions: map[string]*linkproto.Caller{}}
}

func (r *Registry) add(session string, c *linkproto.Caller) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[session] = c
}

// remove drops c only if no newer registration replaced it.
func (r *Registry) remove(session string, c *linkproto.Caller) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[session] == c {
		delete(r.sessions, session)
	}
}

func (r *Registry) Lookup(session string) (*linkproto.Caller, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.sessions[session]
	if !ok {
		return nil, fmt.Errorf("%w %s", ErrNotRegistered, session)
	}
	return c, nil
}

func Serve(ctx context.Context, ln net.Listener, reg *Registry, status func() Status, logf func(error)) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("accepting control connection: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := serveConn(ctx, conn, reg, status); err != nil {
				logf(err)
			}
		}()
	}
}

type readWriter struct {
	io.Reader
	io.Writer
}

func serveConn(ctx context.Context, conn net.Conn, reg *Registry, status func() Status) error {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	reader := bufio.NewReader(conn)
	req, err := jsonline.Read[Request](reader, maxMessage)
	if err != nil {
		return fmt.Errorf("reading control request: %w", err)
	}
	switch {
	case req.Register != nil && req.Status == nil:
		if err := jsonline.Write(conn, Response{}); err != nil {
			return err
		}
		caller, done := linkproto.CallOver(ctx, readWriter{reader, conn}, linkproto.ErrNoTerminal)
		reg.add(req.Register.Session, caller)
		defer reg.remove(req.Register.Session, caller)
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("registration of %s: %w", req.Register.Session, err)
		}
		return nil
	case req.Status != nil && req.Register == nil:
		st := status()
		return jsonline.Write(conn, Response{Status: &st})
	}
	return jsonline.Write(conn, Response{Error: "request must carry exactly one operation"})
}

type Client struct {
	Socket string
}

func (c Client) call(ctx context.Context, req Request) (net.Conn, *bufio.Reader, Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, nil, Response{}, fmt.Errorf("%w: %v", ErrDaemonDown, err)
	}
	if err := jsonline.Write(conn, req); err != nil {
		conn.Close()
		return nil, nil, Response{}, err
	}
	reader := bufio.NewReader(conn)
	resp, err := jsonline.Read[Response](reader, maxMessage)
	if err != nil {
		conn.Close()
		return nil, nil, Response{}, fmt.Errorf("reading control response: %w", err)
	}
	if resp.Error != "" {
		conn.Close()
		return nil, nil, Response{}, fmt.Errorf("link daemon: %s", resp.Error)
	}
	return conn, reader, resp, nil
}

func (c Client) Status(ctx context.Context) (Status, error) {
	conn, _, resp, err := c.call(ctx, Request{Status: &struct{}{}})
	if err != nil {
		return Status{}, err
	}
	conn.Close()
	if resp.Status == nil {
		return Status{}, errors.New("link daemon: status response without status")
	}
	return *resp.Status, nil
}

// Held is a live registration; hook requests arrive on it as linkproto frames.
type Held struct {
	conn   net.Conn
	reader *bufio.Reader
}

func (h *Held) Read(p []byte) (int, error)  { return h.reader.Read(p) }
func (h *Held) Write(p []byte) (int, error) { return h.conn.Write(p) }
func (h *Held) Close() error                { return h.conn.Close() }

func (c Client) Register(ctx context.Context, session string) (*Held, error) {
	conn, reader, _, err := c.call(ctx, Request{Register: &RegisterRequest{Session: session}})
	if err != nil {
		return nil, err
	}
	return &Held{conn: conn, reader: reader}, nil
}

// ServeHooks answers the daemon's hook requests on h with run until the
// daemon goes away or ctx ends.
func (h *Held) ServeHooks(ctx context.Context, run func(context.Context, linkproto.HookRequest) (linkproto.HookResult, error), logf func(error)) error {
	stop := context.AfterFunc(ctx, func() { h.conn.Close() })
	defer stop()
	err := linkproto.ServeMux(ctx, h, h, linkproto.HookOnly(run), linkproto.HeartbeatInterval, func() {}, logf)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
