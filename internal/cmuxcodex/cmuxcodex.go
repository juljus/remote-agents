// Package cmuxcodex lists the hooks through which cmux follows Codex. cmux
// itself writes them into ~/.codex/hooks.json (`cmux hooks codex install`);
// for a Codex on the box, the box adds the same hooks to each launch and
// relays them to the laptop, where they run in the wrapper's cmux terminal.
package cmuxcodex

import (
	"encoding/base64"
	"errors"
	"maps"
	"path/filepath"
	"strconv"
	"strings"

	"remote-agents/internal/hookexec"
)

// PIDEnvVar names the process cmux follows as the Codex agent; on the laptop
// that is the wrapper.
const PIDEnvVar = "CMUX_CODEX_PID"

type Hook struct {
	// Event is the Codex hook event the hook runs on.
	Event string
	// Argv is what the hook passes to the cmux CLI.
	Argv []string
}

func feed(event string) Hook {
	return Hook{Event: event, Argv: []string{"hooks", "feed", "--source", "codex", "--event", event}}
}

// Hooks are those cmux 0.64 installs for Codex.
var Hooks = []Hook{
	{Event: "SessionStart", Argv: []string{"hooks", "codex", "session-start"}},
	{Event: "UserPromptSubmit", Argv: []string{"hooks", "codex", "prompt-submit"}},
	{Event: "Stop", Argv: []string{"hooks", "codex", "stop"}},
	feed("PermissionRequest"),
	feed("PreToolUse"),
	feed("PostToolUse"),
	feed("PreCompact"),
	feed("PostCompact"),
	feed("SubagentStart"),
	feed("SubagentStop"),
}

// Argvs lists the cmux CLI argument lists of Hooks, which alone the laptop relays.
func Argvs() [][]string {
	out := make([][]string, len(Hooks))
	for i, h := range Hooks {
		out[i] = h.Argv
	}
	return out
}

// launchEnvPrefix starts the variables in which an agent wrapper describes
// its launch to cmux. Inherited from an outer agent, they describe that one.
const launchEnvPrefix = "CMUX_AGENT_LAUNCH_"

// Launch is how the wrapper was started: cmux restores a Codex tab by
// running Executable with resume <id> and the flags among Args, in Dir.
type Launch struct {
	Executable string
	Args       []string
	Dir        string
}

func (l Launch) env() map[string]string {
	argv := append([]string{l.Executable}, l.Args...)
	return map[string]string{
		launchEnvPrefix + "KIND":       "codex",
		launchEnvPrefix + "EXECUTABLE": l.Executable,
		launchEnvPrefix + "ARGV_B64":   base64.StdEncoding.EncodeToString([]byte(strings.Join(argv, "\x00"))),
		launchEnvPrefix + "CWD":        l.Dir,
	}
}

// NewExecutor runs, in the wrapper's cmux terminal, the hooks a Codex on the
// box relays; environ is the wrapper's environment and pid its process.
// Outside cmux there is nothing to relay to, and it returns nil.
func NewExecutor(environ []string, pid int, launch Launch) (*hookexec.Executor, error) {
	base := make([]string, 0, len(environ))
	env := map[string]string{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, launchEnvPrefix) || k == "CMUX_CUSTOM_CLAUDE_PATH" {
			continue
		}
		base = append(base, kv)
		if strings.HasPrefix(k, "CMUX_") {
			env[k] = v
		}
	}
	if env["CMUX_SURFACE_ID"] == "" {
		return nil, nil
	}
	cli := env["CMUX_BUNDLED_CLI_PATH"]
	if !filepath.IsAbs(cli) {
		return nil, errors.New("cmux set no absolute CMUX_BUNDLED_CLI_PATH, the cmux CLI its Codex hooks run")
	}
	env[hookexec.BinEnvVar] = cli
	env[PIDEnvVar] = strconv.Itoa(pid)
	maps.Copy(env, launch.env())
	return &hookexec.Executor{BaseEnv: base, Env: func([]string) map[string]string { return env }, Allowed: Argvs()}, nil
}
