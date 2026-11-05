package laptopfiles

import (
	"context"
	"errors"
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

const otherSession = "codex-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func TestAnApprovalHoldsForItsSessionPurposeAndPathOnly(t *testing.T) {
	a := &answers{}
	g := NewGate(a, time.Minute)
	ctx := context.Background()
	for _, reason := range []string{"first", "second"} {
		if err := g.ask(ctx, session, writeBackUse, "/u/.zshrc", reason); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.asked(); len(got) != 1 || got[0] != "first" {
		t.Fatalf("asked %q, want once", got)
	}
	if !g.approved(session, writeBackUse, "/u/.zshrc") {
		t.Fatal("the approval is not remembered")
	}
	for name, tc := range map[string]struct {
		session string
		use     purpose
		path    string
	}{
		"another session": {otherSession, writeBackUse, "/u/.zshrc"},
		"a save":          {session, saveUse, "/u/.zshrc"},
		"another path":    {session, writeBackUse, "/u/.bashrc"},
	} {
		if g.approved(tc.session, tc.use, tc.path) {
			t.Errorf("%s: approved without asking", name)
		}
	}
}

func TestADeclinedPromptIsARefusalAndAsksAgainNextTime(t *testing.T) {
	a := &answers{next: []error{errors.New("Canceled by user.")}}
	g := NewGate(a, time.Minute)
	err := g.ask(context.Background(), session, saveUse, "/u/x", "r")
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "Canceled by user.") {
		t.Fatalf("err = %v, want a refusal saying why", err)
	}
	if g.approved(session, saveUse, "/u/x") {
		t.Fatal("a declined prompt was remembered as approved")
	}
	if err := g.ask(context.Background(), session, saveUse, "/u/x", "r"); err != nil {
		t.Fatal(err)
	}
	if got := a.asked(); len(got) != 2 {
		t.Fatalf("asked %q, want twice", got)
	}
}

func TestAPromptNobodyAnswersIsRefusedAfterTheTimeout(t *testing.T) {
	h := held{prompts: make(chan prompt, 1)}
	g := NewGate(h, 20*time.Millisecond)
	err := g.ask(context.Background(), session, saveUse, "/u/x", "r")
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
	go func() { done <- g.ask(ctx, session, saveUse, "/u/x", "r") }()
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
	go func() { done <- g.ask(ctx, session, saveUse, "/u/a", "a") }()
	first := <-h.prompts
	go func() { done <- g.ask(ctx, session, saveUse, "/u/b", "b") }()
	// A second request for the shown path waits for its answer instead of asking again.
	go func() { done <- g.ask(ctx, session, saveUse, "/u/a", "a again") }()
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
	go func() { _ = g.ask(context.Background(), session, saveUse, "/u/a", "a") }()
	first := <-h.prompts
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.ask(ctx, session, saveUse, "/u/b", "b") }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	first.answer <- nil
}

func TestAnApproverThatCannotAskRefuses(t *testing.T) {
	a := &answers{next: []error{errors.New("No identities are enrolled.")}}
	g := NewGate(a, time.Minute)
	if err := g.ask(context.Background(), session, writeBackUse, "/u/.zshrc", "r"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
}
