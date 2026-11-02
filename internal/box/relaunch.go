package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/claudeargs"
	"remote-agents/internal/jsonfile"
	"remote-agents/internal/reaper"
	"remote-agents/internal/spec"
)

// releaseWait bounds the wait for a Claude of an earlier launch that still
// holds the session a launch resumes.
const releaseWait = 10 * time.Second

// markStarted records that the run's agent has been started once.
func markStarted(l boxlayout.Layout, runID string) error {
	if err := os.MkdirAll(l.StartedDir(), 0o700); err != nil {
		return fmt.Errorf("creating started dir: %w", err)
	}
	return jsonfile.Write(l.StartedMarker(runID), struct{}{})
}

func launchStarted(l boxlayout.Layout, runID string) (bool, error) {
	return jsonfile.Read(l.StartedMarker(runID), &struct{}{})
}

// resumeLostLaunch points s at the Claude session of the run's earlier
// launch, when that launch ended without an ended marker: something killed
// its launcher with it. The run's own command would start that session anew,
// which Claude refuses once its transcript exists. The session is the one
// the earlier launch's hooks last reported, which /clear may have changed,
// else the one its command line started.
func resumeLostLaunch(l boxlayout.Layout, s *spec.Spec, logf func(string, ...any)) error {
	if s.Claude == nil {
		return nil
	}
	started, err := launchStarted(l, s.RunID)
	if err != nil || !started {
		return err
	}
	hooks, err := reaper.ReadHookState(l.HookState(s.Session))
	if err != nil {
		return err
	}
	id := hooks.ClaudeSession
	if id == "" {
		id, _ = claudeargs.SessionOf(s.Argv[1:])
	}
	if id == "" {
		logf("run %s lost its launcher before its Claude session was known; starting its command again", s.RunID)
		return nil
	}
	s.Argv = slices.Concat(s.Claude.Resume, []string{"--resume", id})
	logf("run %s lost its launcher; resuming Claude session %s", s.RunID, id)
	return nil
}

// claudeSessionsDir is where each running Claude registers itself as <pid>.json.
func claudeSessionsDir(l boxlayout.Layout, env []string) string {
	if dir := envValue(env, "CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	return filepath.Join(l.Home, ".claude", "sessions")
}

// claudeRegistration is the part of a Claude's registration that tells which
// process holds which session; ProcStart is the process's start time.
type claudeRegistration struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	ProcStart string `json:"procStart"`
}

// claudeHolder is the pid of a running Claude registered in dir under
// claudeSession, or 0.
func claudeHolder(dir string, proc fs.FS, claudeSession string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("listing Claude sessions: %w", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		// The Claude exited since the listing.
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("reading Claude session %s: %w", e.Name(), err)
		}
		var r claudeRegistration
		if err := json.Unmarshal(data, &r); err != nil {
			return 0, fmt.Errorf("parsing Claude session %s: %w", e.Name(), err)
		}
		if r.SessionID != claudeSession {
			continue
		}
		start, err := strconv.ParseUint(r.ProcStart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("procStart %q of Claude session %s: %w", r.ProcStart, e.Name(), err)
		}
		running, err := reaper.Running(proc, r.PID, start)
		if err != nil {
			return 0, err
		}
		if running {
			return r.PID, nil
		}
	}
	return 0, nil
}

// awaitRelease waits up to wait for no running Claude to hold claudeSession:
// two would write one transcript.
func awaitRelease(dir string, proc fs.FS, claudeSession string, wait time.Duration, logf func(string, ...any)) error {
	deadline := time.Now().Add(wait)
	held := 0
	for {
		pid, err := claudeHolder(dir, proc, claudeSession)
		if err != nil {
			return err
		}
		if pid == 0 {
			if held != 0 {
				logf("Claude session %s is free again", claudeSession)
			}
			return nil
		}
		if held == 0 {
			logf("Claude session %s is still held by pid %d; waiting up to %s for it to exit", claudeSession, pid, wait)
		}
		held = pid
		if time.Now().After(deadline) {
			err := fmt.Errorf("pid %d still holds Claude session %s after %s; end it, then reconnect", pid, claudeSession, wait)
			logf("not resuming: %v", err)
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}
