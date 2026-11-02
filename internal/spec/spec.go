// Package spec is the launch description the laptop sends to the box.
package spec

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"

	"github.com/google/uuid"
)

type Mode string

const (
	// ModeSession attaches to the named shpool session, creating it to run Argv if absent.
	ModeSession Mode = "session"
	// ModeReattach attaches to the named shpool session and never starts anything.
	ModeReattach Mode = "reattach"
	// ModeExec runs Argv directly, without shpool.
	ModeExec Mode = "exec"
)

type Spec struct {
	Mode    Mode   `json:"mode"`
	Session string `json:"session,omitempty"`
	// RunID identifies one laptop-side run, so a reconnect can tell that the
	// session it created has already ended.
	RunID string `json:"run_id,omitempty"`
	// Attempt numbers the run's connection attempts from 1. The box refuses an
	// attempt older than one it already attached, which a delayed connection
	// would otherwise use to take the session from its successor. 0 leaves the
	// attempt unnumbered: it attaches unchecked.
	Attempt int               `json:"attempt,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Argv    []string          `json:"argv,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// HookRelay routes the agent's cmux hook commands to the laptop.
	HookRelay bool `json:"hook_relay,omitempty"`
	// Claude is set when the agent is Claude Code: the box then watches its
	// activity, may end it while detached and idle, and resumes it after that.
	Claude *Claude `json:"claude,omitempty"`
}

type Claude struct {
	// Resume is the command line that, followed by --resume <id>, resumes the session.
	Resume []string `json:"resume"`
	// Resumes is the Claude session id this run resumes, or "".
	Resumes string `json:"resumes,omitempty"`
}

func (c *Claude) validate() error {
	if len(c.Resume) == 0 || c.Resume[0] == "" {
		return errors.New("empty resume command")
	}
	if c.Resumes == "" {
		return nil
	}
	if _, err := uuid.Parse(c.Resumes); err != nil {
		return fmt.Errorf("invalid resumed session id %q: %w", c.Resumes, err)
	}
	return nil
}

// Exit codes the box reports besides the agent's own.
const (
	// ExitTakenOver: another terminal attached to the session, which keeps running.
	ExitTakenOver = 75
	// ExitStaleAttempt: a newer connection attempt of the same run holds the session.
	ExitStaleAttempt = 76
	// ExitAgentReserved stands in for an agent exit status that means something
	// else on the link: 255, which ssh reserves for its own failures, and the
	// box's own codes above.
	ExitAgentReserved = 254
	// ExitNotListening: nothing listens on the port that remote-agents-box listening checked.
	ExitNotListening = 3
)

var sessionNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func SessionName(agent, sessionID string) string {
	return agent + "-" + sessionID
}

// AgentOf is the agent a SessionName names.
func AgentOf(session string) (string, error) {
	cut := len(session) - len(uuid.Nil.String()) - 1
	if cut <= 0 || session[cut] != '-' {
		return "", fmt.Errorf("session name %q does not end with -<uuid>", session)
	}
	if _, err := uuid.Parse(session[cut+1:]); err != nil {
		return "", fmt.Errorf("session name %q does not end with -<uuid>: %w", session, err)
	}
	return session[:cut], nil
}

func ValidSessionName(name string) error {
	if !sessionNamePattern.MatchString(name) {
		return fmt.Errorf("invalid session name %q", name)
	}
	return nil
}

func (s Spec) Validate() error {
	switch s.Mode {
	case ModeSession:
		if err := ValidSessionName(s.Session); err != nil {
			return err
		}
		if _, err := uuid.Parse(s.RunID); err != nil {
			return fmt.Errorf("invalid run id %q: %w", s.RunID, err)
		}
		if err := s.validateAttempt(); err != nil {
			return err
		}
		if s.Claude != nil {
			if err := s.Claude.validate(); err != nil {
				return err
			}
		}
		return s.validateRun()
	case ModeReattach:
		if err := ValidSessionName(s.Session); err != nil {
			return err
		}
		if err := s.validateAttempt(); err != nil {
			return err
		}
		if s.RunID != "" {
			if _, err := uuid.Parse(s.RunID); err != nil {
				return fmt.Errorf("invalid run id %q: %w", s.RunID, err)
			}
		}
		if len(s.Argv) != 0 || s.Claude != nil {
			return errors.New("reattach spec must not carry a command")
		}
		return nil
	case ModeExec:
		if s.Session != "" || s.RunID != "" || s.Attempt != 0 || s.Claude != nil {
			return errors.New("exec spec must not name a session")
		}
		return s.validateRun()
	}
	return fmt.Errorf("unknown mode %q", s.Mode)
}

func (s Spec) validateAttempt() error {
	if s.Attempt < 0 {
		return fmt.Errorf("invalid attempt %d", s.Attempt)
	}
	if s.Attempt > 0 && s.RunID == "" {
		return errors.New("a numbered attempt needs a run id")
	}
	return nil
}

func (s Spec) validateRun() error {
	if !path.IsAbs(s.Workdir) {
		return fmt.Errorf("workdir must be absolute, got %q", s.Workdir)
	}
	if len(s.Argv) == 0 || s.Argv[0] == "" {
		return errors.New("empty command")
	}
	return nil
}

// Encode returns a shell-safe token.
func (s Spec) Encode() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encoding spec: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func Decode(token string) (Spec, error) {
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Spec{}, fmt.Errorf("decoding spec: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("parsing spec: %w", err)
	}
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}
