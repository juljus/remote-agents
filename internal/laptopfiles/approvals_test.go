package laptopfiles

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// answers approves with its answers in turn, nil for yes, recording each
// reason; past its last answer it says yes.
type answers struct {
	mu      sync.Mutex
	next    []error
	reasons []string
}

func (a *answers) Approve(_ context.Context, reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reasons = append(a.reasons, reason)
	if len(a.next) == 0 {
		return nil
	}
	err := a.next[0]
	a.next = a.next[1:]
	return err
}

func (a *answers) asked() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.reasons...)
}

// prompt is one prompt on a held approver's screen, waiting for answer.
type prompt struct {
	reason string
	answer chan<- error
}

// held hands each prompt to the test, which answers it.
type held struct{ prompts chan prompt }

func (h held) Approve(ctx context.Context, reason string) error {
	answer := make(chan error, 1)
	h.prompts <- prompt{reason, answer}
	select {
	case err := <-answer:
		return err
	case <-ctx.Done():
		return errors.New("dismissed")
	}
}

func (g *Gate) waitingNow() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waiting
}

const otherSession = "codex-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func TestAnApprovalHoldsForItsSessionPurposeAndPathOnly(t *testing.T) {
	a := &answers{}
	g := NewGate(a, time.Minute)
	ctx := context.Background()
	for _, reason := range []string{"first", "second"} {
		if err := g.ask(ctx, grant{session: session, use: writeBackUse, path: "/u/.zshrc"}, reason); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.asked(); len(got) != 1 || got[0] != "first" {
		t.Fatalf("asked %q, want once", got)
	}
	if !g.approved(grant{session: session, use: writeBackUse, path: "/u/.zshrc"}) {
		t.Fatal("the approval is not remembered")
	}
	for name, other := range map[string]grant{
		"another session": {session: otherSession, use: writeBackUse, path: "/u/.zshrc"},
		"a save":          {session: session, use: saveUse, path: "/u/.zshrc"},
		"another path":    {session: session, use: writeBackUse, path: "/u/.bashrc"},
		"other terms":     {session: session, use: writeBackUse, path: "/u/.zshrc", terms: "hidden, executable"},
	} {
		if g.approved(other) {
			t.Errorf("%s: approved without asking", name)
		}
	}
}

func TestADeclinedPromptIsARefusalAndAsksAgainNextTime(t *testing.T) {
	a := &answers{next: []error{errors.New("Canceled by user.")}}
	g := NewGate(a, time.Minute)
	err := g.ask(context.Background(), grant{session: session, use: saveUse, path: "/u/x"}, "r")
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "Canceled by user.") {
		t.Fatalf("err = %v, want a refusal saying why", err)
	}
	if g.approved(grant{session: session, use: saveUse, path: "/u/x"}) {
		t.Fatal("a declined prompt was remembered as approved")
	}
	if err := g.ask(context.Background(), grant{session: session, use: saveUse, path: "/u/x"}, "r"); err != nil {
		t.Fatal(err)
	}
	if got := a.asked(); len(got) != 2 {
		t.Fatalf("asked %q, want twice", got)
	}
}

func TestAPromptNobodyAnswersIsRefusedAfterTheTimeout(t *testing.T) {
	h := held{prompts: make(chan prompt, 1)}
	g := NewGate(h, 20*time.Millisecond)
	err := g.ask(context.Background(), grant{session: session, use: saveUse, path: "/u/x"}, "r")
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "within") {
		t.Fatalf("err = %v, want a refusal for no answer", err)
	}
}

// The box giving up, or the link dropping, is no answer of the user's: the
// box may ask again.
func TestAPromptWhoseRequestEndsIsNoRefusal(t *testing.T) {
	h := held{prompts: make(chan prompt)}
	g := NewGate(h, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.ask(ctx, grant{session: session, use: saveUse, path: "/u/x"}, "r") }()
	<-h.prompts
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want the request's end", err)
	}
}

