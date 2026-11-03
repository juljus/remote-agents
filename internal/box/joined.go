package box

import (
	"fmt"
	"os"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/jsonfile"
)

// A run that resumes a session another run's launcher already runs joins
// that session. The launcher records its end under its own run and under
// the session's name; the joined run looks for the latter.
type joinedRecord struct {
	Session string `json:"session"`
	At      int64  `json:"at_ms"`
}

type sessionEnd struct {
	Marker marker `json:"marker"`
	At     int64  `json:"at_ms"`
}

func recordJoined(l boxlayout.Layout, runID, session string, now time.Time) error {
	if err := os.MkdirAll(l.JoinedDir(), 0o700); err != nil {
		return fmt.Errorf("creating joined dir: %w", err)
	}
	return jsonfile.Write(l.Joined(runID), joinedRecord{Session: session, At: now.UnixMilli()})
}

func writeSessionEnd(l boxlayout.Layout, session string, m marker, now time.Time) error {
	if err := os.MkdirAll(l.SessionEndsDir(), 0o700); err != nil {
		return fmt.Errorf("creating session ends dir: %w", err)
	}
	return jsonfile.Write(l.SessionEnd(session), sessionEnd{Marker: m, At: now.UnixMilli()})
}

// joinedEnd is how the session that run runID joined ended, if it ended
// after the run joined it.
func joinedEnd(l boxlayout.Layout, runID string) (marker, bool, error) {
	var j joinedRecord
	found, err := jsonfile.Read(l.Joined(runID), &j)
	if err != nil || !found {
		return marker{}, false, err
	}
	var end sessionEnd
	found, err = jsonfile.Read(l.SessionEnd(j.Session), &end)
	if err != nil || !found || end.At < j.At {
		return marker{}, false, err
	}
	return end.Marker, true, nil
}
