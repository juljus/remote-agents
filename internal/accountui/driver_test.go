package accountui

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"remote-agents/internal/accounts"
)

// fakeBox is a box whose accounts the test sets, and that records each call.
type fakeBox struct {
	listing accounts.Listing
	calls   []string
	// fail is the error of the next call it names, by verb.
	fail map[string]error
	// onStart is what a request to an account does to its listing.
	onStart func(l *accounts.Listing)
}

func (b *fakeBox) List() (accounts.Listing, error) {
	b.calls = append(b.calls, "list")
	if err := b.fail["list"]; err != nil {
		return accounts.Listing{}, err
	}
	return accounts.Listing{Accounts: slices.Clone(b.listing.Accounts)}, nil
}

func (b *fakeBox) Do(args ...string) (string, error) {
	b.calls = append(b.calls, strings.Join(args, " "))
	if err := b.fail[args[0]]; err != nil {
		return "", err
	}
	switch args[0] {
	case "use":
		return "switched the box to " + args[1] + "\n", nil
	case "rename":
		i := slices.IndexFunc(b.listing.Accounts, func(r accounts.Row) bool { return r.Name == args[1] })
		b.listing.Accounts[i].Name = args[2]
		return "renamed " + args[1] + " to " + args[2] + "\n", nil
	case "remove":
		b.listing.Accounts = slices.DeleteFunc(b.listing.Accounts, func(r accounts.Row) bool { return r.Name == args[1] })
		return "removed " + args[1] + "\n", nil
	}
	return "", errors.New("unexpected " + args[0])
}

func (b *fakeBox) Add(name string) error {
	b.calls = append(b.calls, "add "+name)
	if err := b.fail["add"]; err != nil {
		return err
	}
	b.listing.Accounts = append(b.listing.Accounts, accounts.Row{Name: name, Email: name + "@example.com"})
	return nil
}

func (b *fakeBox) Start(name string) error {
	b.calls = append(b.calls, "start "+name)
	if err := b.fail["start"]; err != nil {
		return err
	}
	b.onStart(&b.listing)
	return nil
}

// keys is terminal input, a key per burst.
func keys(ks ...string) *bursts {
	src := &bursts{}
	for _, k := range ks {
		src.chunks = append(src.chunks, []byte(k))
	}
	return src
}

type harness struct {
	box       *fakeBox
	out       bytes.Buffer
	suspended int
}

func newHarness() *harness {
	return &harness{box: &fakeBox{listing: listing(), fail: map[string]error{}}}
}

func (h *harness) run(t *testing.T, src *bursts) (string, error) {
	t.Helper()
	p := picker{
		src:     src,
		scr:     &screen{out: &h.out, width: func() (int, error) { return 200, nil }, now: func() time.Time { return now }},
		backend: h.box,
		suspend: func(fn func() error) error { h.suspended++; return fn() },
	}
	return p.run()
}

// frames are the pictures the picker drew, each as its lines.
func (h *harness) frames() [][]string {
	var frames [][]string
	for _, f := range strings.Split(h.out.String(), "\x1b[J")[1:] {
		frames = append(frames, strings.Split(f, "\r\n"))
	}
	return frames
}

func TestRunSwitchesAndLeavesWhatTheBoxSaid(t *testing.T) {
	h := newHarness()
	said, err := h.run(t, keys("\x1b[A", "\r"))
	if err != nil {
		t.Fatal(err)
	}
	if said != "switched the box to invoices\n" {
		t.Fatalf("said %q", said)
	}
	if !slices.Equal(h.box.calls, []string{"list", "use invoices"}) {
		t.Fatalf("calls %q", h.box.calls)
	}
	// The picker erases itself: it goes up to its first line and clears down.
	if !strings.HasSuffix(h.out.String(), "\r\x1b[5A\x1b[J") {
		t.Fatalf("picker left on screen: %q", h.out.String())
	}
}

func TestRunLeavesWithNothingChanged(t *testing.T) {
	h := newHarness()
	said, err := h.run(t, keys("j", "q"))
	if err != nil || said != "" {
		t.Fatalf("q = %q, %v", said, err)
	}
	if !slices.Equal(h.box.calls, []string{"list"}) {
		t.Fatalf("calls %q", h.box.calls)
	}
}

