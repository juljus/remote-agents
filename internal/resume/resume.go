// Package resume follows which agent session a wrapper runs, so that after
// the agent exits it can say how to resume that session on the box. The
// agent's own hint names its local command, which would look for the session
// locally.
package resume

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"remote-agents/internal/agentcli"
)

type Tracker struct {
	mu sync.Mutex
	id string
	// confirmed: the session exists, so resuming it cannot fail for want of it.
	confirmed bool
	// command is the wrapper command line that resumes a session.
	command func(id string) string
}

// NewTracker starts from the id the command line fixes, or "" when the agent
// picks it; resumes tells that the id names an existing session.
func NewTracker(id string, resumes bool, command func(id string) string) *Tracker {
	return &Tracker{id: id, confirmed: resumes, command: command}
}

// ObserveHook takes the session id from a SessionStart payload, which Claude
// sends at startup, resume, /clear and compaction, and Codex at the first
// turn of each of those. Other hooks are ignored:
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
	if !agentcli.IsUUID(*p.SessionID) {
		return fmt.Errorf("SessionStart payload session_id %q is not a session id", *p.SessionID)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.id, t.confirmed = *p.SessionID, true
	return nil
}

// Hint is the line to print after the agent exits; "" unless the session is
// known to exist, since a wrong id would resume another session or none.
func (t *Tracker) Hint() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.confirmed {
		return ""
	}
	return "Resume on the box with: " + t.command(t.id)
}
