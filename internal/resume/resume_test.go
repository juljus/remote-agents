package resume

import (
	"testing"
)

const (
	idA = "aaaaaaaa-1111-4222-8333-444444444444"
	idB = "bbbbbbbb-1111-4222-8333-444444444444"
)

func sessionStart(id string) []byte {
	return []byte(`{"session_id":"` + id + `","hook_event_name":"SessionStart","source":"startup"}`)
}

func TestResumedSessionNeedsNoConfirmation(t *testing.T) {
	if got := NewTracker(idA, true).Hint("rclaude"); got != "Resume on the box with: rclaude --resume "+idA {
		t.Fatalf("Hint = %q", got)
	}
}

// A new session may never have started, for example when the box could not launch it.
func TestNewSessionNeedsSessionStart(t *testing.T) {
	tr := NewTracker(idA, false)
	if got := tr.Hint("rclaude"); got != "" {
		t.Fatalf("unconfirmed Hint = %q, want none", got)
	}
	if err := tr.ObserveHook(sessionStart(idA)); err != nil {
		t.Fatal(err)
	}
	if got := tr.Hint("rclaude"); got != "Resume on the box with: rclaude --resume "+idA {
		t.Fatalf("Hint = %q", got)
	}
}

func TestUnknownIDPrintsNothing(t *testing.T) {
	if got := NewTracker("", false).Hint("rclaude"); got != "" {
		t.Fatalf("Hint = %q, want none", got)
	}
}

// After /clear, async hooks of the old session (auto-name, feed) can finish
// after the new session's SessionStart, so only SessionStart moves the id.
func TestOnlySessionStartMovesTheID(t *testing.T) {
	tr := NewTracker("", false)
	for _, payload := range [][]byte{
		sessionStart(idA),
		sessionStart(idB),
		[]byte(`{"session_id":"` + idA + `","hook_event_name":"Stop"}`),
	} {
		if err := tr.ObserveHook(payload); err != nil {
			t.Fatal(err)
		}
	}
	if got := tr.Hint("rclaude"); got != "Resume on the box with: rclaude --resume "+idB {
		t.Fatalf("Hint = %q, want the latest SessionStart id", got)
	}
}

func TestMalformedPayloadsKeepTheID(t *testing.T) {
	tr := NewTracker(idA, true)
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
	if got := tr.Hint("rclaude"); got != "Resume on the box with: rclaude --resume "+idA {
		t.Fatalf("Hint = %q", got)
	}
}