func TestRunShowsARefusedSwitchOnTheStatusLine(t *testing.T) {
	h := newHarness()
	h.box.fail["use"] = errors.New("switching while juljus's token is about to expire risks its refresh")
	if _, err := h.run(t, keys("k", "\r", "q")); err != nil {
		t.Fatal(err)
	}
	frames := h.frames()
	if got := frames[len(frames)-2][4]; got != "switching while juljus's token is about to expire risks its refresh" {
		t.Fatalf("status %q", got)
	}
}

func TestRunRenamesAndFollowsTheAccount(t *testing.T) {
	h := newHarness()
	src := keys(append(append([]string{"k", "e"}, strings.Split(strings.Repeat("\x7f", 8), "")...), "b", "i", "l", "l", "s", "\r", "q")...)
	if _, err := h.run(t, src); err != nil {
		t.Fatal(err)
	}
	frames := h.frames()
	last := frames[len(frames)-2]
	if !strings.HasPrefix(last[0], "▸ bills") || last[4] != "renamed invoices to bills" {
		t.Fatalf("after rename:\n%s", strings.Join(last, "\n"))
	}
}

func TestRunRemovesAfterYes(t *testing.T) {
	h := newHarness()
	if _, err := h.run(t, keys("k", "d", "y", "q")); err != nil {
		t.Fatal(err)
	}
	frames := h.frames()
	last := frames[len(frames)-2]
	if len(last) != 5 || last[3] != "removed invoices" {
		t.Fatalf("after remove:\n%s", strings.Join(last, "\n"))
	}
}

func TestRunAddsWithTheTerminalHandedBack(t *testing.T) {
	h := newHarness()
	if _, err := h.run(t, keys("a", "w", "o", "r", "k", "\r", "q")); err != nil {
		t.Fatal(err)
	}
	if h.suspended != 1 {
		t.Fatalf("suspended %d times", h.suspended)
	}
	frames := h.frames()
	last := frames[len(frames)-2]
	if !strings.HasPrefix(last[3], "▸ work") || last[5] != "added work" {
		t.Fatalf("after add:\n%s", strings.Join(last, "\n"))
	}
}

func TestRunShowsAFailedAddOnTheStatusLine(t *testing.T) {
	h := newHarness()
	h.box.fail["add"] = errors.New("signing in failed (exit 1)")
	if _, err := h.run(t, keys("a", "w", "\r", "q")); err != nil {
		t.Fatal(err)
	}
	if got := h.frames()[len(h.frames())-2][4]; got != "signing in failed (exit 1)" {
		t.Fatalf("status %q", got)
	}
}

func TestRunStartsAWeekAndSaysWhenItResets(t *testing.T) {
	h := newHarness()
	h.box.onStart = func(l *accounts.Listing) {
		l.Accounts[2].Weekly = &accounts.Window{Utilization: 0, ResetsAt: &resets}
	}
	if _, err := h.run(t, keys("j", "s", "q")); err != nil {
		t.Fatal(err)
	}
	frames := h.frames()
	if got := frames[len(frames)-3][4]; got != "sending molton a request to start its week…" {
		t.Fatalf("progress %q", got)
	}
	if got := frames[len(frames)-2][4]; got != "started molton's week; it resets Thu 18:00" {
		t.Fatalf("result %q", got)
	}
}

func TestRunSaysPlainlyWhenTheWeekStillHasNotStarted(t *testing.T) {
	h := newHarness()
	h.box.onStart = func(*accounts.Listing) {}
	if _, err := h.run(t, keys("j", "s", "q")); err != nil {
		t.Fatal(err)
	}
	if got := h.frames()[len(h.frames())-2][4]; got != "sent molton a request, but its week still has not started" {
		t.Fatalf("result %q", got)
	}
}

func TestRunFailsWhenTheFirstListingFails(t *testing.T) {
	h := newHarness()
	h.box.fail["list"] = errors.New("link to box failed")
	if _, err := h.run(t, keys("q")); err == nil || h.out.Len() != 0 {
		t.Fatalf("err %v, drew %q", err, h.out.String())
	}
}
