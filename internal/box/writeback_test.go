package box

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/linkproto"
)

// laptopTaking answers write-backs as a laptop that takes every copy,
// sending each request on seen.
func laptopTaking(seen chan<- linkproto.WriteBackRequest) *fakeLaptop {
	return &fakeLaptop{writeBack: func(req linkproto.WriteBackRequest) (linkproto.WriteBackResult, error) {
		sum, err := fileSum(req.BoxPath)
		if err != nil {
			return linkproto.WriteBackResult{}, err
		}
		seen <- req
		return linkproto.WriteBackResult{Outcome: "written", Path: req.Origin, SHA256: sum}, nil
	}}
}

// watchLayout watches l's session copies, as a link holder does, until the test ends.
func watchLayout(t *testing.T, l boxlayout.Layout, laptop Laptop) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchSessionCopies(ctx, l, laptop, 10*time.Millisecond, 50*time.Millisecond, t.Logf)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func expectWriteBack(t *testing.T, seen <-chan linkproto.WriteBackRequest, want linkproto.WriteBackRequest) {
	t.Helper()
	select {
	case got := <-seen:
		if got != want {
			t.Fatalf("write-back %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no write-back of %s", want.BoxPath)
	}
}

func expectNone(t *testing.T, seen <-chan linkproto.WriteBackRequest) {
	t.Helper()
	select {
	case got := <-seen:
		t.Fatalf("unexpected write-back %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAnEditedCopyGoesBackToTheLaptopOnce(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/notes.md", "laptop"}, [2]string{"/l/other.md", "other"})
	seen := make(chan linkproto.WriteBackRequest, 10)
	watchLayout(t, l, laptopTaking(seen))
	expectNone(t, seen)

	if err := os.WriteFile(paths[0], []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectWriteBack(t, seen, linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/notes.md", BoxPath: paths[0]})
	expectNone(t, seen)
	copies, err := sessionCopies(l)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range copies {
		if c.Path == paths[0] && c.Synced != sha("agent") {
			t.Fatalf("synced %s, want the agent's", c.Synced)
		}
	}

	// An editor's write through a rename is a change too.
	tmp := paths[0] + ".swp"
	if err := os.WriteFile(tmp, []byte("agent 2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, paths[0]); err != nil {
		t.Fatal(err)
	}
	expectWriteBack(t, seen, linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/notes.md", BoxPath: paths[0]})
}

func TestCopiesChangedWhileTheLinkWasDownGoBackOnceItIsUp(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/notes.md", "laptop"})
	if err := os.WriteFile(paths[0], []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := make(chan linkproto.WriteBackRequest, 10)
	watchLayout(t, l, laptopTaking(seen))
	expectWriteBack(t, seen, linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/notes.md", BoxPath: paths[0]})
	expectNone(t, seen)
}

func TestAFailedWriteBackIsTriedAgain(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/notes.md", "laptop"})
	var mu sync.Mutex
	tries := 0
	seen := make(chan linkproto.WriteBackRequest, 10)
	taking := laptopTaking(seen)
	laptop := &fakeLaptop{writeBack: func(req linkproto.WriteBackRequest) (linkproto.WriteBackResult, error) {
		mu.Lock()
		tries++
		first := tries == 1
		mu.Unlock()
		if first {
			return linkproto.WriteBackResult{}, errors.New("laptop: fetching: connection lost")
		}
		return taking.writeBack(req)
	}}
	watchLayout(t, l, laptop)
	if err := os.WriteFile(paths[0], []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectWriteBack(t, seen, linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/notes.md", BoxPath: paths[0]})
}

func TestARefusedWriteBackWaitsForTheNextChange(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/notes.md", "laptop"})
	seen := make(chan linkproto.WriteBackRequest, 10)
	laptop := &fakeLaptop{writeBack: func(req linkproto.WriteBackRequest) (linkproto.WriteBackResult, error) {
		seen <- req
		return linkproto.WriteBackResult{}, fmt.Errorf("laptop: %w: /l/notes.md was not pasted into this session", linkproto.ErrRefused)
	}}
	watchLayout(t, l, laptop)
	if err := os.WriteFile(paths[0], []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/notes.md", BoxPath: paths[0]}
	expectWriteBack(t, seen, want)
	// The retry delay in watchLayout is 50 ms; a refusal is not retried at all.
	expectNone(t, seen)
	if err := os.WriteFile(paths[0], []byte("agent 2"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectWriteBack(t, seen, want)
}

// A link holder lingers until its link times out; only one may send write-backs.
func TestOneWatcherAtATimeSendsWriteBacks(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/notes.md", "laptop"})
	older := make(chan linkproto.WriteBackRequest, 10)
	ctx, stopOlder := context.WithCancel(context.Background())
	olderDone := make(chan struct{})
	go func() {
		defer close(olderDone)
		watchSessionCopies(ctx, l, laptopTaking(older), 10*time.Millisecond, 50*time.Millisecond, t.Logf)
	}()
	t.Cleanup(func() { stopOlder(); <-olderDone })
	time.Sleep(50 * time.Millisecond)
	newer := make(chan linkproto.WriteBackRequest, 10)
	watchLayout(t, l, laptopTaking(newer))

	want := linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/notes.md", BoxPath: paths[0]}
	if err := os.WriteFile(paths[0], []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectWriteBack(t, older, want)
	expectNone(t, newer)
	stopOlder()
	<-olderDone
	if err := os.WriteFile(paths[0], []byte("agent 2"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectWriteBack(t, newer, want)
}
