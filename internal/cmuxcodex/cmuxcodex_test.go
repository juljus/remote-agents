package cmuxcodex

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"remote-agents/internal/linkproto"
)

func TestOutsideCmuxNothingIsRelayed(t *testing.T) {
	ex, err := NewExecutor([]string{"HOME=/h", "CMUX_BUNDLED_CLI_PATH=/c"}, 42)
	if err != nil || ex != nil {
		t.Fatalf("executor = %+v, %v; want none", ex, err)
	}
}

func TestInCmuxNeedsCmuxsCLI(t *testing.T) {
	if _, err := NewExecutor([]string{"CMUX_SURFACE_ID=s1"}, 42); err == nil || !strings.Contains(err.Error(), "CMUX_BUNDLED_CLI_PATH") {
		t.Fatalf("err = %v", err)
	}
}

// The relay runs cmux's Codex hooks as if the wrapper were Codex, and drops
// the launch an outer agent described, which is not this one.
func TestRelayedHooksRunAsTheWrappersCodex(t *testing.T) {
	cli := filepath.Join(t.TempDir(), "cmux")
	script := "#!/bin/sh\necho \"args=$* surface=$CMUX_SURFACE_ID pid=$CMUX_CODEX_PID kind=${CMUX_AGENT_LAUNCH_KIND:-none} custom=${CMUX_CUSTOM_CLAUDE_PATH:-none} home=$HOME\"\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	environ := []string{"HOME=/h", "CMUX_SURFACE_ID=s1", "CMUX_BUNDLED_CLI_PATH=" + cli, "CMUX_AGENT_LAUNCH_KIND=claude",
		"CMUX_AGENT_LAUNCH_ARGV_B64=x", "CMUX_CUSTOM_CLAUDE_PATH=/c", "CMUX_CLAUDE_PID=7"}
	ex, err := NewExecutor(environ, 42)
	if err != nil || ex == nil {
		t.Fatalf("executor = %+v, %v", ex, err)
	}
	if !slices.EqualFunc(ex.Allowed, Argvs(), slices.Equal) {
		t.Fatalf("allowed = %q", ex.Allowed)
	}
	res, err := ex.Run(context.Background(), linkproto.HookRequest{Argv: []string{"hooks", "codex", "stop"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "args=hooks codex stop surface=s1 pid=42 kind=none custom=none home=/h\n"; string(res.Stdout) != want {
		t.Fatalf("hook ran as %q, want %q", res.Stdout, want)
	}
	if _, err := ex.Run(context.Background(), linkproto.HookRequest{Argv: []string{"hooks", "claude", "stop"}}); err == nil {
		t.Fatal("ran a hook cmux does not give Codex")
	}
}
