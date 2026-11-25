// Package cmuxcapture asks cmux's own claude wrapper what it would launch:
// the wrapper is run with remote-agents standing in for the claude binary,
// which reports the final arguments (hook --settings included) and the CMUX_*
// environment instead of starting anything.
package cmuxcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"remote-agents/internal/hookexec"
)

// EnvVar switches the remote-agents binary into capture mode.
const EnvVar = "REMOTE_AGENTS_CMUX_CAPTURE"

// customClaudeEnvVar is how cmux's wrapper lets users pick the claude binary.
const customClaudeEnvVar = "CMUX_CUSTOM_CLAUDE_PATH"

type Captured struct {
	Args []string
	// Env holds the CMUX_* variables cmux's hook commands need.
	Env map[string]string
}

// InCmux reports whether the wrapper ran inside a cmux terminal surface.
func (c Captured) InCmux() bool {
	return c.Env["CMUX_SURFACE_ID"] != ""
}

type report struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
}

func Requested(getenv func(string) string) bool {
	return getenv(EnvVar) == "1"
}

// Report is what the binary does in capture mode.
func Report(w io.Writer, args, env []string) error {
	data, err := json.Marshal(report{Args: args, Env: env})
	if err != nil {
		return fmt.Errorf("encoding capture: %w", err)
	}
	_, err = w.Write(data)
	return err
}

func Capture(ctx context.Context, wrapper, self string, args, baseEnv []string) (Captured, error) {
	cmd := exec.CommandContext(ctx, wrapper, args...)
	cmd.Env = append(append([]string(nil), baseEnv...), customClaudeEnvVar+"="+self, EnvVar+"=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Captured{}, fmt.Errorf("running %s: %w: %s", wrapper, err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return Captured{}, fmt.Errorf("%s did not launch the capture binary (stderr: %q)", wrapper, strings.TrimSpace(stderr.String()))
	}
	dec := json.NewDecoder(&stdout)
	dec.DisallowUnknownFields()
	var r report
	if err := dec.Decode(&r); err != nil {
		return Captured{}, fmt.Errorf("parsing capture: %w", err)
	}
	return Captured{Args: r.Args, Env: cmuxEnv(r.Env)}, nil
}

// LaunchedByCmux recognises a run that cmux's wrapper started with entry as
// its claude binary, as cmux's session restore does: the wrapper already
// injected its hooks, so its arguments and environment are what Capture
// would report. The wrapper passes its pid on through exec, which tells this
// launch from a process that merely inherited the variables.
func LaunchedByCmux(entry string, args, env []string, pid int) (Captured, bool) {
	c := Captured{Args: args, Env: cmuxEnv(env)}
	if !c.InCmux() || c.Env[customClaudeEnvVar] != entry || c.Env["CMUX_AGENT_LAUNCH_EXECUTABLE"] != entry ||
		c.Env["CMUX_CLAUDE_PID"] != strconv.Itoa(pid) {
		return Captured{}, false
	}
	return c, true
}

// sessionStartArgvs are the hook from which cmux records the launch it
// restores: called directly by older cmux, queued by cmux 0.65.
var sessionStartArgvs = [][]string{
	{"hooks", "claude", "session-start"},
	{"hooks", "enqueue", "claude", "session-start"},
}

// HookEnv is the environment a relayed hook runs with. cmux restores a
// session by running its claude wrapper with the CMUX_CUSTOM_CLAUDE_PATH the
// SessionStart hook reported, so that hook names the entry. Other hooks keep
// the user's own value: one may start claude through it, as auto-naming's
// summarizer can, and that must not become a run on the box.
func (c Captured) HookEnv(argv []string) map[string]string {
	env := maps.Clone(c.Env)
	if !slices.ContainsFunc(sessionStartArgvs, func(a []string) bool { return slices.Equal(a, argv) }) {
		delete(env, customClaudeEnvVar)
	}
	return env
}

// cmuxEnv keeps the CMUX_* variables cmux's hook commands read, including
// CMUX_CUSTOM_CLAUDE_PATH, which names the entry in a capture run.
func cmuxEnv(kvs []string) map[string]string {
	env := map[string]string{}
	for _, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CMUX_") {
			env[k] = v
		}
	}
	return env
}

const shellSyntax = "\"'`$\\|&;<>()*?[]{}~#!\t\n"

// hookBinRef is how cmux's injected hook commands invoke its CLI.
const hookBinRef = `"${CMUX_CLAUDE_HOOK_CMUX_BIN:-cmux}"`

// quietFallback ends hook commands whose output Claude parses as JSON:
// "{}" stands in when the CLI fails. The relay answers a failed delivery
// with no output, which Claude reads the same way.
const quietFallback = ` 2>/dev/null || echo '{}'`

// cmuxCLIRun is how cmux 0.65's hook scripts invoke the CLI they resolve from
// CMUX_CLAUDE_HOOK_CMUX_BIN: after the variables they set to literal values,
// with or without an explicit socket, and answering "{}" when it fails.
var cmuxCLIRun = regexp.MustCompile(`((?:[A-Z][A-Z0-9_]*=[^\s"'$;|&]+ )*)"\$cmux_cli" (?:--socket "\$CMUX_SOCKET_PATH" )?([^"'$|&;<>(){}\n]*?) 2>/dev/null \|\| \{ cat >/dev/null; echo `)

// BoxLaunch is what the box needs to run claude with cmux's hooks relayed.
type BoxLaunch struct {
	// Args are the captured arguments with cmux's settings given inline, as
	// the box cannot read a settings file on the laptop, and each cmux hook
	// reduced to a plain call of the CLI that the box relays.
	Args []string
	// Allowed are the cmux CLI calls of those hooks; the relay runs nothing
	// else. Hooks from the user's own settings run on the box.
	Allowed []hookexec.Hook
}

