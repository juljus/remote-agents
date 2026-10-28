package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/claudeargs"
	"remote-agents/internal/entrypoint"
	"remote-agents/internal/jsonfile"
	"remote-agents/internal/reaper"
	"remote-agents/internal/spec"
)

// RecordActivity runs as the agent's activity hook: it folds the hook's
// payload on stdin into the session's hook state. Failures also go to the
// reaper log, since Claude shows a hook's error only briefly.
func RecordActivity(l boxlayout.Layout, getenv func(string) string, stdin io.Reader, now time.Time) (err error) {
	defer func() {
		if err != nil {
			err = errors.Join(err, reaperLog(l, now, "activity hook: %v", err))
		}
	}()
	session := getenv(SessionEnvVar)
	if err := spec.ValidSessionName(session); err != nil {
		return fmt.Errorf("%s: %w", SessionEnvVar, err)
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading hook payload: %w", err)
	}
	return reaper.RecordHook(l.HookState(session), raw, now)
}

// activitySettings adds to the settings JSON base (nil for none) the hooks
// that make Claude report its turns, background tasks and scheduled wakeups
// to RecordActivity. The other settings and hooks in base stay as they are.
func activitySettings(l boxlayout.Layout, base []byte) (string, error) {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Hooks []hook `json:"hooks"`
	}
	settings := map[string]json.RawMessage{}
	hooks := map[string][]json.RawMessage{}
	if base != nil {
		if err := json.Unmarshal(base, &settings); err != nil {
			return "", fmt.Errorf("parsing --settings: %w", err)
		}
		if settings == nil {
			return "", errors.New("--settings is null, not a JSON object")
		}
		if raw, ok := settings["hooks"]; ok {
			if err := json.Unmarshal(raw, &hooks); err != nil {
				return "", fmt.Errorf("parsing --settings hooks: %w", err)
			}
		}
		if hooks == nil {
			hooks = map[string][]json.RawMessage{}
		}
	}
	ours, err := json.Marshal(matcher{Hooks: []hook{{Type: "command", Command: shellQuote(l.Binary()) + " activity"}}})
	if err != nil {
		return "", fmt.Errorf("encoding activity hooks: %w", err)
	}
	for _, event := range reaper.HookEvents {
		hooks[event] = append(hooks[event], ours)
	}
	if settings["hooks"], err = json.Marshal(hooks); err != nil {
		return "", fmt.Errorf("encoding activity hooks: %w", err)
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("encoding settings: %w", err)
	}
	return string(data), nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// withActivityHooks adds the activity hooks to an agent command line. Claude
// applies only the last --settings it is given, so the hooks join that one,
// read from its file (relative to workdir) when it is not inline JSON, and
// come as their own --settings when there is none.
func withActivityHooks(l boxlayout.Layout, argv []string, workdir string) ([]string, error) {
	at, found := claudeargs.LastSettings(argv[1:])
	if !found {
		settings, err := activitySettings(l, nil)
		if err != nil {
			return nil, err
		}
		return slices.Concat(argv[:1], []string{"--settings", settings}, argv[1:]), nil
	}
	i := at.Index + 1
	value := argv[i]
	if at.Joined {
		value = strings.TrimPrefix(value, "--settings=")
	}
	base := []byte(value)
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		path := value
		if !filepath.IsAbs(path) {
			path = filepath.Join(workdir, path)
		}
		var err error
		if base, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("reading --settings: %w", err)
		}
	}
	settings, err := activitySettings(l, base)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(argv)
	if at.Joined {
		settings = "--settings=" + settings
	}
	out[i] = settings
	return out, nil
}

// A reap is recorded twice: by Claude session, so the next run resuming it
// can say so, and by shpool session, so an attach by name can resume it and
// listings can show it.
type reapedRecord struct {
	Reason  reaper.Reason `json:"reason"`
	Session string        `json:"session"`
}

type reapedSessionRecord struct {
	Reason        reaper.Reason `json:"reason"`
	ClaudeSession string        `json:"claude_session"`
	At            int64         `json:"at_ms"`
	// Spec is the reaped run's, which an attach by name resumes.
	Spec spec.Spec `json:"spec"`
}

