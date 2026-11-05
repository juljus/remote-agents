package laptopfiles

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Approver asks the user at the laptop whether the box may do what reason
// says, a phrase that follows "remote-agents is trying to"; nil means yes.
// It gives up when ctx ends.
type Approver interface {
	Approve(ctx context.Context, reason string) error
}

// ApproverFunc is a function that approves.
type ApproverFunc func(ctx context.Context, reason string) error

func (f ApproverFunc) Approve(ctx context.Context, reason string) error { return f(ctx, reason) }

// purpose is what a session got approved to do to a path.
type purpose int

const (
	writeBackUse purpose = iota
	saveUse
)

type grant struct {
	session string
	use     purpose
	path    string
}

// Gate asks the user, one prompt at a time, before the box writes where it
// may not on its own, and remembers what each session got approved. The
// approvals live in the daemon's memory only, so a restart asks again: a
// grant kept on disk would be one a file written on the laptop could forge.
type Gate struct {
	approver Approver
	timeout  time.Duration
	// turn holds a token while a prompt is up.
	turn chan struct{}

	mu      sync.Mutex
	granted map[grant]bool
}

// NewGate asks approver, giving each prompt timeout for the user's answer.
func NewGate(approver Approver, timeout time.Duration) *Gate {
	return &Gate{approver: approver, timeout: timeout, turn: make(chan struct{}, 1), granted: map[grant]bool{}}
}

// approved reports whether session got approved to put use to path.
func (g *Gate) approved(session string, use purpose, path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.granted[grant{session, use, path}]
}

// ask makes sure session may put use to path, asking the user with reason
// unless it got approved already. A prompt declined, failed or unanswered is
// a refusal; ctx ending first is not, as the user gave no answer.
func (g *Gate) ask(ctx context.Context, session string, use purpose, path, reason string) error {
	if g.approved(session, use, path) {
		return nil
	}
	select {
	case g.turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-g.turn }()
	// The prompt before this one may have been for the same path.
	if g.approved(session, use, path) {
		return nil
	}
	promptCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	err := g.approver.Approve(promptCtx, reason)
	switch {
	case err == nil:
		g.mu.Lock()
		defer g.mu.Unlock()
		g.granted[grant{session, use, path}] = true
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(promptCtx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%w: nobody approved %s within %s; the box keeps its version", ErrRefused, path, g.timeout)
	}
	return fmt.Errorf("%w: %s not approved: %v; the box keeps its version", ErrRefused, path, err)
}
