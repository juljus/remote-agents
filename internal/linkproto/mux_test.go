package linkproto

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"remote-agents/internal/jsonline"
	"strings"
	"testing"
	"time"
)

// wire connects a laptop-side ServeMux and a box-side Relay through pipes,
// as the ssh session's stdin and stdout would.
type wire struct {
	sock      string
	laptopErr chan error
	relayErr  chan error
	ready     chan struct{}
	cancel    context.CancelFunc
	toBox     *io.PipeWriter
}

func connect(t *testing.T, h Handler, heartbeat, timeout time.Duration) *wire {
	t.Helper()
	dir, err := shortTempDir(t)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "laptop.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	boxIn, toBox := io.Pipe()
	fromBox, boxOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	w := &wire{sock: sock, laptopErr: make(chan error, 1), relayErr: make(chan error, 1), ready: make(chan struct{}), cancel: cancel, toBox: toBox}
	go func() {
		w.relayErr <- Relay(ctx, ln, boxIn, boxOut, timeout, func(error) {})
		boxOut.Close()
	}()
	go func() {
		w.laptopErr <- ServeMux(ctx, fromBox, toBox, DispatchTo(h), heartbeat, func() { close(w.ready) }, func(error) {})
	}()
	t.Cleanup(func() {
		cancel()
		toBox.Close()
	})
	select {
	case <-w.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("relay never reported ready")
	}
	return w
}

func TestMuxRoundTrips(t *testing.T) {
	h := &fakeHandler{clipboard: map[string][]byte{"image/png": {0x89, 'P', 'N', 'G'}}, hookSeen: make(chan HookRequest, 1)}
	w := connect(t, h, 50*time.Millisecond, time.Second)
	c := Client{Socket: w.sock, DialTimeout: time.Second}
	ctx := context.Background()

	if err := c.OpenURL(ctx, "https://example.com/x?y=1"); err != nil {
		t.Fatalf("OpenURL: %v", err)
	}
	if err := c.OpenURL(ctx, "file:///etc/passwd"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected handler error, got %v", err)
	}
	data, err := c.ClipboardRead(ctx, "image/png")
	if err != nil || string(data) != "\x89PNG" {
		t.Fatalf("ClipboardRead = %q, %v", data, err)
	}
	res, err := c.Hook(ctx, HookRequest{Session: "claude-x", Argv: []string{"hooks", "claude", "stop"}, Stdin: []byte("{}")})
	if err != nil || string(res.Stdout) != "out:{}" || res.ExitCode != 2 {
		t.Fatalf("Hook = %+v, %v", res, err)
	}
}

func TestMuxConcurrentRequests(t *testing.T) {
	h := &fakeHandler{clipboard: map[string][]byte{"text/plain": []byte("x")}}
	w := connect(t, h, 50*time.Millisecond, time.Second)
	c := Client{Socket: w.sock, DialTimeout: time.Second}
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			_, err := c.ClipboardRead(context.Background(), "text/plain")
			errs <- err
		}()
	}
	for i := 0; i < 20; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
}

func TestMuxClientHangupCancelsHandler(t *testing.T) {
	h := &fakeHandler{hookSeen: make(chan HookRequest, 1), hookBlock: true, hookDone: make(chan error, 1)}
	w := connect(t, h, 50*time.Millisecond, time.Second)
	c := Client{Socket: w.sock, DialTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = c.Hook(ctx, HookRequest{Session: "s", Argv: []string{"hooks"}}) }()
	<-h.hookSeen
	cancel()
	select {
	case err := <-h.hookDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handler ctx err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler not cancelled after the box client hung up")
	}
}

func TestRelayStopsWithoutHeartbeats(t *testing.T) {
	h := &fakeHandler{}
	// The laptop pings far less often than the relay's timeout allows.
	w := connect(t, h, time.Hour, 200*time.Millisecond)
	select {
	case err := <-w.relayErr:
		if err == nil || !strings.Contains(err.Error(), "nothing from the laptop") {
			t.Fatalf("relay err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay kept running without heartbeats")
	}
	c := Client{Socket: w.sock, DialTimeout: time.Second}
	if err := c.OpenURL(context.Background(), "https://x"); !errors.Is(err, ErrLinkDown) {
		t.Fatalf("after relay stop err = %v, want ErrLinkDown", err)
	}
}

func TestRelayEndsOnLaptopEOF(t *testing.T) {
	w := connect(t, &fakeHandler{}, 50*time.Millisecond, time.Second)
	w.toBox.Close()
	select {
	case err := <-w.relayErr:
		if err != nil {
			t.Fatalf("relay err = %v, want clean EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not end on EOF")
	}
}

// slowReader trickles data in small chunks, like a large frame on a slow uplink.
type slowReader struct {
	r     io.Reader
	chunk int
	pause time.Duration
}

func (s slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.pause)
	if len(p) > s.chunk {
		p = p[:s.chunk]
	}
	return s.r.Read(p)
}

func TestRelayKeepsALargeSlowResponseAlive(t *testing.T) {
	big := bytes.Repeat([]byte{0xAB}, 200_000)
	h := &fakeHandler{clipboard: map[string][]byte{"image/png": big}}
	dir, err := shortTempDir(t)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "laptop.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	boxIn, toBox := io.Pipe()
	fromBox, boxOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	// The response takes ~1s to arrive while the relay tolerates only 300ms of silence.
	go func() {
		_ = Relay(ctx, ln, slowReader{r: boxIn, chunk: 8192, pause: 30 * time.Millisecond}, boxOut, 300*time.Millisecond, func(error) {})
	}()
	go func() {
		_ = ServeMux(ctx, fromBox, toBox, DispatchTo(h), 100*time.Millisecond, func() { close(ready) }, func(error) {})
	}()
	<-ready
	data, err := Client{Socket: sock, DialTimeout: time.Second}.ClipboardRead(context.Background(), "image/png")
	if err != nil || !bytes.Equal(data, big) {
		t.Fatalf("large read: %d bytes, %v", len(data), err)
	}
}

func TestInFlightRequestIsLinkDownWhenRelayStops(t *testing.T) {
	h := &fakeHandler{hookSeen: make(chan HookRequest, 1), hookBlock: true, hookDone: make(chan error, 1)}
	w := connect(t, h, 50*time.Millisecond, time.Second)
	errs := make(chan error, 1)
	go func() {
		_, err := Client{Socket: w.sock, DialTimeout: time.Second}.Hook(context.Background(), HookRequest{Session: "s", Argv: []string{"hooks"}})
		errs <- err
	}()
	<-h.hookSeen
	w.toBox.Close()
	select {
	case err := <-errs:
		if !errors.Is(err, ErrLinkDown) {
			t.Fatalf("err = %v, want ErrLinkDown", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request never failed")
	}
}

func TestMalformedShimRequestGetsAnError(t *testing.T) {
	w := connect(t, &fakeHandler{}, 50*time.Millisecond, time.Second)
	conn, err := net.Dial("unix", w.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("{not json\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := jsonline.Read[Response](bufio.NewReader(conn), MaxMessage)
	if err != nil {
		t.Fatalf("no reply to a malformed request: %v", err)
	}
	if err := resp.Err(); err == nil || errors.Is(err, ErrLinkDown) || !strings.Contains(err.Error(), "reading shim request") {
		t.Fatalf("reply error = %v, want an explicit request error", err)
	}
}
