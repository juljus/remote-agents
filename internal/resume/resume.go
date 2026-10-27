// Package resume follows which Claude session a wrapper runs, so that after
// the agent exits it can say how to resume that session on the box. Claude's
// own hint names its local command, which would look for the session locally.
package resume

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"remote-agents/internal/claudeargs"
)

type Tracker struct {
	mu sync.Mutex
	id string
	// confirmed: the session exists, so resuming it cannot fail for want of it.
	confirmed bool
}

// NewTracker starts from the id the command line fixes, or "" when Claude
// picks it; resumes tells that the id names an existing session.
func NewTracker(id string, resumes bool) *Tracker {
	return &Tracker{id: id, confirmed: resumes}
}

// ObserveHook takes the session id from a SessionStart payload, which Claude
// sends at startup, resume, /clear and compaction. Other hooks are ignored:
// async ones of an earlier session can arrive after a newer SessionStart.
func (t *Tracker) ObserveHook(payload []byte) error {
	var p struct {
		SessionID *string `json:"session_id"`
		Event     *string `json:"hook_event_name"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("reading hook payload: %w", err)
	}
	if p.Event == nil {
		return errors.New("hook payload has no hook_event_name")
	}
	if *p.Event != "SessionStart" {
		return nil
	}
	if p.SessionID == nil {
		return errors.New("SessionStart payload has no session_id")
	}
	if !claudeargs.IsUUID(*p.SessionID) {
		return fmt.Errorf("SessionStart payload session_id %q is not a session id", *p.SessionID)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.id, t.confirmed = *p.SessionID, true
	return nil
}

// Hint is the line to print after the agent exits; "" unless the session is
// known to exist, since a wrong id would resume another session or none.
func (t *Tracker) Hint(entry string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.confirmed {
		return ""
	}
	return "Resume on the box with: " + entry + " --resume " + t.id
}
