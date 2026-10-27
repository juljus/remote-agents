package sshrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"remote-agents/internal/reconnect"
	"slices"
	"strings"
	"testing"
	"time"
)

var testBox = Box{Host: "box", Install: "ra-test"}

func TestSessionArgv(t *testing.T) {
	got := SessionArgv("ssh", testBox, "TOKEN")
	want := append(append([]string{"ssh"}, commonOptions...),
		"-tt", "-e", "none", "box", `exec "$HOME/.local/share/ra-test/bin/remote-agents-box" attach TOKEN`)
	if !slices.Equal(got, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", got, want)
	}
}

func TestExecArgv(t *testing.T) {
	tty := ExecArgv("ssh", testBox, "TOKEN", true)
	if !slices.Contains(tty, "-tt") {
		t.Fatalf("tty exec without -tt: %q", tty)
	}
	pipe := ExecArgv("ssh", testBox, "TOKEN", false)
	if !slices.Contains(pipe, "-T") || slices.Contains(pipe, "-tt") {
		t.Fatalf("non-tty exec argv: %q", pipe)
	}
	if pipe[len(pipe)-1] != `exec "$HOME/.local/share/ra-test/bin/remote-agents-box" exec TOKEN` {
		t.Fatalf("remote command = %q", pipe[len(pipe)-1])
	}
}

func TestLinkArgv(t *testing.T) {
	got := LinkArgv("ssh", testBox)
	want := append(append([]string{"ssh"}, commonOptions...), "-T", "box",
		`exec "$HOME/.local/share/ra-test/bin/remote-agents-box" link-hold`)
	if !slices.Equal(got, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", got, want)
	}
}

func TestTunnelArgv(t *testing.T) {
	got := TunnelArgv("ssh", "box", 8250)
	want := append(append([]string{"ssh"}, commonOptions...), "-W", "localhost:8250", "box")
	if !slices.Equal(got, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", got, want)
	}
}

func TestListeningArgv(t *testing.T) {
	got := ListeningArgv("ssh", testBox, 8250)
	want := append(append([]string{"ssh"}, commonOptions...), "-T", "box",
		`exec "$HOME/.local/share/ra-test/bin/remote-agents-box" listening 8250`)
	if !slices.Equal(got, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", got, want)
	}
}

func fakeSSH(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func runFake(t *testing.T, ctx context.Context, body string) (res reconnect.Result) {
	t.Helper()
	return Run(ctx, []string{fakeSSH(t, body)}, Stdio{}, time.Second)
}

func TestRunClassifiesExits(t *testing.T) {
	ctx := context.Background()
	if r := runFake(t, ctx, "exit 0"); r.Transport || r.ExitCode != 0 || r.Err != nil {
		t.Fatalf("exit 0 -> %+v", r)
	}
	if r := runFake(t, ctx, "exit 3"); r.Transport || r.ExitCode != 3 {
		t.Fatalf("exit 3 -> %+v", r)
	}
	r := runFake(t, ctx, "echo 'ssh: connect to host box port 22: Network is unreachable' >&2; exit 255")
	if !r.Transport || !strings.Contains(r.Detail, "Network is unreachable") {
		t.Fatalf("exit 255 -> %+v", r)
	}
	if r := runFake(t, ctx, "kill -KILL $$"); !r.Transport || r.Detail != "ssh ended by signal 9 (killed)" {
		t.Fatalf("killed -> %+v", r)
	}
}

func TestRunCancelTerminatesSSH(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan reconnect.Result, 1)
	go func() { done <- runFake(t, ctx, "trap 'exit 255' TERM; while :; do sleep 0.05; done") }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case r := <-done:
		if !r.Transport {
			t.Fatalf("cancelled -> %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ssh not terminated on cancel")
	}
}

func TestRunCancelKillsStubbornSSH(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan reconnect.Result, 1)
	go func() { done <- runFake(t, ctx, "trap '' TERM; while :; do sleep 0.05; done") }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case r := <-done:
		if !r.Transport {
			t.Fatalf("killed -> %+v", r)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("stubborn ssh not killed")
	}
}

// A child that inherits ssh's piped stdout, such as a LocalCommand, must not keep Run waiting.
func TestRunReturnsWhileAChildHoldsStdout(t *testing.T) {
	cases := map[string]struct {
		body   string
		cancel bool
		check  func(reconnect.Result) bool
	}{
		"exit 0":    {body: "sleep 5 & exit 0", check: func(r reconnect.Result) bool { return r.Err == nil && !r.Transport && r.ExitCode == 0 }},
		"exit 3":    {body: "sleep 5 & exit 3", check: func(r reconnect.Result) bool { return r.Err == nil && !r.Transport && r.ExitCode == 3 }},
		"exit 255":  {body: "sleep 5 & exit 255", check: func(r reconnect.Result) bool { return r.Err == nil && r.Transport }},
		"cancelled": {body: "sleep 5 & exec sleep 30", cancel: true, check: func(r reconnect.Result) bool { return r.Err == nil && r.Transport }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out bytes.Buffer
			done := make(chan reconnect.Result, 1)
			ssh := fakeSSH(t, tc.body)
			go func() { done <- Run(ctx, []string{ssh}, Stdio{Stdout: &out}, 500*time.Millisecond) }()
			if tc.cancel {
				time.Sleep(200 * time.Millisecond)
				cancel()
			}
			select {
			case r := <-done:
				if !tc.check(r) {
					t.Fatalf("result %+v", r)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Run waited for the child holding stdout")
			}
		})
	}
}

func TestRunMissingBinaryIsFatal(t *testing.T) {
	r := Run(context.Background(), []string{"/nonexistent/ssh"}, Stdio{}, time.Second)
	if r.Err == nil {
		t.Fatalf("expected fatal error, got %+v", r)
	}
}
