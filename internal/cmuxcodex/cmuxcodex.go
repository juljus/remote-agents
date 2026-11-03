// Package cmuxcodex lists the hooks through which cmux follows Codex. cmux
// itself writes them into ~/.codex/hooks.json (`cmux hooks codex install`);
// for a Codex on the box, the box adds the same hooks to each launch and
// relays them to the laptop, where they run in the wrapper's cmux terminal.
package cmuxcodex

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