func TestPromptsAreShownOneAtATime(t *testing.T) {
	h := held{prompts: make(chan prompt)}
	g := NewGate(h, time.Minute)
	ctx := context.Background()
	done := make(chan error, 3)
	go func() { done <- g.ask(ctx, grant{session: session, use: saveUse, path: "/u/a"}, "a") }()
	first := <-h.prompts
	go func() { done <- g.ask(ctx, grant{session: session, use: saveUse, path: "/u/b"}, "b") }()
	// A second request for the shown path waits for its answer instead of asking again.
	go func() { done <- g.ask(ctx, grant{session: session, use: saveUse, path: "/u/a"}, "a again") }()
	select {
	case p := <-h.prompts:
		t.Fatalf("%q shown while another prompt was up", p.reason)
	case <-time.After(50 * time.Millisecond):
	}
	first.answer <- nil
	second := <-h.prompts
	if second.reason != "b" {
		t.Fatalf("then shown %q, want b", second.reason)
	}
	second.answer <- nil
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// A request that waits its turn and ends meanwhile leaves the queue.
func TestARequestEndingWhileQueuedLeavesTheQueue(t *testing.T) {
	h := held{prompts: make(chan prompt)}
	g := NewGate(h, time.Minute)
	go func() { _ = g.ask(context.Background(), grant{session: session, use: saveUse, path: "/u/a"}, "a") }()
	first := <-h.prompts
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.ask(ctx, grant{session: session, use: saveUse, path: "/u/b"}, "b") }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	first.answer <- nil
}

func TestAnApproverThatCannotAskRefuses(t *testing.T) {
	a := &answers{next: []error{errors.New("No identities are enrolled.")}}
	g := NewGate(a, time.Minute)
	if err := g.ask(context.Background(), grant{session: session, use: writeBackUse, path: "/u/.zshrc"}, "r"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// A box that sends request after request must not fill the link with
// prompts nobody answers: past maxWaiting, an ask fails, and the box may try
// again later.
func TestTooManyWaitingPromptsFailWithoutRefusing(t *testing.T) {
	h := held{prompts: make(chan prompt)}
	g := NewGate(h, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, maxWaiting)
	go func() { done <- g.ask(ctx, grant{session: session, use: saveUse, path: "/u/0"}, "0") }()
	first := <-h.prompts
	for i := 1; i < maxWaiting; i++ {
		go func() { done <- g.ask(ctx, grant{session: session, use: saveUse, path: fmt.Sprintf("/u/%d", i)}, "r") }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for g.waitingNow() < maxWaiting {
		if time.Now().After(deadline) {
			t.Fatalf("%d waiting, want %d", g.waitingNow(), maxWaiting)
		}
		time.Sleep(time.Millisecond)
	}
	err := g.ask(ctx, grant{session: session, use: saveUse, path: "/u/over"}, "over")
	if err == nil || errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "waiting") {
		t.Fatalf("err = %v, want a failure that is no refusal", err)
	}
	first.answer <- nil
	cancel()
	for range maxWaiting {
		<-done
	}
}

// A request that waited its turn for as long as a prompt may stay up was
// never shown: it fails without refusing, and the box asks again later.
func TestARequestThatNeverGotItsTurnFailsWithoutRefusing(t *testing.T) {
	release := make(chan struct{})
	shown := make(chan string, 2)
	// The first prompt overstays its timeout, as one stuck in the system might.
	g := NewGate(ApproverFunc(func(_ context.Context, reason string) error {
		shown <- reason
		if reason == "a" {
			<-release
		}
		return nil
	}), 30*time.Millisecond)
	first := make(chan error, 1)
	go func() { first <- g.ask(context.Background(), grant{session: session, use: saveUse, path: "/u/a"}, "a") }()
	<-shown
	err := g.ask(context.Background(), grant{session: session, use: saveUse, path: "/u/b"}, "b")
	if err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want a failure that is no refusal", err)
	}
	close(release)
	<-first
	if len(shown) != 0 {
		t.Fatalf("shown %q while another prompt was up", <-shown)
	}
}

// Once the user says no, no prompt shows for DeclineCoolDown, whatever its
// session or path: the box picks both, and one touch must not end a string
// of fresh prompts. What is approved already needs no prompt and goes on.
func TestADeclinePausesEveryPromptForAWhile(t *testing.T) {
	a := &answers{}
	g := NewGate(a, time.Minute)
	now := time.Now()
	g.now = func() time.Time { return now }
	kept := grant{session: otherSession, use: writeBackUse, path: "/u/.zshrc", terms: "hidden"}
	if err := g.ask(context.Background(), kept, "kept"); err != nil {
		t.Fatal(err)
	}
	a.next = []error{fmt.Errorf("Canceled by user.: %w", ErrDeclined)}
	x := grant{session: session, use: saveUse, path: "/u/x"}
	if err := g.ask(context.Background(), x, "x"); !errors.Is(err, ErrRefused) {
		t.Fatalf("declined: %v", err)
	}
	now = now.Add(DeclineCoolDown - 30*time.Second)
	for _, again := range []grant{x, {session: session, use: saveUse, path: "/u/x (1)"}, {session: otherSession, use: writeBackUse, path: "/u/y"}} {
		err := g.ask(context.Background(), again, "again")
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "paused for 30s more") {
			t.Fatalf("%+v asked during the pause: %v", again, err)
		}
	}
	if err := g.ask(context.Background(), kept, "kept"); err != nil {
		t.Fatalf("an approved write stopped during the pause: %v", err)
	}
	if got := a.asked(); len(got) != 2 {
		t.Fatalf("asked %q, want only the two before the pause", got)
	}
	now = now.Add(30 * time.Second)
	if err := g.ask(context.Background(), x, "later"); err != nil {
		t.Fatalf("after the pause: %v", err)
	}
	if got := a.asked(); len(got) != 3 || got[2] != "later" {
		t.Fatalf("asked %q", got)
	}
}

