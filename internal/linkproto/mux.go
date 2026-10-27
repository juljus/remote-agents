package linkproto

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"remote-agents/internal/jsonline"
)

// Frame travels over the link's ssh session. The box sends Ready once, then
// Requests and Cancels; the laptop sends Pings and Responses.
type Frame struct {
	ID       uint64    `json:"id,omitempty"`
	Request  *Request  `json:"request,omitempty"`
	Response *Response `json:"response,omitempty"`
	Cancel   bool      `json:"cancel,omitempty"`
	Ping     bool      `json:"ping,omitempty"`
	Ready    bool      `json:"ready,omitempty"`
}

// frameMax leaves room for base64 and framing around a MaxMessage payload.
const frameMax = MaxMessage/3*4 + 4096

type frameWriter struct {
	mu     sync.Mutex
	w      io.Writer
	closed atomic.Bool
}

var errWriterClosed = errors.New("frame stream closed")

func (f *frameWriter) write(fr Frame) error {
	if f.closed.Load() {
		return errWriterClosed
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return jsonline.Write(f.w, fr)
}

// Dispatch answers one request.
type Dispatch func(ctx context.Context, req Request) Response

// DispatchTo answers requests with h.
func DispatchTo(h Handler) Dispatch {
	return func(ctx context.Context, req Request) Response { return dispatch(ctx, h, req) }
}

// Refused is a failed response with reason.
func Refused(reason string) Response {
	return Response{Error: reason, ErrorCode: codeFailed}
}

// HookOnly answers hook requests with run and refuses every other operation.
func HookOnly(run func(context.Context, HookRequest) (HookResult, error)) Dispatch {
	return func(ctx context.Context, req Request) Response {
		if req.Hook == nil {
			return Refused("only hook requests are served here")
		}
		res, err := run(ctx, *req.Hook)
		if err != nil {
			return Refused(err.Error())
		}
		return Response{Hook: &res}
	}
}

func readFrames(ctx context.Context, in io.Reader) (<-chan Frame, <-chan error) {
	frames := make(chan Frame)
	errs := make(chan error, 1)
	go func() {
		r := bufio.NewReader(in)
		for {
			fr, err := jsonline.Read[Frame](r, frameMax)
			if err != nil {
				errs <- err
				return
			}
			select {
			case frames <- fr:
			case <-ctx.Done():
				return
			}
		}
	}()
	return frames, errs
}

// ServeMux answers requests arriving on in with dispatch and pings the other
// end every heartbeat. It returns nil when in reaches EOF.
func ServeMux(ctx context.Context, in io.Reader, out io.Writer, dispatch Dispatch, heartbeat time.Duration, onReady func(), logf func(error)) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	w := &frameWriter{w: out}
	pingErr := make(chan error, 1)
	go func() { pingErr <- ping(ctx, w, heartbeat) }()

	var mu sync.Mutex
	inflight := map[uint64]context.CancelFunc{}
	frames, readErr := readFrames(ctx, in)
	for {
		var fr Frame
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-pingErr:
			return fmt.Errorf("sending heartbeat: %w", err)
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("reading requests: %w", err)
		case fr = <-frames:
		}
		switch {
		case fr.Ready:
			onReady()
		case fr.Cancel:
			mu.Lock()
			if c, ok := inflight[fr.ID]; ok {
				c()
			}
			mu.Unlock()
		case fr.Request != nil:
			reqCtx, reqCancel := context.WithCancel(ctx)
			mu.Lock()
			inflight[fr.ID] = reqCancel
			mu.Unlock()
			wg.Add(1)
			go func(id uint64, req Request) {
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(inflight, id)
					mu.Unlock()
					reqCancel()
				}()
				resp := Refused("invalid request")
				if err := req.Validate(); err == nil {
					resp = dispatch(reqCtx, req)
				}
				if err := w.write(Frame{ID: id, Response: &resp}); err != nil {
					logf(fmt.Errorf("answering request %d: %w", id, err))
				}
			}(fr.ID, *fr.Request)
		default:
			return fmt.Errorf("unexpected frame: %+v", fr)
		}
	}
}

func ping(ctx context.Context, w *frameWriter, every time.Duration) error {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if err := w.write(Frame{Ping: true}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Caller sends requests over a frame stream and matches the responses that
// its owner feeds back through deliver.
type Caller struct {
	w       *frameWriter
	mu      sync.Mutex
	next    uint64
	waiting map[uint64]chan Response
	closed  chan struct{}
	err     error
}

func newCaller(out io.Writer) *Caller {
	return &Caller{w: &frameWriter{w: out}, waiting: map[uint64]chan Response{}, closed: make(chan struct{})}
}

// Call returns ctx.Err() when ctx ends first, after telling the other end to cancel.
func (c *Caller) Call(ctx context.Context, req Request) (Response, error) {
	c.mu.Lock()
	c.next++
	id := c.next
	answer := make(chan Response, 1)
	c.waiting[id] = answer
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiting, id)
		c.mu.Unlock()
	}()
	if err := c.w.write(Frame{ID: id, Request: &req}); err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrLinkDown, err)
	}
	select {
	case resp := <-answer:
		return resp, nil
	case <-c.closed:
		return Response{}, c.err
	case <-ctx.Done():
		if err := c.w.write(Frame{ID: id, Cancel: true}); err != nil {
			return Response{}, errors.Join(ctx.Err(), err)
		}
		return Response{}, ctx.Err()
	}
}

