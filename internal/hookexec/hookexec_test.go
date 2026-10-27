package hookexec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"remote-agents/internal/linkproto"
)

func fakeCmux(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cmux")
	script := "#!/bin/sh\necho \"args=$* surface=$CMUX_SURFACE_ID pid=$CMUX_CLAUDE_PID home=$HOME\"\ncat\necho problem >&2\nexit 3\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixedEnv(env map[string]string) func([]string) map[string]string {
	return func([]string) map[string]string { return env }
}

func TestRunsHookWithCmuxEnv(t *testing.T) {
	e := Executor{
		BaseEnv: []string{"HOME=/Users/me", "CMUX_SURFACE_ID=stale"},
		Env:     fixedEnv(map[string]string{BinEnvVar: fakeCmux(t), "CMUX_SURFACE_ID": "surface-1", "CMUX_CLAUDE_PID": "4242"}),
		Allowed: [][]string{{"hooks", "claude", "stop"}},
	}
	res, err := e.Run(context.Background(), linkproto.HookRequest{Argv: []string{"hooks", "claude", "stop"}, Stdin: []byte(`{"x":1}`)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "args=hooks claude stop surface=surface-1 pid=4242 home=/Users/me\n{\"x\":1}"
	if string(res.Stdout) != want || strings.TrimSpace(string(res.Stderr)) != "problem" || res.ExitCode != 3 {
		t.Fatalf("result = %q / %q / %d", res.Stdout, res.Stderr, res.ExitCode)
	}
}

func TestNeedsAbsoluteBin(t *testing.T) {
	e := Executor{Env: fixedEnv(map[string]string{BinEnvVar: "cmux"}), Allowed: [][]string{{"hooks"}}}
	if _, err := e.Run(context.Background(), linkproto.HookRequest{Argv: []string{"hooks"}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestRefusesArgvCmuxDidNotInject(t *testing.T) {
	e := Executor{Env: fixedEnv(map[string]string{BinEnvVar: fakeCmux(t)}), Allowed: [][]string{{"hooks", "claude", "stop"}}}
	for _, argv := range [][]string{
		{"send", "rm -rf ~\n"},
		{"hooks", "setup"},
		{"hooks", "claude", "stop", "--extra"},
		{"hooks", "claude"},
	} {
		if _, err := e.Run(context.Background(), linkproto.HookRequest{Argv: argv}); err == nil {
			t.Fatalf("argv %q should be refused", argv)
		}
	}
}
