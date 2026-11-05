package laptopfiles

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Approver asks the user at the laptop whether the box may do what reason
// says, a phrase that follows "remote-agents is trying to"; nil means yes,
// and an error wrapping ErrDeclined that the user said no. It gives up when
// ctx ends.
type Approver interface {
	Approve(ctx context.Context, reason string) error
}

// ErrDeclined marks an Approver's answer as the user's no, rather than a
// prompt that could not be shown.
var ErrDeclined = errors.New("declined")

// DeclineCoolDown is how long a session's path, once the user declined it,
// is refused unasked, so that asking again for it at once gets no new
// prompt. Other paths and sessions, both of which the box picks, still ask.
const DeclineCoolDown = 5 * time.Minute

// ApproverFunc is a function that approves.
type ApproverFunc func(ctx context.Context, reason string) error

func (f ApproverFunc) Approve(ctx context.Context, reason string) error { return f(ctx, reason) }

// purpose is what a session got approved to do to a path.
type purpose int

const (
	writeBackUse purpose = iota
	saveUse
)

// grant is what a session gets approved: a use of a path, on the terms the
// prompt named beyond it, such as the risks and whether a save replaces a
// file. Other terms ask again.
type grant struct {
	session string
	use     purpose
	path    string
	terms   string
}

// maxWaiting bounds the prompts shown or waiting their turn, so that a box
// sending request after request cannot hold every slot of the link.
const maxWaiting = 8

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
	waiting int
	// declined is when the user last declined each session's path.
	declined map[declinedPath]time.Time
	now      func() time.Time
}

type declinedPath struct{ session, path string }

// NewGate asks approver, giving each prompt timeout for the user's answer
// and each request as long to wait its turn.
func NewGate(approver Approver, timeout time.Duration) *Gate {
	return &Gate{approver: approver, timeout: timeout, turn: make(chan struct{}, 1), granted: map[grant]bool{}, declined: map[declinedPath]time.Time{}, now: time.Now}
}

func (g *Gate) approved(gr grant) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.granted[gr]
}

// ask makes sure gr is granted, asking the user with reason unless it is
// already. A prompt declined, failed or unanswered is a refusal. A request
// that ends first, or never gets its turn, is not, as the user gave no answer.
func (g *Gate) ask(ctx context.Context, gr grant, reason string) error {
	g.mu.Lock()
	if g.granted[gr] {
		g.mu.Unlock()
		return nil
	}
	if err := g.coolingDown(gr); err != nil {
		g.mu.Unlock()
		return err
	}
	if g.waiting == maxWaiting {
		g.mu.Unlock()
		return fmt.Errorf("%d prompts are waiting for you already; asking about %s later", maxWaiting, gr.path)
	}
	g.waiting++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.waiting--
	}()
	queued := time.NewTimer(g.timeout)
	defer queued.Stop()
	select {
	case g.turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-queued.C:
		return fmt.Errorf("another prompt held the screen for %s; asking about %s later", g.timeout, gr.path)
	}
	defer func() { <-g.turn }()
	// The prompt before this one may have granted it, or the user declined it.
	g.mu.Lock()
	granted, cooling := g.granted[gr], g.coolingDown(gr)
	g.mu.Unlock()
	if granted || cooling != nil {
		return cooling
	}
	promptCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	err := g.approver.Approve(promptCtx, reason)
	switch {
	case err == nil:
		g.mu.Lock()
		defer g.mu.Unlock()
		g.granted[gr] = true
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(promptCtx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%w: nobody approved %s within %s; the box keeps its version", ErrRefused, gr.path, g.timeout)
	case errors.Is(err, ErrDeclined):
		g.mu.Lock()
		defer g.mu.Unlock()
		g.declined[declinedPath{gr.session, gr.path}] = g.now()
	}
	return fmt.Errorf("%w: %s not approved: %v; the box keeps its version", ErrRefused, gr.path, err)
}

// coolingDown refuses gr while the user's decline of its session and path is
// recent. The caller holds g.mu.
func (g *Gate) coolingDown(gr grant) error {
	at, ok := g.declined[declinedPath{gr.session, gr.path}]
	if !ok {
		return nil
	}
	if left := DeclineCoolDown - g.now().Sub(at); left > 0 {
		return fmt.Errorf("%w: you declined %s for this session %s ago; the box keeps its version, and may offer it again in %s", ErrRefused, gr.path, g.now().Sub(at).Round(time.Second), left.Round(time.Second))
	}
	delete(g.declined, declinedPath{gr.session, gr.path})
	return nil
}
