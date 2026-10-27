package box

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/linkproto"
)

// shortLayout keeps socket paths under the macOS sun_path limit.
func shortLayout(t *testing.T) boxlayout.Layout {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "ra-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return boxlayout.Layout{Home: home, Name: boxlayout.DefaultName}
}

type openRecorder struct{ urls chan string }

func (o openRecorder) OpenURL(_ context.Context, url string) error      { o.urls <- url; return nil }
func (openRecorder) ClipboardTargets(context.Context) ([]string, error) { return nil, nil }
func (openRecorder) ClipboardRead(context.Context, string) ([]byte, error) {
	return nil, errors.New("empty")
}
func (openRecorder) ClipboardWrite(context.Context, string, []byte) error { return nil }
func (openRecorder) Hook(context.Context, linkproto.HookRequest) (linkproto.HookResult, error) {
	return linkproto.HookResult{}, nil
}

type hold struct {
	toBox  *io.PipeWriter
	result chan error
}

// startHold runs LinkHold against an in-process laptop end.
func startHold(t *testing.T, l boxlayout.Layout, laptop linkproto.Handler, heartbeat, timeout time.Duration) hold {
	t.Helper()
	boxIn, toBox := io.Pipe()
	fromBox, boxOut := io.Pipe()
	ready := make(chan struct{})
	h := hold{toBox: toBox, result: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		h.result <- LinkHold(ctx, l, boxIn, boxOut, timeout, func(error) {})
		boxOut.Close()
	}()
	go func() {
		_ = linkproto.ServeMux(ctx, fromBox, toBox, linkproto.DispatchTo(laptop), heartbeat, func() { close(ready) }, func(error) {})
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("link holder not ready")
	}
	return h
}

func client(l boxlayout.Layout) linkproto.Client {
	return linkproto.Client{Socket: l.LaptopSocket(), DialTimeout: time.Second}
}

func TestLinkHoldServesAndCleansUp(t *testing.T) {
	l := shortLayout(t)
	rec := openRecorder{urls: make(chan string, 1)}
	h := startHold(t, l, rec, 50*time.Millisecond, time.Second)
	if err := client(l).OpenURL(context.Background(), "https://example.com"); err != nil {
		t.Fatalf("OpenURL through the relay: %v", err)
	}
	if got := <-rec.urls; got != "https://example.com" {
		t.Fatalf("laptop got %q", got)
	}
	h.toBox.Close()
	if err := <-h.result; err != nil {
		t.Fatalf("LinkHold: %v", err)
	}
	if _, err := os.Lstat(l.LaptopSocket()); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestNewerLinkTakesOverAndSurvivesOlderExit(t *testing.T) {
	l := shortLayout(t)
	older := startHold(t, l, openRecorder{urls: make(chan string, 1)}, 50*time.Millisecond, time.Second)
	newerRec := openRecorder{urls: make(chan string, 1)}
	newer := startHold(t, l, newerRec, 50*time.Millisecond, time.Second)
	older.toBox.Close()
	if err := <-older.result; err != nil {
		t.Fatalf("older LinkHold: %v", err)
	}
	if err := client(l).OpenURL(context.Background(), "https://new"); err != nil {
		t.Fatalf("newer link unreachable after older exit: %v", err)
	}
	if got := <-newerRec.urls; got != "https://new" {
		t.Fatalf("got %q", got)
	}
	newer.toBox.Close()
	<-newer.result
}

func TestLinkHoldGivesUpWithoutHeartbeats(t *testing.T) {
	l := shortLayout(t)
	h := startHold(t, l, openRecorder{}, time.Hour, 150*time.Millisecond)
	select {
	case err := <-h.result:
		if err == nil || !strings.Contains(err.Error(), "nothing from the laptop") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("LinkHold kept running without heartbeats")
	}
	if _, err := os.Lstat(l.LaptopSocket()); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
	err := client(l).OpenURL(context.Background(), "https://x")
	if !errors.Is(err, linkproto.ErrLinkDown) {
		t.Fatalf("err = %v, want ErrLinkDown", err)
	}
	if _, err := os.Stat(filepath.Dir(l.LaptopSocket())); err != nil {
		t.Fatal(err)
	}
}
