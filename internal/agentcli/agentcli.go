// Package agentcli is what the wrapper needs to know of an agent's command
// line, whatever the agent: whether an invocation is an interactive session,
// which session it runs, and how that session is resumed.
package agentcli

import (
	"fmt"

	"github.com/google/uuid"
)

type Mode int

const (
	// ModeSession is an interactive TUI session, held in shpool on the box.
	ModeSession Mode = iota
	// ModeCommand is a one-shot invocation (subcommand, --print, --help, --version).
	ModeCommand
)

func (m Mode) String() string {
	switch m {
	case ModeSession:
		return "session"
	case ModeCommand:
		return "command"
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

type Plan struct {
	Mode Mode
	// Args is the argument list to run, including any session id the wrapper injected.
	Args []string
	// SessionID is the agent session id the run will use, or "" when the
	// agent picks it (a resume picker, a new Codex session).
	SessionID string
	// Resumes is true when SessionID names a session that already exists.
	Resumes bool
}

// IsUUID reports whether s is a session id in the canonical form Claude and Codex use.
func IsUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil && len(s) == 36
}

// CLI understands one kind of agent's command line.
type CLI interface {
	// Analyze plans an invocation. newID supplies a fresh session id when one must be injected.
	Analyze(args []string, newID func() string) (Plan, error)
	// CheckDefaults accepts arguments that are prepended to every invocation.
	CheckDefaults(args []string) error
	// ResumeBase is args without the session they pick and without a prompt:
	// the options a later resume of the same session runs with.
	ResumeBase(args []string) []string
	// ResumeArgs follow the agent command and ResumeBase to resume session id.
	ResumeArgs(id string) []string
}