// ForBox prepares the captured launch for the box. Older cmux passes its
// settings as JSON whose hooks call the CLI directly; cmux 0.65 passes a file
// path, and its hooks are scripts that spool the event or fall back to
// calling the CLI. On the box only that call can reach cmux, through the relay.
func (c Captured) ForBox() (BoxLaunch, error) {
	at, value, prefix := -1, "", ""
	for i, a := range c.Args {
		if a == "--settings" && i+1 < len(c.Args) {
			at, value = i+1, c.Args[i+1]
			break
		}
		if v, ok := strings.CutPrefix(a, "--settings="); ok {
			at, value, prefix = i, v, "--settings="
			break
		}
	}
	if at < 0 {
		return BoxLaunch{}, errors.New("cmux injected no --settings")
	}
	raw, err := loadSettings(value)
	if err != nil {
		return BoxLaunch{}, err
	}
	settings, allowed, err := relayedSettings(raw)
	if err != nil {
		return BoxLaunch{}, err
	}
	args := slices.Clone(c.Args)
	args[at] = prefix + settings
	return BoxLaunch{Args: args, Allowed: allowed}, nil
}

// loadSettings reads a --settings value the way Claude does: inline JSON, or
// the path of a JSON file.
func loadSettings(value string) ([]byte, error) {
	text := strings.TrimSpace(value)
	if strings.HasPrefix(text, "{") {
		return []byte(text), nil
	}
	if rest, ok := strings.CutPrefix(text, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolving cmux --settings %s: %w", text, err)
		}
		text = filepath.Join(home, rest)
	}
	data, err := os.ReadFile(text)
	if err != nil {
		return nil, fmt.Errorf("reading cmux --settings: %w", err)
	}
	return data, nil
}

// relayedSettings rewrites each cmux hook command in the settings JSON raw to
// a plain CLI call and returns the settings with those calls. Every other
// setting and hook is kept as it is.
func relayedSettings(raw []byte) (string, []hookexec.Hook, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var settings map[string]any
	if err := dec.Decode(&settings); err != nil {
		return "", nil, fmt.Errorf("parsing cmux --settings: %w", err)
	}
	if settings == nil {
		return "", nil, errors.New("cmux --settings is null, not a JSON object")
	}
	var allowed []hookexec.Hook
	if hooks, ok := settings["hooks"]; ok {
		events, ok := hooks.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("cmux --settings hooks is %T, not an object", hooks)
		}
		for event, groups := range events {
			groups, ok := groups.([]any)
			if !ok {
				return "", nil, fmt.Errorf("cmux --settings hooks.%s is not a list", event)
			}
			for _, g := range groups {
				g, ok := g.(map[string]any)
				if !ok {
					return "", nil, fmt.Errorf("cmux --settings hooks.%s holds a %T, not an object", event, g)
				}
				entries, _ := g["hooks"].([]any)
				for _, h := range entries {
					h, ok := h.(map[string]any)
					if !ok {
						continue
					}
					command, _ := h["command"].(string)
					hook, ok, err := cmuxHook(command)
					if err != nil {
						return "", nil, err
					}
					if !ok {
						continue
					}
					h["command"] = hookBinRef + " " + strings.Join(hook.Argv, " ")
					allowed = addHook(allowed, hook)
				}
			}
		}
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return "", nil, fmt.Errorf("encoding cmux settings: %w", err)
	}
	return string(data), allowed, nil
}

// addHook adds hook to hooks. The box relays only the argument list, so one
// that several commands share is quiet if any of them hides its failure.
func addHook(hooks []hookexec.Hook, hook hookexec.Hook) []hookexec.Hook {
	i := slices.IndexFunc(hooks, func(h hookexec.Hook) bool { return slices.Equal(h.Argv, hook.Argv) })
	if i < 0 {
		return append(hooks, hook)
	}
	hooks[i].Quiet = hooks[i].Quiet || hook.Quiet
	return hooks
}

// cmuxHook is the cmux CLI call a hook command makes, ok false when the
// command is not one of cmux's.
func cmuxHook(command string) (hookexec.Hook, bool, error) {
	if rest, ok := strings.CutPrefix(command, hookBinRef+" "); ok {
		rest, quiet := strings.CutSuffix(rest, quietFallback)
		// The box sends argv after its shell parsed the command; splitting
		// on spaces only matches that for commands without shell syntax.
		if strings.ContainsAny(rest, shellSyntax) {
			return hookexec.Hook{}, false, fmt.Errorf("cmux hook command %q uses shell syntax the relay cannot match", command)
		}
		return hookexec.Hook{Argv: strings.Fields(rest), Quiet: quiet}, true, nil
	}
	if !strings.Contains(command, `cmux_cli="${CMUX_CLAUDE_HOOK_CMUX_BIN:-`) {
		return hookexec.Hook{}, false, nil
	}
	runs := cmuxCLIRun.FindAllStringSubmatch(command, -1)
	if len(runs) == 0 {
		return hookexec.Hook{}, false, fmt.Errorf("cmux hook script %.80q… calls its CLI in a way the relay does not know", command)
	}
	for _, r := range runs[1:] {
		if r[1] != runs[0][1] || r[2] != runs[0][2] {
			return hookexec.Hook{}, false, fmt.Errorf("cmux hook script calls its CLI both as %q and as %q", runs[0][1]+runs[0][2], r[1]+r[2])
		}
	}
	env := map[string]string{}
	for _, kv := range strings.Fields(runs[0][1]) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return hookexec.Hook{Argv: strings.Fields(runs[0][2]), Env: env, Quiet: true}, true, nil
}
