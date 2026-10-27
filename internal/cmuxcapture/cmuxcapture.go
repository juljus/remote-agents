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
	"os/exec"
	"slices"
	"strconv"
	"strings"
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

// sessionStartArgv is the hook from which cmux records the launch it restores.
var sessionStartArgv = []string{"hooks", "claude", "session-start"}

// HookEnv is the environment a relayed hook runs with. cmux restores a
// session by running its claude wrapper with the CMUX_CUSTOM_CLAUDE_PATH the
// SessionStart hook reported, so that hook names the entry. Other hooks keep
// the user's own value: one may start claude through it, as auto-naming's
// summarizer can, and that must not become a run on the box.
func (c Captured) HookEnv(argv []string) map[string]string {
	env := maps.Clone(c.Env)
	if !slices.Equal(argv, sessionStartArgv) {
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

type settings struct {
	Hooks map[string][]struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// HookArgvs lists the cmux CLI argument lists of the hooks cmux injected; the
// relay runs nothing else. Hooks from the user's own settings run on the box.
func (c Captured) HookArgvs() ([][]string, error) {
	raw := ""
	for i, a := range c.Args {
		if a == "--settings" && i+1 < len(c.Args) {
			raw = c.Args[i+1]
			break
		}
	}
	if raw == "" {
		return nil, errors.New("cmux injected no --settings")
	}
	var s settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("parsing cmux --settings: %w", err)
	}
	var out [][]string
	for _, groups := range s.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				rest, ok := strings.CutPrefix(h.Command, hookBinRef+" ")
				if !ok {
					continue
				}
				// The box sends argv after its shell parsed the command; splitting
				// on spaces only matches that for commands without shell syntax.
				if strings.ContainsAny(rest, shellSyntax) {
					return nil, fmt.Errorf("cmux hook command %q uses shell syntax the relay cannot match", h.Command)
				}
				out = append(out, strings.Fields(rest))
			}
		}
	}
	return out, nil
}
