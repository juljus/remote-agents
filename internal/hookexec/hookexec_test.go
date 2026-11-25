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
		Allowed: []Hook{{Argv: []string{"hooks", "claude", "stop"}}},
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
	e := Executor{Env: fixedEnv(map[string]string{BinEnvVar: "cmux"}), Allowed: []Hook{{Argv: []string{"hooks"}}}}
	if _, err := e.Run(context.Background(), linkproto.HookRequest{Argv: []string{"hooks"}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestRefusesArgvCmuxDidNotInject(t *testing.T) {
	e := Executor{Env: fixedEnv(map[string]string{BinEnvVar: fakeCmux(t)}), Allowed: []Hook{{Argv: []string{"hooks", "claude", "stop"}}}}
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

// cmux's own hook commands answer "{}" when its CLI fails; quiet hooks do too.
func TestQuietHookHidesFailure(t *testing.T) {
	e := Executor{
		Env: fixedEnv(map[string]string{BinEnvVar: fakeCmux(t)}),
		Allowed: []Hook{
			{Argv: []string{"hooks", "enqueue", "claude", "stop"}, Quiet: true},
			{Argv: []string{"hooks", "claude", "inbox-drain"}, Quiet: true},
		},
	}
	res, err := e.Run(context.Background(), linkproto.HookRequest{Argv: []string{"hooks", "enqueue", "claude", "stop"}})
	if err != nil || string(res.Stdout) != "{}\n" || len(res.Stderr) != 0 || res.ExitCode != 0 {
		t.Fatalf("result = %q / %q / %d, err %v", res.Stdout, res.Stderr, res.ExitCode, err)
	}
	e.Env = fixedEnv(map[string]string{BinEnvVar: "cmux"})
	res, err = e.Run(context.Background(), linkproto.HookRequest{Argv: []string{"hooks", "claude", "inbox-drain"}})
	if err != nil || string(res.Stdout) != "{}\n" {
		t.Fatalf("unrunnable quiet hook: %q, err %v", res.Stdout, err)
	}
}

func TestQuietHookPassesSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho \"timeout=$CMUXTERM_CLI_RESPONSE_TIMEOUT_SEC\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := Executor{
		Env:     fixedEnv(map[string]string{BinEnvVar: path}),
		Allowed: []Hook{{Argv: []string{"hooks", "enqueue", "claude", "stop"}, Env: map[string]string{"CMUXTERM_CLI_RESPONSE_TIMEOUT_SEC": "0.5"}, Quiet: true}},
	}
	res, err := e.Run(context.Background(), linkproto.HookRequest{Argv: []string{"hooks", "enqueue", "claude", "stop"}})
	if err != nil || string(res.Stdout) != "timeout=0.5\n" || res.ExitCode != 0 {
		t.Fatalf("result = %q / %d, err %v", res.Stdout, res.ExitCode, err)
	}
}