func (c *Caller) deliver(id uint64, resp Response) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.waiting[id]; ok {
		ch <- resp
		delete(c.waiting, id)
	}
}

// close fails every pending and future call with err.
func (c *Caller) close(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
	default:
		c.err = err
		close(c.closed)
	}
}

// CallOver returns a Caller over conn and runs its response reader until conn
// ends; calls then fail with closedErr.
func CallOver(ctx context.Context, conn io.ReadWriter, closedErr error) (*Caller, <-chan error) {
	c := newCaller(conn)
	done := make(chan error, 1)
	go func() {
		frames, readErr := readFrames(ctx, conn)
		var err error
	loop:
		for {
			select {
			case <-ctx.Done():
				err = ctx.Err()
				break loop
			case err = <-readErr:
				if errors.Is(err, io.EOF) {
					err = nil
				}
				break loop
			case fr := <-frames:
				switch {
				case fr.Ping:
				case fr.Response != nil:
					c.deliver(fr.ID, *fr.Response)
				default:
					err = fmt.Errorf("unexpected frame: %+v", fr)
					break loop
				}
			}
		}
		c.close(closedErr)
		done <- err
	}()
	return c, done
}

// activityReader reports every read that returns data, so a large frame in
// transit keeps the heartbeat alive.
type activityReader struct {
	r    io.Reader
	seen chan<- struct{}
}

func (a activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		select {
		case a.seen <- struct{}{}:
		default:
		}
	}
	return n, err
}

// Relay is the box end: it serves shim connections on ln and forwards each
// request to the laptop over out, matching responses read from in. It stops
// when nothing arrives from the laptop for heartbeatTimeout, so shims fail
// fast instead of hanging on a dead link. On return it closes ln and every
// open shim connection; a request write stuck on a stalled out is abandoned.
func Relay(ctx context.Context, ln net.Listener, in io.Reader, out io.Writer, heartbeatTimeout time.Duration, logf func(error)) error {
	ctx, cancel := context.WithCancel(ctx)
	c := newCaller(out)
	var mu sync.Mutex
	conns := map[net.Conn]struct{}{}
	shutDown := false
	defer func() {
		ln.Close()
		c.w.closed.Store(true)
		c.close(ErrLinkDown)
		cancel()
		mu.Lock()
		shutDown = true
		for conn := range conns {
			conn.Close()
		}
		mu.Unlock()
	}()
	if err := c.w.write(Frame{Ready: true}); err != nil {
		return fmt.Errorf("signalling ready: %w", err)
	}
	acceptErr := make(chan error, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			mu.Lock()
			if shutDown {
				mu.Unlock()
				conn.Close()
				return
			}
			conns[conn] = struct{}{}
			mu.Unlock()
			go func() {
				if err := relayConn(ctx, conn, c); err != nil {
					logf(err)
				}
				mu.Lock()
				delete(conns, conn)
				mu.Unlock()
			}()
		}
	}()

	seen := make(chan struct{}, 1)
	frames, readErr := readFrames(ctx, activityReader{r: in, seen: seen})
	timer := time.NewTimer(heartbeatTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-acceptErr:
			return fmt.Errorf("accepting shim connection: %w", err)
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("reading from laptop: %w", err)
		case <-timer.C:
			return fmt.Errorf("nothing from the laptop for %s", heartbeatTimeout)
		case <-seen:
			timer.Reset(heartbeatTimeout)
		case fr := <-frames:
			switch {
			case fr.Ping:
			case fr.Response != nil:
				c.deliver(fr.ID, *fr.Response)
			default:
				return fmt.Errorf("unexpected frame from laptop: %+v", fr)
			}
		}
	}
}

func relayConn(ctx context.Context, conn net.Conn, c *Caller) error {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	req, err := jsonline.Read[Request](reader, MaxMessage)
	if err != nil {
		err = fmt.Errorf("reading shim request: %w", err)
		return errors.Join(err, jsonline.Write(conn, Refused(err.Error())))
	}
	// The shim never sends more after its request; a completed read means it hung up.
	callCtx, hangUp := context.WithCancel(ctx)
	defer hangUp()
	go func() {
		var b [1]byte
		_, _ = reader.Read(b[:])
		hangUp()
	}()
	resp, err := c.Call(callCtx, req)
	switch {
	case err == nil:
		return jsonline.Write(conn, resp)
	case callCtx.Err() != nil && ctx.Err() == nil:
		return nil
	}
	return jsonline.Write(conn, Response{Error: err.Error(), ErrorCode: codeLinkDown})
}
