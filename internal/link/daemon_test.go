package link

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"remote-agents/internal/control"
	"remote-agents/internal/laptoppaths"
	"remote-agents/internal/linkproto"
	"remote-agents/internal/sshrun"
)

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (l *logSink) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

func (l *logSink) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, s) {
			return true
		}
	}
	return false
}

func TestDaemonBringsLinkUpAndReconnects(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ra-d-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	calls := filepath.Join(dir, "calls")
	ssh := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
echo "$*" >> ` + calls + `
echo '{"ready":true}'
sleep 0.3
echo "Connection closed by remote host" >&2
exit 255
`
	if err := os.WriteFile(ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	logs := &logSink{}
	reg := control.NewRegistry()
	d := &Daemon{
		SSH: ssh, Box: sshrun.Box{Host: "box", Install: "ra-test"},
		Paths:    laptoppaths.Paths{StateDir: filepath.Join(dir, "state")},
		Handler:  &Handler{Pasteboard: &fakePasteboard{}, Opener: &recordOpener{}, Registry: reg},
		Registry: reg,
		Triggers: make(chan string),
		Logf:     logs.logf,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for {
		data, _ := os.ReadFile(calls)
		links := strings.Count(string(data), "link-hold")
		if links >= 2 && logs.contains("link up") && logs.contains("box: Connection closed by remote host") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("link did not cycle; calls:\n%s\nlogs: %v", data, logs.snapshot())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := (control.Client{Socket: d.Paths.ControlSocket()}).Status(ctx); err != nil {
		t.Fatalf("control socket not served: %v", err)
	}
	second := &Daemon{Paths: d.Paths, Logf: logs.logf}
	if err := second.Run(ctx); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second daemon err = %v", err)
	}
	cancel()
	<-done
}

func TestLimitRefusesBeyondCapacity(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	dispatch := limit(func(context.Context, linkproto.Request) linkproto.Response {
		close(started)
		<-release
		return linkproto.Response{}
	}, 1)
	done := make(chan linkproto.Response, 1)
	go func() { done <- dispatch(context.Background(), linkproto.Request{}) }()
	<-started
	if resp := dispatch(context.Background(), linkproto.Request{}); resp.Err() == nil {
		t.Fatal("second request should be refused while the first is in flight")
	}
	close(release)
	if resp := <-done; resp.Err() != nil {
		t.Fatalf("first request: %v", resp.Err())
	}
}
