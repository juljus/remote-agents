package box

import (
	"fmt"
	"os"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/jsonfile"
	"remote-agents/internal/spec"
)

// attachment is the latest connection attempt that claimed a session. The
// laptop retries a dead link with a new attempt, but the old attempt's
// command can still reach the box later, delayed on the network; it must
// not take the session from the attempt that replaced it.
type attachment struct {
	RunID   string `json:"run_id"`
	Attempt int    `json:"attempt"`
}

// claimAttachment records s as the session's latest attachment and reports
// true, or reports false without recording anything when a newer attempt of
// the same run already claimed the session.
func claimAttachment(l boxlayout.Layout, s spec.Spec) (bool, error) {
	claimed := false
	err := withAttachments(l, func() error {
		cur, found, err := readAttachment(l, s.Session)
		if err != nil {
			return err
		}
		if found && s.Attempt > 0 && cur.RunID == s.RunID && cur.Attempt > s.Attempt {
			return nil
		}
		claimed = true
		return jsonfile.Write(l.Attachment(s.Session), attachment{RunID: s.RunID, Attempt: s.Attempt})
	})
	return claimed, err
}

// ownsAttachment reports whether the numbered attempt s is still the
// session's latest claim.
func ownsAttachment(l boxlayout.Layout, s spec.Spec) (bool, error) {
	if s.Attempt == 0 {
		return false, nil
	}
	owns := false
	err := withAttachments(l, func() error {
		cur, found, err := readAttachment(l, s.Session)
		owns = found && cur == attachment{RunID: s.RunID, Attempt: s.Attempt}
		return err
	})
	return owns, err
}

func withAttachments(l boxlayout.Layout, fn func() error) error {
	if err := os.MkdirAll(l.AttachmentsDir(), 0o700); err != nil {
		return fmt.Errorf("creating attachments dir: %w", err)
	}
	return withFlock(l.AttachmentsLock(), fn)
}

func readAttachment(l boxlayout.Layout, session string) (attachment, bool, error) {
	var a attachment
	found, err := jsonfile.Read(l.Attachment(session), &a)
	return a, found, err
}