// Only the user saying no starts a cool-down: a prompt nobody answered, or
// one that could not be shown, asks again at once.
func TestOnlyADeclineStartsTheCoolDown(t *testing.T) {
	h := held{prompts: make(chan prompt, 1)}
	g := NewGate(h, 20*time.Millisecond)
	x := grant{session: session, use: saveUse, path: "/u/x"}
	if err := g.ask(context.Background(), x, "x"); !errors.Is(err, ErrRefused) {
		t.Fatalf("unanswered: %v", err)
	}
	<-h.prompts
	a := &answers{next: []error{errors.New("No identities are enrolled.")}}
	g.approver = a
	if err := g.ask(context.Background(), x, "x"); !errors.Is(err, ErrRefused) {
		t.Fatalf("failed: %v", err)
	}
	if err := g.ask(context.Background(), x, "x"); err != nil {
		t.Fatalf("asked after a failure: %v", err)
	}
	if got := a.asked(); len(got) != 2 {
		t.Fatalf("asked %q, want twice", got)
	}
}

// Requests queued behind the prompt the user declined are refused unasked,
// another path's too.
func TestRequestsQueuedBehindADeclineAreRefused(t *testing.T) {
	h := held{prompts: make(chan prompt)}
	g := NewGate(h, time.Minute)
	x := grant{session: session, use: saveUse, path: "/u/x"}
	first := make(chan error, 1)
	go func() { first <- g.ask(context.Background(), x, "x") }()
	p := <-h.prompts
	second := make(chan error, 1)
	go func() {
		second <- g.ask(context.Background(), grant{session: otherSession, use: saveUse, path: "/u/y"}, "y")
	}()
	for g.waitingNow() < 2 {
		time.Sleep(time.Millisecond)
	}
	p.answer <- fmt.Errorf("Canceled by user.: %w", ErrDeclined)
	<-first
	select {
	case err := <-second:
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "paused") {
			t.Fatalf("queued request: %v", err)
		}
	case p := <-h.prompts:
		t.Fatalf("shown %q right after a decline", p.reason)
	}
}
