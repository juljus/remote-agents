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
	"remote-agents/internal/jsonfile"
	"remote-agents/internal/reaper"
	"remote-agents/internal/spec"
)

// releaseWait bounds the wait for an agent of an earlier launch that still
// holds the session a launch resumes.
const releaseWait = 10 * time.Second

// runRecord is the session a live run's agent was last seen running,
// with the transcript the agent writes it to.
type runRecord struct {
	AgentSession string `json:"claude_session,omitempty"`
	Transcript   string `json:"transcript,omitempty"`
}

func writeRunRecord(l boxlayout.Layout, runID string, rec runRecord) error {
	if err := os.MkdirAll(l.RunsDir(), 0o700); err != nil {
		return fmt.Errorf("creating runs dir: %w", err)
	}
	return jsonfile.Write(l.RunRecord(runID), rec)
}

func readRunRecord(l boxlayout.Layout, runID string) (runRecord, bool, error) {
	var rec runRecord
	found, err := jsonfile.Read(l.RunRecord(runID), &rec)
	return rec, found, err
}

func removeRunRecord(l boxlayout.Layout, runID string) error {
	if err := os.Remove(l.RunRecord(runID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing run record: %w", err)
	}
	return nil
}

// lostRecord is when a launcher stopped from outside lost its session, in
// Unix milliseconds.
type lostRecord struct {
	At int64 `json:"at"`
}

func recordLost(l boxlayout.Layout, session string, now time.Time) error {
	if err := os.MkdirAll(l.LostDir(), 0o700); err != nil {
		return fmt.Errorf("creating lost dir: %w", err)
	}
	return jsonfile.Write(l.Lost(session), lostRecord{At: now.UnixMilli()})
}

// sessionLostSince reports whether session was lost at or after since, and
// forgets the loss once reported.
func sessionLostSince(l boxlayout.Layout, session string, since time.Time) (bool, error) {
	var rec lostRecord
	found, err := jsonfile.Read(l.Lost(session), &rec)
	if err != nil || !found || rec.At < since.UnixMilli() {
		return false, err
	}
	if err := os.Remove(l.Lost(session)); err != nil {
		return false, fmt.Errorf("forgetting the loss of %s: %w", session, err)
	}
	return true, nil
}

// resumeLostLaunch continues the agent session of the run's earlier launch
// when that launch left its record but no ended marker: something killed its
// launcher with it. The run's own command would start its session anew,
// which Claude refuses once that session has a transcript. The session is the
// one the hooks last reported, else the record's. It reports whether it
// resumes a lost launch's session and returns the record the relaunch starts
// with.
func resumeLostLaunch(l boxlayout.Layout, s *spec.Spec, logf func(string, ...any)) (runRecord, bool, error) {
	rec, lost, err := readRunRecord(l, s.RunID)
	if err != nil || !lost {
		return runRecord{}, false, err
	}
	hooks, err := reaper.ReadHookState(l.HookState(s.Session))
	if err != nil {
		return runRecord{}, false, err
	}
	if hooks.AgentSession != "" {
		rec = runRecord{AgentSession: hooks.AgentSession, Transcript: hooks.Transcript}
	}
	if rec.AgentSession == "" {
		logf("run %s lost its launcher before its agent reported a session", s.RunID)
		return rec, false, nil
	}
	kind, agent := followed(*s)
	args, how, err := kind.relaunchArgs(rec)
	if err != nil {
		return runRecord{}, false, err
	}
	s.Argv = slices.Concat(agent.Resume, args)
	logf("run %s lost its launcher; %s", s.RunID, how)
	return rec, true, nil
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

// claudeHolder finds a running Claude registered in dir under agentSession.
// A registration Claude is rewriting in place may not parse; unless its pid,
// the file's name, has exited, unreadable is then the error of the last such
// file, and a later look may read it.
func claudeHolder(dir string, proc fs.FS, agentSession string) (pid int, unreadable, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("listing Claude sessions: %w", err)
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
			return 0, nil, fmt.Errorf("reading Claude session %s: %w", e.Name(), err)
		}
		var r claudeRegistration
		if err := json.Unmarshal(data, &r); err != nil {
			owner, perr := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json"))
			if perr != nil {
				return 0, nil, fmt.Errorf("registration %s in the Claude sessions dir is not named by a pid", e.Name())
			}
			exited, xerr := reaper.Exited(proc, owner)
			if xerr != nil {
				return 0, nil, xerr
			}
			if !exited {
				unreadable = fmt.Errorf("parsing Claude session %s: %w", e.Name(), err)
			}
			continue
		}
		if r.SessionID != agentSession {
			continue
		}
		start, err := strconv.ParseUint(r.ProcStart, 10, 64)
		if err != nil {
			return 0, nil, fmt.Errorf("procStart %q of Claude session %s: %w", r.ProcStart, e.Name(), err)
		}
		running, err := reaper.Running(proc, r.PID, start)
		if err != nil {
			return 0, nil, err
		}
		if running {
			return r.PID, nil, nil
		}
	}
	return 0, unreadable, nil
}

// holderProbe looks once for a running agent that holds a session. holder
// describes it, as "pid 12"; unsure is set when the look could not tell and
// a later one may.
type holderProbe func() (holder string, unsure error, err error)

func claudeProbe(dir string, proc fs.FS, agentSession string) holderProbe {
	return func() (string, error, error) {
		pid, unreadable, err := claudeHolder(dir, proc, agentSession)
		if err != nil || pid == 0 {
			return "", unreadable, err
		}
		return "pid " + strconv.Itoa(pid), nil, nil
	}
}

// awaitRelease waits up to wait for no running agent to hold agentSession:
// two would write one transcript.
func awaitRelease(agentSession string, wait time.Duration, probe holderProbe, logf func(string, ...any)) error {
	deadline := time.Now().Add(wait)
	held := ""
	for {
		holder, unsure, err := probe()
		if err != nil {
			return err
		}
		if holder == "" && unsure == nil {
			if held != "" {
				logf("session %s is free again", agentSession)
			}
			return nil
		}
		if holder != "" && holder != held {
			logf("session %s is still held by %s; waiting up to %s for it to exit", agentSession, holder, wait)
			held = holder
		}
		if time.Now().After(deadline) {
			if holder == "" {
				err = fmt.Errorf("telling whether a running agent holds session %s: %w", agentSession, unsure)
			} else {
				err = fmt.Errorf("%s still holds session %s after %s; end it, then reconnect", holder, agentSession, wait)
			}
			logf("not starting the agent: %v", err)
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}
