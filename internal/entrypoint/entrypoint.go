// Package entrypoint names the laptop commands that run an agent on the box:
// r<agent>, a symlink to the remote-agents binary, so rclaude runs claude there.
package entrypoint

import (
	"path/filepath"
	"strings"
)

// Binary is the remote-agents binary's own name, which runs subcommands instead.
const Binary = "remote-agents"

func Name(agent string) string { return "r" + agent }

// Agent reads the agent an invocation names through its argv[0].
func Agent(argv0 string) (string, bool) {
	base := filepath.Base(argv0)
	if base == Binary {
		return "", false
	}
	agent, ok := strings.CutPrefix(base, "r")
	if !ok || agent == "" {
		return "", false
	}
	return agent, true
}
