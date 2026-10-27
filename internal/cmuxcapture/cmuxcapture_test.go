package cmuxcapture

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if Requested(os.Getenv) {
		if err := Report(os.Stdout, os.Args[1:], os.Environ()); err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeWrapper(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cmux-claude-wrapper")
	if err := os.WriteFile(path, []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func self(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestCaptureInjectedArgsAndEnv(t *testing.T) {
	wrapper := fakeWrapper(t, `
export CMUX_CLAUDE_PID=$$
export CMUX_CLAUDE_HOOK_CMUX_BIN=/Applications/cmux.app/Contents/Resources/bin/cmux
export NODE_OPTIONS=--require=/tmp/guard.cjs
exec "$CMUX_CUSTOM_CLAUDE_PATH" --settings '{"hooks":{"Stop":[]}}' "$@"`)
	base := []string{"PATH=/usr/bin:/bin", "CMUX_SURFACE_ID=surface-1", "CMUX_SOCKET_PATH=/tmp/cmux.sock", "HOME=/Users/me",
		"CMUX_CUSTOM_CLAUDE_PATH=/Users/me/.local/bin/claude-cmux"}
	got, err := Capture(context.Background(), wrapper, self(t), []string{"--session-id", "abc", "hello world"}, base)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	wantArgs := []string{"--settings", `{"hooks":{"Stop":[]}}`, "--session-id", "abc", "hello world"}
	if !slices.Equal(got.Args, wantArgs) {
		t.Fatalf("args = %q, want %q", got.Args, wantArgs)
	}
	if got.Env["CMUX_SURFACE_ID"] != "surface-1" || got.Env["CMUX_SOCKET_PATH"] != "/tmp/cmux.sock" ||
		got.Env["CMUX_CLAUDE_HOOK_CMUX_BIN"] != "/Applications/cmux.app/Contents/Resources/bin/cmux" {
		t.Fatalf("env = %v", got.Env)
	}
	for k := range got.Env {
		if !strings.HasPrefix(k, "CMUX_") {
			t.Fatalf("unexpected captured key %s", k)
		}
	}
	// cmux restores a session by running its claude wrapper with the
	// CMUX_CUSTOM_CLAUDE_PATH the hooks reported, so they must name the entry.
	if got.Env["CMUX_CUSTOM_CLAUDE_PATH"] != self(t) {
		t.Fatalf("CMUX_CUSTOM_CLAUDE_PATH = %q, want the entry %q", got.Env["CMUX_CUSTOM_CLAUDE_PATH"], self(t))
	}
	if !got.InCmux() {
		t.Fatal("expected InCmux")
	}
}

func launchedEnv(entry string, pid int) []string {
	return []string{
		"PATH=/usr/bin:/bin", "HOME=/Users/me", "CMUX_SURFACE_ID=surface-1",
		"CMUX_CUSTOM_CLAUDE_PATH=" + entry, "CMUX_AGENT_LAUNCH_EXECUTABLE=" + entry,
		"CMUX_CLAUDE_PID=" + strconv.Itoa(pid), "CMUX_CLAUDE_HOOK_CMUX_BIN=/Applications/cmux.app/Contents/Resources/bin/cmux",
	}
}

// cmux's restore runs its wrapper, which already injected the hooks before starting the entry.
func TestLaunchedByCmuxUsesTheWrappersWork(t *testing.T) {
	entry := "/Users/me/.local/bin/rclaude"
	args := []string{"--settings", `{"hooks":{"Stop":[]}}`, "--resume", "abc"}
	got, ok := LaunchedByCmux(entry, args, launchedEnv(entry, 4242), 4242)
	if !ok {
		t.Fatal("expected a cmux launch")
	}
	if !slices.Equal(got.Args, args) {
		t.Fatalf("args = %q", got.Args)
	}
	if got.Env["CMUX_SURFACE_ID"] != "surface-1" || got.Env["CMUX_CUSTOM_CLAUDE_PATH"] != entry || got.Env["PATH"] != "" {
		t.Fatalf("env = %v", got.Env)
	}
}

func TestNotLaunchedByCmux(t *testing.T) {
	entry := "/Users/me/.local/bin/rclaude"
	for name, env := range map[string][]string{
		// A shell inside a cmux-launched claude inherits its launch variables.
		"another process's launch": launchedEnv(entry, 1111),
		"another executable":       launchedEnv("/Users/me/.local/bin/claude-cmux", 4242),
		"outside cmux":             slices.DeleteFunc(launchedEnv(entry, 4242), func(kv string) bool { return strings.HasPrefix(kv, "CMUX_SURFACE_ID=") }),
	} {
		if _, ok := LaunchedByCmux(entry, nil, env, 4242); ok {
			t.Fatalf("%s: taken for a cmux launch", name)
		}
	}
}

// cmux records the launch to restore from SessionStart alone; other hooks
// may start claude through CMUX_CUSTOM_CLAUDE_PATH, which stays the user's.
func TestOnlySessionStartNamesTheEntry(t *testing.T) {
	c := Captured{Env: map[string]string{"CMUX_SURFACE_ID": "surface-1", "CMUX_CUSTOM_CLAUDE_PATH": "/Users/me/.local/bin/rclaude"}}
	if got := c.HookEnv([]string{"hooks", "claude", "session-start"}); got["CMUX_CUSTOM_CLAUDE_PATH"] != "/Users/me/.local/bin/rclaude" || got["CMUX_SURFACE_ID"] != "surface-1" {
		t.Fatalf("session-start env = %v", got)
	}
	for _, argv := range [][]string{{"hooks", "claude", "stop"}, {"hooks", "claude", "auto-name"}, {"hooks", "feed", "--source", "claude"}} {
		got := c.HookEnv(argv)
		if _, ok := got["CMUX_CUSTOM_CLAUDE_PATH"]; ok || got["CMUX_SURFACE_ID"] != "surface-1" {
			t.Fatalf("%q env = %v", argv, got)
		}
	}
	if c.Env["CMUX_CUSTOM_CLAUDE_PATH"] == "" {
		t.Fatal("HookEnv changed the captured env")
	}
}

func TestCaptureOutsideCmux(t *testing.T) {
	wrapper := fakeWrapper(t, `exec "$CMUX_CUSTOM_CLAUDE_PATH" "$@"`)
	got, err := Capture(context.Background(), wrapper, self(t), []string{"--resume"}, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got.InCmux() || !slices.Equal(got.Args, []string{"--resume"}) {
		t.Fatalf("captured = %+v", got)
	}
}

func TestCaptureWrapperFailure(t *testing.T) {
	wrapper := fakeWrapper(t, `echo "Error: claude not found in PATH" >&2; exit 127`)
	_, err := Capture(context.Background(), wrapper, self(t), nil, []string{"PATH=/usr/bin:/bin"})
	if err == nil || !strings.Contains(err.Error(), "claude not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestCaptureWrapperThatDoesNotExecUs(t *testing.T) {
	wrapper := fakeWrapper(t, `exit 0`)
	if _, err := Capture(context.Background(), wrapper, self(t), nil, []string{"PATH=/usr/bin:/bin"}); err == nil {
		t.Fatal("expected error when the wrapper never runs the capture")
	}
}

func TestHookArgvs(t *testing.T) {
	c := Captured{Args: []string{"--settings", `{"preferredNotifChannel":"x","hooks":{
		"Stop":[{"matcher":"","hooks":[{"type":"command","command":"\"${CMUX_CLAUDE_HOOK_CMUX_BIN:-cmux}\" hooks claude stop"},
		                                {"type":"command","command":"\"${CMUX_CLAUDE_HOOK_CMUX_BIN:-cmux}\" hooks feed --source claude"}]}],
		"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"my-own-hook --check"}]}]}}`, "--session-id", "x"}}
	got, err := c.HookArgvs()
	if err != nil {
		t.Fatalf("HookArgvs: %v", err)
	}
	want := map[string]bool{"hooks claude stop": true, "hooks feed --source claude": true}
	if len(got) != len(want) {
		t.Fatalf("argvs = %q", got)
	}
	for _, a := range got {
		if !want[strings.Join(a, " ")] {
			t.Fatalf("unexpected argv %q", a)
		}
	}
	if _, err := (Captured{Args: []string{"--resume"}}).HookArgvs(); err == nil {
		t.Fatal("expected error without --settings")
	}
}

func TestHookArgvsFromRealCmuxSettings(t *testing.T) {
	raw, err := os.ReadFile("testdata/cmux-1.3.2-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Captured{Args: []string{"--settings", strings.TrimSpace(string(raw))}}.HookArgvs()
	if err != nil {
		t.Fatalf("HookArgvs: %v", err)
	}
	seen := map[string]bool{}
	for _, a := range got {
		seen[strings.Join(a, " ")] = true
	}
	for _, want := range []string{"hooks claude session-start", "hooks claude pre-tool-use", "hooks feed --source claude", "hooks claude cron-create-guard"} {
		if !seen[want] {
			t.Fatalf("missing %q in %v", want, got)
		}
	}
}

func TestHookArgvsRefusesShellSyntax(t *testing.T) {
	c := Captured{Args: []string{"--settings", `{"hooks":{"Stop":[{"hooks":[{"command":"\"${CMUX_CLAUDE_HOOK_CMUX_BIN:-cmux}\" hooks claude stop || true"}]}]}}`}}
	if _, err := c.HookArgvs(); err == nil {
		t.Fatal("expected refusal of a command with shell syntax")
	}
}
