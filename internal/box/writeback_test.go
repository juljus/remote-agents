package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// A refused version never reached the laptop: it is the only copy of the
// agent's change, so neither a prune nor a paste may drop it.
func TestARefusedVersionStaysTheAgentsChange(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/notes.md", "laptop"})
	seen := make(chan linkproto.WriteBackRequest, 10)
	laptop := &fakeLaptop{writeBack: func(req linkproto.WriteBackRequest) (linkproto.WriteBackResult, error) {
		seen <- req
		return linkproto.WriteBackResult{}, fmt.Errorf("laptop: %w: /l now leads to /elsewhere", linkproto.ErrRefused)
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchSessionCopies(ctx, l, laptop, 10*time.Millisecond, 50*time.Millisecond, t.Logf)
	}()
	if err := os.WriteFile(paths[0], []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectWriteBack(t, seen, linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/notes.md", BoxPath: paths[0]})
	expectNone(t, seen)
	cancel()
	<-done

	weekAgo := time.Now().Add(-8 * 24 * time.Hour)
	for _, p := range []string{paths[0], filepath.Dir(paths[0]) + ".json"} {
		if err := os.Chtimes(p, weekAgo, weekAgo); err != nil {
			t.Fatal(err)
		}
	}
	if err := prunePasted(l, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(paths[0]); err != nil || string(got) != "agent" {
		t.Fatalf("the refused change was pruned: %q, %v", got, err)
	}
	err := PasteFilesInto(l, pasteSession, exchange(t, []string{"/l/notes.md"}, [2]string{"/l/notes.md", "laptop 2"}), io.Discard, time.Now())
	if err == nil || !strings.Contains(err.Error(), "the laptop refused") {
		t.Fatalf("a paste over the refused change = %v", err)
	}
	if got, _ := os.ReadFile(paths[0]); string(got) != "agent" {
		t.Fatalf("the paste replaced the refused change with %q", got)
	}
}

// A paste of the laptop's unchanged file asks the laptop afresh about the
// version it refused, as when its record of the paste had run out.
func TestPastingAgainOffersARefusedVersionAnew(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/notes.md", "laptop"})
	if err := os.WriteFile(paths[0], []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	copies, err := sessionCopies(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := markRefused(l, copies[0], sha("agent")); err != nil {
		t.Fatal(err)
	}
	pasteInto(t, l, pasteSession, [2]string{"/l/notes.md", "laptop"})
	if copies, err = sessionCopies(l); err != nil || copies[0].Refused != "" || copies[0].Synced != sha("laptop") {
		t.Fatalf("copies %+v, %v", copies, err)
	}
	seen := make(chan linkproto.WriteBackRequest, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchSessionCopies(ctx, l, laptopTaking(seen), 10*time.Millisecond, 50*time.Millisecond, t.Logf)
	}()
	expectWriteBack(t, seen, linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/notes.md", BoxPath: paths[0]})
	cancel()
	<-done

	// Both changed and the laptop refused the agent's version: the paste says where it stays.
	if err := os.WriteFile(paths[0], []byte("agent 2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if copies, err = sessionCopies(l); err != nil {
		t.Fatal(err)
	}
	if err := markRefused(l, copies[0], sha("agent 2")); err != nil {
		t.Fatal(err)
	}
	err = PasteFilesInto(l, pasteSession, exchange(t, []string{"/l/notes.md"}, [2]string{"/l/notes.md", "laptop 2"}), io.Discard, time.Now())
	if err == nil || !strings.Contains(err.Error(), "the laptop refused") || !strings.Contains(err.Error(), paths[0]) {
		t.Fatalf("PasteFilesInto = %v", err)
	}
}

// The link holder's watcher runs on across pastes; a paste that reopens a
// refused version must reach it too.
func TestARunningWatcherOffersAReopenedVersion(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/notes.md", "laptop"})
	var mu sync.Mutex
	refusing := true
	seen := make(chan linkproto.WriteBackRequest, 10)
	taking := laptopTaking(seen)
	laptop := &fakeLaptop{writeBack: func(req linkproto.WriteBackRequest) (linkproto.WriteBackResult, error) {
		mu.Lock()
		refuse := refusing
		mu.Unlock()
		if refuse {
			seen <- req
			return linkproto.WriteBackResult{}, fmt.Errorf("laptop: %w: /l/notes.md was not pasted into this session", linkproto.ErrRefused)
		}
		return taking.writeBack(req)
	}}
	watchLayout(t, l, laptop)
	if err := os.WriteFile(paths[0], []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/notes.md", BoxPath: paths[0]}
	expectWriteBack(t, seen, want)
	expectNone(t, seen)
	mu.Lock()
	refusing = false
	mu.Unlock()
	pasteInto(t, l, pasteSession, [2]string{"/l/notes.md", "laptop"})
	expectWriteBack(t, seen, want)
}

// A write-back may wait up to linkproto.ApprovalTimeout for the user to
// approve it on the laptop; the other copies go back meanwhile.
func TestAWriteBackWaitingForTheUserHoldsUpNoOther(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/.zshrc", "laptop"}, [2]string{"/l/notes.md", "laptop"})
	seen := make(chan linkproto.WriteBackRequest, 10)
	taking := laptopTaking(seen)
	approve := make(chan struct{})
	asking := make(chan struct{}, 1)
	laptop := &fakeLaptop{writeBack: func(req linkproto.WriteBackRequest) (linkproto.WriteBackResult, error) {
		if req.Origin == "/l/.zshrc" {
			asking <- struct{}{}
			<-approve
		}
		return taking.writeBack(req)
	}}
	watchLayout(t, l, laptop)
	t.Cleanup(func() {
		select {
		case <-approve:
		default:
			close(approve)
		}
	})
	if err := os.WriteFile(paths[0], []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	<-asking
	if err := os.WriteFile(paths[1], []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectWriteBack(t, seen, linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/notes.md", BoxPath: paths[1]})
	close(approve)
	expectWriteBack(t, seen, linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/.zshrc", BoxPath: paths[0]})
	expectNone(t, seen)
}

// A record that cannot be read for a while, as one written meanwhile, must
// not make the watcher forget a write-back in flight and send it again.
func TestAListingThatFailsMeanwhileSendsNoCopyTwice(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/notes.md", "laptop"})
	calls := make(chan linkproto.WriteBackRequest, 10)
	release := make(chan struct{})
	taking := laptopTaking(make(chan linkproto.WriteBackRequest, 10))
	laptop := &fakeLaptop{writeBack: func(req linkproto.WriteBackRequest) (linkproto.WriteBackResult, error) {
		calls <- req
		<-release
		return taking.writeBack(req)
	}}
	watchLayout(t, l, laptop)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	if err := os.WriteFile(paths[0], []byte("agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectWriteBack(t, calls, linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/notes.md", BoxPath: paths[0]})
	record := filepath.Dir(paths[0]) + ".json"
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(record, data, 0o600); err != nil {
		t.Fatal(err)
	}
	expectNone(t, calls)
	once.Do(func() { close(release) })
	expectNone(t, calls)
}

// A laptop file a save made follows its box file, wherever that is: each
// change goes, through a rename or a rebuild too, until the box file is gone
// for unlinkPolls.
func TestALaptopSavedFileFollowsItsBoxFileUntilItIsGone(t *testing.T) {
	l := testLayout(t)
	path := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if linked, err := linkSaved(l, pasteSession, "/l/Downloads/report.md", path, sha("v1")); err != nil || !linked {
		t.Fatalf("linked %v, %v", linked, err)
	}
	seen := make(chan linkproto.WriteBackRequest, 10)
	watchLayout(t, l, laptopTaking(seen))
	expectNone(t, seen)
	want := linkproto.WriteBackRequest{Session: pasteSession, Origin: "/l/Downloads/report.md", BoxPath: path}
	for _, c := range []string{"v2", "v3"} {
		if err := os.WriteFile(path+".tmp", []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			t.Fatal(err)
		}
		expectWriteBack(t, seen, want)
	}

	// Deleted and built again within the grace, it is the same file.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte("rebuilt"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectWriteBack(t, seen, want)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		copies, err := sessionCopies(l)
		if err != nil {
			t.Fatal(err)
		}
		if len(copies) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a gone box file is still followed: %+v", copies)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(path, []byte("v4"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectNone(t, seen)
}
