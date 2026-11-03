package resume

import (
	"testing"
)

const (
	idA = "aaaaaaaa-1111-4222-8333-444444444444"
	idB = "bbbbbbbb-1111-4222-8333-444444444444"
)

func rclaude(id string) string { return "rclaude --resume " + id }

func sessionStart(id string) []byte {
	return []byte(`{"session_id":"` + id + `","hook_event_name":"SessionStart","source":"startup"}`)
}

func TestResumedSessionNeedsNoConfirmation(t *testing.T) {
	if got := NewTracker(idA, true, rclaude).Hint(); got != "Resume on the box with: rclaude --resume "+idA {
		t.Fatalf("Hint = %q", got)
	}
}

// A new session may never have started, for example when the box could not launch it.
func TestNewSessionNeedsSessionStart(t *testing.T) {
	tr := NewTracker(idA, false, rclaude)
	if got := tr.Hint(); got != "" {
		t.Fatalf("unconfirmed Hint = %q, want none", got)
	}
	if err := tr.ObserveHook(sessionStart(idA)); err != nil {
		t.Fatal(err)
	}
	if got := tr.Hint(); got != "Resume on the box with: rclaude --resume "+idA {
		t.Fatalf("Hint = %q", got)
	}
}

func TestUnknownIDPrintsNothing(t *testing.T) {
	if got := NewTracker("", false, rclaude).Hint(); got != "" {
		t.Fatalf("Hint = %q, want none", got)
	}
}

// After /clear, async hooks of the old session (auto-name, feed) can finish
// after the new session's SessionStart, so only SessionStart moves the id.
func TestOnlySessionStartMovesTheID(t *testing.T) {
	tr := NewTracker("", false, rclaude)
	for _, payload := range [][]byte{
		sessionStart(idA),
		sessionStart(idB),
		[]byte(`{"session_id":"` + idA + `","hook_event_name":"Stop"}`),
	} {
		if err := tr.ObserveHook(payload); err != nil {
			t.Fatal(err)
		}
	}
	if got := tr.Hint(); got != "Resume on the box with: rclaude --resume "+idB {
		t.Fatalf("Hint = %q, want the latest SessionStart id", got)
	}
}

func TestMalformedPayloadsKeepTheID(t *testing.T) {
	tr := NewTracker(idA, true, rclaude)
	for _, payload := range []string{
		`not json`,
		`{"session_id":"` + idB + `"}`,
		`{"hook_event_name":"SessionStart"}`,
		`{"session_id":"","hook_event_name":"SessionStart"}`,
		`{"session_id":"x; rm -rf ~","hook_event_name":"SessionStart"}`,
		`{"session_id":7,"hook_event_name":"SessionStart"}`,
	} {
		if err := tr.ObserveHook([]byte(payload)); err == nil {
			t.Fatalf("payload %s accepted", payload)
		}
	}
	if got := tr.Hint(); got != "Resume on the box with: rclaude --resume "+idA {
		t.Fatalf("Hint = %q", got)
	}
}

// The agent's own resume command line follows the entry, as Codex's resume subcommand does.
func TestHintUsesTheAgentsResumeCommand(t *testing.T) {
	rcodex := func(id string) string { return "rcodex resume " + id }
	if got := NewTracker(idA, true, rcodex).Hint(); got != "Resume on the box with: rcodex resume "+idA {
		t.Fatalf("Hint = %q", got)
	}
}
