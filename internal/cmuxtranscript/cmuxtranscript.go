// Package cmuxtranscript gives cmux the transcript file it looks for before it
// restores a Claude session. cmux saves a tab's agent as running, and resumes
// it after a restart, only when the session's transcript exists under the
// laptop's Claude directory. A box session's transcript is on the box, so the
// laptop gets a stand-in for it.
package cmuxtranscript

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"remote-agents/internal/agentcli"
)

// standIn is one line Claude reads no message from, so the file is no
// session to it. cmux needs only a file that is not empty.
const standIn = `{"type":"remote-agents-stand-in","note":"This session runs on the box; its transcript is there. cmux restores the tab only when this file exists."}` + "\n"

// StandIns writes the stand-ins of one wrapper's sessions.
type StandIns struct {
	// ClaudeDir is the laptop's Claude directory, where cmux looks.
	ClaudeDir string

	mu sync.Mutex
	// tried holds the sessions already attempted: every hook names its
	// session, and one failure is not worth a log line per hook.
	tried map[string]bool
}

// Observe takes a relayed Claude hook payload and, the first time it names a
// session, writes that session's stand-in unless a file is already there. It
// goes in the project directory the box keeps the transcript in: with the
// same home on both sides that is the path cmux records, and with another
// cmux finds the file by the session's id.
func (s *StandIns) Observe(payload []byte) error {
	var p struct {
		SessionID  string `json:"session_id"`
		Transcript string `json:"transcript_path"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("reading hook payload: %w", err)
	}
	if p.SessionID == "" || p.Transcript == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tried[p.SessionID] {
		return nil
	}
	if s.tried == nil {
		s.tried = map[string]bool{}
	}
	s.tried[p.SessionID] = true

	// The box names the path; only a session's own file in a project directory is written.
	if !agentcli.IsUUID(p.SessionID) {
		return fmt.Errorf("hook payload session_id %q is not a session id", p.SessionID)
	}
	file := p.SessionID + ".jsonl"
	project := path.Base(path.Dir(p.Transcript))
	if !path.IsAbs(p.Transcript) || path.Clean(p.Transcript) != p.Transcript || path.Base(p.Transcript) != file ||
		project == "." || project == ".." || project == "/" || strings.ContainsAny(project, "\\\x00") {
		return fmt.Errorf("hook payload transcript_path %q is not session %s's transcript in a project directory", p.Transcript, p.SessionID)
	}
	dir := filepath.Join(s.ClaudeDir, "projects", project)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the transcript stand-in's directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, file), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating the transcript stand-in: %w", err)
	}
	if _, err := f.WriteString(standIn); err != nil {
		f.Close()
		return fmt.Errorf("writing the transcript stand-in: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing the transcript stand-in: %w", err)
	}
	return nil
}
