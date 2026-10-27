package control

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"remote-agents/internal/linkproto"
)

type daemon struct {
	sock string
	reg  *Registry
	stop func()
}

func serve(t *testing.T, status Status) daemon {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ra-ctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, reg, func() Status { return status }, func(error) {}) }()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return daemon{sock: sock, reg: reg, stop: stop}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func registerServing(t *testing.T, d daemon, session, tag string) (*Held, chan error) {
	t.Helper()
	held, err := Client{Socket: d.sock}.Register(context.Background(), session)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	served := make(chan error, 1)
	go func() {
		served <- held.ServeHooks(context.Background(), func(_ context.Context, req linkproto.HookRequest) (linkproto.HookResult, error) {
			return linkproto.HookResult{Stdout: []byte(tag + ":" + string(req.Stdin)), ExitCode: 4}, nil
		}, func(error) {})
	}()
	eventually(t, func() bool { _, err := d.reg.Lookup(session); return err == nil })
	return held, served
}

func callHook(t *testing.T, d daemon, session string) (linkproto.Response, error) {
	t.Helper()
	c, err := d.reg.Lookup(session)
	if err != nil {
		return linkproto.Response{}, err
	}
	return c.Call(context.Background(), linkproto.Request{Hook: &linkproto.HookRequest{Session: session, Argv: []string{"hooks", "claude", "stop"}, Stdin: []byte("{}")}})
}

func TestHooksRunInTheRegisteredWrapper(t *testing.T) {
	d := serve(t, Status{})
	held, _ := registerServing(t, d, "claude-x", "wrapper")
	resp, err := callHook(t, d, "claude-x")
	if err != nil || resp.Hook == nil || string(resp.Hook.Stdout) != "wrapper:{}" || resp.Hook.ExitCode != 4 {
		t.Fatalf("hook = %+v, %v", resp, err)
	}
	held.Close()
	eventually(t, func() bool { _, err := d.reg.Lookup("claude-x"); return errors.Is(err, ErrNotRegistered) })
}

func TestNewerRegistrationWins(t *testing.T) {
	d := serve(t, Status{})
	old, _ := registerServing(t, d, "claude-x", "old")
	_, _ = registerServing(t, d, "claude-x", "new")
	eventually(t, func() bool {
		resp, err := callHook(t, d, "claude-x")
		return err == nil && string(resp.Hook.Stdout) == "new:{}"
	})
	old.Close()
	time.Sleep(100 * time.Millisecond)
	resp, err := callHook(t, d, "claude-x")
	if err != nil || string(resp.Hook.Stdout) != "new:{}" {
		t.Fatalf("after old closed: %+v, %v", resp, err)
	}
}

func TestStatus(t *testing.T) {
	d := serve(t, Status{LinkUp: true, Detail: "connected"})
	st, err := Client{Socket: d.sock}.Status(context.Background())
	if err != nil || !st.LinkUp || st.Detail != "connected" {
		t.Fatalf("Status = %+v, %v", st, err)
	}
}

func TestDaemonNotRunning(t *testing.T) {
	_, err := Client{Socket: "/nonexistent/control.sock"}.Status(context.Background())
	if !errors.Is(err, ErrDaemonDown) {
		t.Fatalf("err = %v, want ErrDaemonDown", err)
	}
}

func TestServeHooksEndsWhenTheDaemonGoesAway(t *testing.T) {
	d := serve(t, Status{})
	_, served := registerServing(t, d, "claude-y", "w")
	d.stop()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHooks did not notice the daemon going away")
	}
}
