// Package claudeargs understands the Claude Code command line well enough to
// decide whether an invocation is an interactive session and which session id
// it will run under.
package claudeargs

import (
	"errors"
	"fmt"
	"strings"

	"remote-agents/internal/agentcli"
)

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
	settings    *SettingsArg
}

// SettingsArg locates a --settings value in an argument list.
type SettingsArg struct {
	// Index is the argument holding the value.
	Index int
	// Joined is true for the --settings=<value> form.
	Joined bool
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
		case arg == "--settings":
			if i+1 < len(args) {
				s.settings = &SettingsArg{Index: i + 1}
				i++
			}
		case strings.HasPrefix(arg, "--settings="):
			s.settings = &SettingsArg{Index: i, Joined: true}
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

// LastSettings finds the --settings that Claude applies: given several, it
// reads only the last.
func LastSettings(args []string) (SettingsArg, bool) {
	s := scanArgs(args)
	if s.settings == nil {
		return SettingsArg{}, false
	}
	return *s.settings, true
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

// Analyze plans an invocation. newID supplies a fresh session id when one must be injected.
func Analyze(args []string, newID func() string) (agentcli.Plan, error) {
	s := scanArgs(args)
	if s.command {
		return agentcli.Plan{Mode: agentcli.ModeCommand, Args: args}, nil
	}
	switch {
	case s.sessionID != "":
		if !agentcli.IsUUID(s.sessionID) {
			return agentcli.Plan{}, fmt.Errorf("--session-id must be a UUID, got %q", s.sessionID)
		}
		return agentcli.Plan{Mode: agentcli.ModeSession, Args: args, SessionID: s.sessionID}, nil
	case s.fork && (s.resume || s.cont):
		return inject(args, newID()), nil
	case s.resume && agentcli.IsUUID(s.resumeValue):
		return agentcli.Plan{Mode: agentcli.ModeSession, Args: args, SessionID: s.resumeValue, Resumes: true}, nil
	case s.resume || s.cont:
		return agentcli.Plan{Mode: agentcli.ModeSession, Args: args}, nil
	}
	return inject(args, newID()), nil
}

// SessionOf is the session id args run under, and whether they resume it
// rather than start it; "" when Claude picks the session or a new one.
func SessionOf(args []string) (string, bool) {
	s := scanArgs(args)
	switch {
	case s.sessionID != "":
		return s.sessionID, false
	case s.fork:
		return "", false
	case s.resume && agentcli.IsUUID(s.resumeValue):
		return s.resumeValue, true
	}
	return "", false
}

func inject(args []string, id string) agentcli.Plan {
	out := make([]string, 0, len(args)+2)
	out = append(out, "--session-id", id)
	out = append(out, args...)
	return agentcli.Plan{Mode: agentcli.ModeSession, Args: out, SessionID: id}
}

// ResumeArgs resume session id.
func ResumeArgs(id string) []string { return []string{"--resume", id} }

// CLI is the Claude Code command line as agentcli sees it.
type CLI struct{}

func (CLI) Analyze(args []string, newID func() string) (agentcli.Plan, error) {
	return Analyze(args, newID)
}
func (CLI) CheckDefaults(args []string) error { return CheckDefaults(args) }
func (CLI) ResumeBase(args []string) []string { return ResumeBase(args) }
func (CLI) ResumeArgs(id string) []string     { return ResumeArgs(id) }
func (CLI) SignInArgs() []string              { return []string{"auth", "login"} }

// HelloArgs ask the cheapest model, keep no session, and load none of the
// box's MCP servers, user settings or hooks.
func (CLI) HelloArgs() ([]string, bool) {
	return []string{"-p", "hi", "--model", "haiku", "--no-session-persistence", "--strict-mcp-config", "--setting-sources", "project"}, true
}
