// Package linkproto is the request/response protocol between box-side shims
// and the laptop link daemon. Shims talk to the box relay over a unix socket,
// one JSON request line and one JSON response line per connection; the relay
// multiplexes requests to the laptop over the link's ssh session.
package linkproto

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"remote-agents/internal/jsonline"
)

// MaxMessage bounds one request or response; clipboard images are the largest payload.
const MaxMessage = 64 << 20

// The laptop writes a heartbeat line to the box's link holder every
// HeartbeatInterval; the holder unpublishes the socket after HeartbeatTimeout
// without one.
const (
	HeartbeatInterval = 2 * time.Second
	HeartbeatTimeout  = 6 * time.Second
)

var (
	// ErrLinkDown means the laptop socket is not reachable from the box.
	ErrLinkDown = errors.New("laptop link is down")
	// ErrNoTerminal means no laptop terminal is registered for the hook's session.
	ErrNoTerminal = errors.New("no laptop terminal registered for this session")
)

type OpenURLRequest struct {
	URL string `json:"url"`
}

type ClipboardReadRequest struct {
	MIME string `json:"mime"`
}

type ClipboardWriteRequest struct {
	MIME string `json:"mime"`
	Data []byte `json:"data"`
}

type HookRequest struct {
	Session string   `json:"session"`
	Argv    []string `json:"argv"`
	Stdin   []byte   `json:"stdin"`
}

type HookResult struct {
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Request holds exactly one operation.
type Request struct {
	OpenURL          *OpenURLRequest        `json:"open_url,omitempty"`
	ClipboardTargets *struct{}              `json:"clipboard_targets,omitempty"`
	ClipboardRead    *ClipboardReadRequest  `json:"clipboard_read,omitempty"`
	ClipboardWrite   *ClipboardWriteRequest `json:"clipboard_write,omitempty"`
	Hook             *HookRequest           `json:"hook,omitempty"`
}

func (r Request) Validate() error {
	n := 0
	for _, set := range []bool{r.OpenURL != nil, r.ClipboardTargets != nil, r.ClipboardRead != nil, r.ClipboardWrite != nil, r.Hook != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("request must carry exactly one operation, got %d", n)
	}
	return nil
}

type errorCode string

const (
	codeFailed     errorCode = "failed"
	codeNoTerminal errorCode = "no_terminal"
	codeLinkDown   errorCode = "link_down"
)

type Response struct {
	Error     string      `json:"error,omitempty"`
	ErrorCode errorCode   `json:"error_code,omitempty"`
	Targets   []string    `json:"targets,omitempty"`
	Data      []byte      `json:"data,omitempty"`
	Hook      *HookResult `json:"hook,omitempty"`
}

// Err is the failure the response carries, mapped back to the sentinel errors.
func (r Response) Err() error {
	switch r.ErrorCode {
	case "":
		return nil
	case codeNoTerminal:
		return fmt.Errorf("%w: %s", ErrNoTerminal, r.Error)
	case codeLinkDown:
		return fmt.Errorf("%w: %s", ErrLinkDown, r.Error)
	}
	return fmt.Errorf("laptop: %s", r.Error)
}

type Handler interface {
	OpenURL(ctx context.Context, url string) error
	ClipboardTargets(ctx context.Context) ([]string, error)
	ClipboardRead(ctx context.Context, mime string) ([]byte, error)
	ClipboardWrite(ctx context.Context, mime string, data []byte) error
	Hook(ctx context.Context, req HookRequest) (HookResult, error)
}

func dispatch(ctx context.Context, h Handler, req Request) Response {
	var resp Response
	var err error
	switch {
	case req.OpenURL != nil:
		err = h.OpenURL(ctx, req.OpenURL.URL)
	case req.ClipboardTargets != nil:
		resp.Targets, err = h.ClipboardTargets(ctx)
	case req.ClipboardRead != nil:
		resp.Data, err = h.ClipboardRead(ctx, req.ClipboardRead.MIME)
	case req.ClipboardWrite != nil:
		err = h.ClipboardWrite(ctx, req.ClipboardWrite.MIME, req.ClipboardWrite.Data)
	case req.Hook != nil:
		var res HookResult
		res, err = h.Hook(ctx, *req.Hook)
		resp.Hook = &res
	}
	if err != nil {
		code := codeFailed
		if errors.Is(err, ErrNoTerminal) {
			code = codeNoTerminal
		}
		return Response{Error: err.Error(), ErrorCode: code}
	}
	return resp
}

type Client struct {
	Socket      string
	DialTimeout time.Duration
}

func (c Client) call(ctx context.Context, req Request) (Response, error) {
	dialer := net.Dialer{Timeout: c.DialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrLinkDown, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := jsonline.Write(conn, req); err != nil {
		return Response{}, err
	}
	resp, err := jsonline.Read[Response](bufio.NewReader(conn), MaxMessage)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		// The relay closes shim connections when the link goes away.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Response{}, fmt.Errorf("%w: relay closed the connection", ErrLinkDown)
		}
		return Response{}, fmt.Errorf("reading response: %w", err)
	}
	if err := resp.Err(); err != nil {
		return Response{}, err
	}
	return resp, nil
}

func (c Client) OpenURL(ctx context.Context, url string) error {
	_, err := c.call(ctx, Request{OpenURL: &OpenURLRequest{URL: url}})
	return err
}

func (c Client) ClipboardTargets(ctx context.Context) ([]string, error) {
	resp, err := c.call(ctx, Request{ClipboardTargets: &struct{}{}})
	return resp.Targets, err
}

func (c Client) ClipboardRead(ctx context.Context, mime string) ([]byte, error) {
	resp, err := c.call(ctx, Request{ClipboardRead: &ClipboardReadRequest{MIME: mime}})
	return resp.Data, err
}

func (c Client) ClipboardWrite(ctx context.Context, mime string, data []byte) error {
	_, err := c.call(ctx, Request{ClipboardWrite: &ClipboardWriteRequest{MIME: mime, Data: data}})
	return err
}

func (c Client) Hook(ctx context.Context, req HookRequest) (HookResult, error) {
	resp, err := c.call(ctx, Request{Hook: &req})
	if err != nil {
		return HookResult{}, err
	}
	if resp.Hook == nil {
		return HookResult{}, errors.New("laptop: hook response without result")
	}
	return *resp.Hook, nil
}
