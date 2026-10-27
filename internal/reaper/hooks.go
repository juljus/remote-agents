package reaper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"remote-agents/internal/jsonfile"
)

// HookEvents are the Claude Code hooks whose reports HookState follows.
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SubagentStop", "Notification"}

// waitingNotifications are the notifications Claude sends when it stops to wait for its user.
var waitingNotifications = []string{"permission_prompt", "elicitation_dialog"}

// HookState is what the agent's hooks have reported about its session.
type HookState struct {
	ClaudeSession string `json:"claude_session,omitempty"`
	Transcript    string `json:"transcript,omitempty"`
	// A turn is open while TurnStarted is after TurnEnded.
	TurnStarted int64 `json:"turn_started_ms,omitempty"`
	TurnEnded   int64 `json:"turn_ended_ms,omitempty"`
	// WaitingSince is when the open turn stopped to ask its user, as at a permission prompt.
	WaitingSince int64 `json:"waiting_since_ms,omitempty"`
	// BackgroundTasks and SessionCrons are as of the last Stop or SubagentStop.
	BackgroundTasks int `json:"background_tasks"`
	SessionCrons    int `json:"session_crons"`
}

func (h HookState) TurnOpen() bool { return h.TurnStarted > h.TurnEnded }

type task struct {
	Status string `json:"status"`
}

type hookPayload struct {
	Event        string `json:"hook_event_name"`
	SessionID    string `json:"session_id"`
	Transcript   string `json:"transcript_path"`
	Notification string `json:"notification_type"`
	// Absent from events that do not report them; Stop lists ScheduleWakeup and CronCreate jobs as session_crons.
	BackgroundTasks *[]task            `json:"background_tasks"`
	SessionCrons    *[]json.RawMessage `json:"session_crons"`
}

// finishedTask are task states after which a background task does nothing more.
var finishedTask = []string{"completed", "failed", "killed"}

// Apply folds one hook payload into h.
func (h HookState) Apply(raw []byte, now time.Time) (HookState, error) {
	var p hookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return h, fmt.Errorf("parsing hook payload: %w", err)
	}
	if !slices.Contains(HookEvents, p.Event) {
		return h, fmt.Errorf("unexpected hook event %q", p.Event)
	}
	if p.SessionID != "" {
		h.ClaudeSession = p.SessionID
	}
	if p.Transcript != "" {
		h.Transcript = p.Transcript
	}
	switch p.Event {
	case "UserPromptSubmit":
		h.TurnStarted, h.WaitingSince = now.UnixMilli(), 0
	case "Stop", "StopFailure":
		h.TurnEnded, h.WaitingSince = now.UnixMilli(), 0
	case "Notification":
		if slices.Contains(waitingNotifications, p.Notification) {
			h.WaitingSince = now.UnixMilli()
		}
	}
	if p.BackgroundTasks != nil {
		h.BackgroundTasks = 0
		for _, t := range *p.BackgroundTasks {
			if !slices.Contains(finishedTask, t.Status) {
				h.BackgroundTasks++
			}
		}
	}
	if p.SessionCrons != nil {
		h.SessionCrons = len(*p.SessionCrons)
	}
	return h, nil
}

func ReadHookState(path string) (HookState, error) {
	var h HookState
	_, err := jsonfile.Read(path, &h)
	return h, err
}

// RecordHook applies a payload to the state at path. Hooks of one session can
// run concurrently, so the update holds a lock beside the file.
func RecordHook(path string, raw []byte, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating activity dir: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening hook state lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking hook state: %w", err)
	}
	h, err := ReadHookState(path)
	if err != nil {
		return err
	}
	if h, err = h.Apply(raw, now); err != nil {
		return err
	}
	return jsonfile.Write(path, h)
}

// Interrupted reports whether the transcript tail shows the user interrupting
// a turn at or after since; Claude sends no Stop hook for an interrupted turn.
func Interrupted(tail []byte, since time.Time) bool {
	for _, line := range bytes.Split(tail, []byte("\n")) {
		var e struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		// The tail starts mid-line, and Claude writes entry types this check does not need.
		if json.Unmarshal(line, &e) != nil || e.Type != "user" || e.Timestamp.Before(since) {
			continue
		}
		if bytes.Contains(e.Message.Content, []byte(`"[Request interrupted by user`)) {
			return true
		}
	}
	return false
}
