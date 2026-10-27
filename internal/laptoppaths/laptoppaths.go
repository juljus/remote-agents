// Package laptoppaths fixes where the laptop side keeps its socket and logs.
package laptoppaths

import (
	"fmt"
	"os"
	"path/filepath"
)

type Paths struct {
	StateDir string
	LogDir   string
}

func Default() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolving home directory: %w", err)
	}
	return Paths{
		StateDir: filepath.Join(home, ".local", "state", "remote-agents"),
		LogDir:   filepath.Join(home, "Library", "Logs", "remote-agents"),
	}, nil
}

// ControlSocket is where session wrappers reach the link daemon.
func (p Paths) ControlSocket() string { return filepath.Join(p.StateDir, "control.sock") }

// SessionLog collects what session wrappers cannot show over the agent's screen.
func (p Paths) SessionLog() string { return filepath.Join(p.LogDir, "sessions.log") }
