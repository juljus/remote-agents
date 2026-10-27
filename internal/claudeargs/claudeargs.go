// Package claudeargs understands the Claude Code command line well enough to
// decide whether an invocation is an interactive session and which session id
// it will run under.
package claudeargs

import (
	"errors"
	"fmt"
	"strings"

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
	// Args is the argument list to run, including an injected --session-id when needed.
	Args []string
	// SessionID is the Claude session id the run will use, or "" when Claude picks it
	// (resume picker, --continue).
	SessionID string
	// Resumes is true when SessionID names a session that already exists.
	Resumes bool
}

// Options that take a separate value argument, mirroring cmux's claude wrapper.
var valueOptions = map[string]bool{
	"--add-dir": true, "--agent": true, "--agents": true, "--allowedTools": true,
	"--allowed-tools": true, "--append-system-prompt": true, "--betas": true,
	"--debug-file": true, "--disallowedTools": true, "--disallowed-tools": true,
	"--effort": true, "--fallback-model": true, "--file": true, "--input-format": true,
	"--json-schema": true, "--max-budget-usd": true, "--mcp-config": true, "--model": true,
	"-m": true, "-n": true, "--name": true, "--output-format": true, "--permission-mode": true,
	"--plugin-dir": true, "--plugin-url": true, "--remote-control-session-name-prefix": true,
	"--setting-sources": true, "--settings": true, "--system-prompt": true, "--tools": true,
	"--session-id": true,
}

var subcommands = map[string]bool{
	"agents": true, "auth": true, "auto-mode": true, "config": true, "api-key": true,
	"daemon": true, "doctor": true, "install": true, "mcp": true, "experimental-next": true,
	"plugin": true, "plugins": true, "project": true, "rc": true, "remote-control": true,
	"setup-token": true, "ultrareview": true, "update": true, "upgrade": true,
}

var commandFlags = map[string]bool{
	"--help": true, "-h": true, "--version": true, "-v": true, "--print": true, "-p": true,
}

type scan struct {
	command     bool
	resume      bool
	resumeValue string
	cont        bool
	fork        bool
	sessionID   string
}

func scanArgs(args []string) scan {
	var s scan
	sawPositional := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return s
		case commandFlags[arg]:
			s.command = true
		case arg == "--resume" || arg == "-r":
			s.resume = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				s.resumeValue = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--resume="):
			s.resume = true
			s.resumeValue = strings.TrimPrefix(arg, "--resume=")
		case arg == "--continue" || arg == "-c":
			s.cont = true
		case arg == "--fork-session":
			s.fork = true
		case arg == "--session-id":
			if i+1 < len(args) {
				s.sessionID = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--session-id="):
			s.sessionID = strings.TrimPrefix(arg, "--session-id=")
		case strings.HasPrefix(arg, "-"):
			if !strings.Contains(arg, "=") && valueOptions[arg] {
				i++
			}
		default:
			if !sawPositional && subcommands[arg] {
				s.command = true
			}
			sawPositional = true
		}
	}
	return s
}

// ResumeBase is args without the session they pick and without a prompt:
// the options a later --resume of the same session runs with.
func ResumeBase(args []string) []string {
	out := []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return out
		case arg == "--resume" || arg == "-r":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		case arg == "--session-id":
			i++
		case arg == "--continue" || arg == "-c" || arg == "--fork-session",
			strings.HasPrefix(arg, "--resume="), strings.HasPrefix(arg, "--session-id="):
		case strings.HasPrefix(arg, "-"):
			out = append(out, arg)
			if !strings.Contains(arg, "=") && valueOptions[arg] && i+1 < len(args) {
				i++
				out = append(out, args[i])
			}
		}
	}
	return out
}

// CheckDefaults accepts arguments that are prepended to every invocation,
// such as --dangerously-skip-permissions. They may not pick a session or a
// one-shot mode or end option parsing, and an option's value must be joined
// with "=": Claude reads a separate value of some options (--allowedTools,
// --add-dir) as a list, which would swallow the user's own arguments.
func CheckDefaults(args []string) error {
	for _, arg := range args {
		switch {
		case arg == "":
			return errors.New("an argument is empty")
		case arg == "--" || !strings.HasPrefix(arg, "-"):
			return fmt.Errorf("%q is not an option", arg)
		case commandFlags[arg]:
			return fmt.Errorf("%s makes every run a one-shot command", arg)
		}
		if s := scanArgs([]string{arg}); s.resume || s.cont || s.fork || arg == "--session-id" || strings.HasPrefix(arg, "--session-id=") {
			return fmt.Errorf("%s picks the session; pass it on the command line", arg)
		}
		// Commander reads a short option's "=x" as the value "=x", so only long forms can be joined.
		if short, _, _ := strings.Cut(arg, "="); len(short) == 2 && valueOptions[short] {
			return fmt.Errorf("%s takes a value; use its long form as --option=<value>", short)
		}
		if valueOptions[arg] {
			return fmt.Errorf("write %s=<value>", arg)
		}
	}
	return nil
}

// IsUUID reports whether s is a session id in the canonical form Claude uses.
func IsUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil && len(s) == 36
}

// Analyze plans an invocation. newID supplies a fresh session id when one must be injected.
func Analyze(args []string, newID func() string) (Plan, error) {
	s := scanArgs(args)
	if s.command {
		return Plan{Mode: ModeCommand, Args: args}, nil
	}
	switch {
	case s.sessionID != "":
		if !IsUUID(s.sessionID) {
			return Plan{}, fmt.Errorf("--session-id must be a UUID, got %q", s.sessionID)
		}
		return Plan{Mode: ModeSession, Args: args, SessionID: s.sessionID}, nil
	case s.fork && (s.resume || s.cont):
		return inject(args, newID()), nil
	case s.resume && IsUUID(s.resumeValue):
		return Plan{Mode: ModeSession, Args: args, SessionID: s.resumeValue, Resumes: true}, nil
	case s.resume || s.cont:
		return Plan{Mode: ModeSession, Args: args}, nil
	}
	return inject(args, newID()), nil
}

func inject(args []string, id string) Plan {
	out := make([]string, 0, len(args)+2)
	out = append(out, "--session-id", id)
	out = append(out, args...)
	return Plan{Mode: ModeSession, Args: out, SessionID: id}
}