func writeReaped(l boxlayout.Layout, s spec.Spec, claudeSession string, r reaper.Reason, now time.Time) error {
	for _, dir := range []string{l.ReapedDir(), l.ReapedSessionsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", filepath.Base(dir), err)
		}
	}
	if err := jsonfile.Write(l.Reaped(claudeSession), reapedRecord{Reason: r, Session: s.Session}); err != nil {
		return err
	}
	return jsonfile.Write(l.ReapedSession(s.Session), reapedSessionRecord{Reason: r, ClaudeSession: claudeSession, At: now.UnixMilli(), Spec: s})
}

func readReapedSession(l boxlayout.Layout, session string) (reapedSessionRecord, bool, error) {
	var rec reapedSessionRecord
	found, err := jsonfile.Read(l.ReapedSession(session), &rec)
	return rec, found, err
}

// reapedSessions lists the reaps recorded by shpool session, newest first.
func reapedSessions(l boxlayout.Layout) ([]reapedSessionRecord, error) {
	entries, err := os.ReadDir(l.ReapedSessionsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing reaped sessions: %w", err)
	}
	var out []reapedSessionRecord
	for _, e := range entries {
		session, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		rec, found, err := readReapedSession(l, session)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, rec)
		}
	}
	slices.SortFunc(out, func(a, b reapedSessionRecord) int { return int(b.At - a.At) })
	return out, nil
}

// takeReaped returns and removes the records of a reap of claudeSession, if any.
func takeReaped(l boxlayout.Layout, claudeSession string) (reaper.Reason, error) {
	var rec reapedRecord
	if found, err := jsonfile.Read(l.Reaped(claudeSession), &rec); err != nil || !found {
		return "", err
	}
	byName, found, err := readReapedSession(l, rec.Session)
	if err != nil {
		return "", err
	}
	// The name may since have been reaped again with another Claude session.
	if found && byName.ClaudeSession == claudeSession {
		if err := os.Remove(l.ReapedSession(rec.Session)); err != nil {
			return "", fmt.Errorf("removing reaped session record: %w", err)
		}
	}
	if err := os.Remove(l.Reaped(claudeSession)); err != nil {
		return "", fmt.Errorf("removing reaped record: %w", err)
	}
	return rec.Reason, nil
}

// reattachTarget is the run an attach by name of a reaped session resumes.
func reattachTarget(rec reapedSessionRecord) (spec.Spec, error) {
	s := rec.Spec
	if s.Claude == nil {
		return spec.Spec{}, fmt.Errorf("reaped session %s has no resume command", s.Session)
	}
	s.Argv = slices.Concat(s.Claude.Resume, []string{"--resume", rec.ClaudeSession})
	s.Claude.Resumes = rec.ClaudeSession
	return s, s.Validate()
}

// resumeCommand is the laptop command that resumes a reaped session.
func resumeCommand(rec reapedSessionRecord) (string, error) {
	agent, err := spec.AgentOf(rec.Spec.Session)
	if err != nil {
		return "", err
	}
	return entrypoint.Name(agent) + " --resume " + rec.ClaudeSession, nil
}

func endedWhy(r reaper.Reason) string {
	if r == reaper.ReapedCap {
		return "after a long time detached"
	}
	return "for inactivity"
}

// announceResume is the one dim line a session resumed after a reap starts with.
func announceResume(w io.Writer, r reaper.Reason) {
	fmt.Fprintf(w, "\x1b[2m[remote-agents] resumed; the session had been ended %s\x1b[0m\n", endedWhy(r))
}

// reaperLog appends one line to the install's reaper log.
func reaperLog(l boxlayout.Layout, now time.Time, format string, args ...any) error {
	if err := os.MkdirAll(filepath.Dir(l.ReaperLog()), 0o700); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	f, err := os.OpenFile(l.ReaperLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening reaper log: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%s %s\n", now.UTC().Format(time.RFC3339), fmt.Sprintf(format, args...)); err != nil {
		f.Close()
		return fmt.Errorf("writing reaper log: %w", err)
	}
	return f.Close()
}
