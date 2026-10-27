package link

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"remote-agents/internal/control"
	"remote-agents/internal/hookexec"
)

// serveAndRegister registers session with a control server over reg and
// answers its hooks with exec, as a session wrapper does.
func serveAndRegister(t *testing.T, reg *control.Registry, session string, exec hookexec.Executor) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ra-link-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- control.Serve(ctx, ln, reg, func() control.Status { return control.Status{} }, func(error) {})
	}()
	held, err := control.Client{Socket: sock}.Register(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- held.ServeHooks(ctx, exec.Run, func(error) {}) }()
	t.Cleanup(func() { cancel(); <-done; <-served })
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := reg.Lookup(session); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("registration never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
