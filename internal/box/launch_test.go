package box

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/spec"
)

const runID = "11111111-2222-4333-8444-555555555555"

func testLayout(t *testing.T) boxlayout.Layout {
	t.Helper()
	return boxlayout.Layout{Home: t.TempDir(), Name: boxlayout.DefaultName}
}

func sessionSpec(workdir string, argv ...string) spec.Spec {
	return spec.Spec{
		Mode:    spec.ModeSession,
		Session: "claude-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		RunID:   runID,
		Workdir: workdir,
		Argv:    argv,
		Env:     map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"},
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestAgentEnv(t *testing.T) {
	l := boxlayout.Layout{Home: "/home/agent", Name: boxlayout.DefaultName}
	s := sessionSpec("/w", "claude")
	s.HookRelay = true
	env, err := AgentEnv([]string{"HOME=/home/agent", "PATH=/usr/bin", "TERM=dumb", "SHPOOL_SESSION_NAME=x"}, l, s)
	if err != nil {
		t.Fatalf("AgentEnv: %v", err)
	}
	m := envMap(env)
	if !strings.HasPrefix(m["PATH"], "/home/agent/.local/share/remote-agents/shims:/home/agent/.local/bin:") {
		t.Fatalf("PATH = %q", m["PATH"])
	}
	if m["BROWSER"] != "/home/agent/.local/share/remote-agents/shims/xdg-open" {
		t.Fatalf("BROWSER = %q", m["BROWSER"])
	}
	if m["TERM"] != "xterm-256color" || m["COLORTERM"] != "truecolor" {
		t.Fatalf("terminal env not applied: %v", m)
	}
	if m[SessionEnvVar] != s.Session {
		t.Fatalf("%s = %q", SessionEnvVar, m[SessionEnvVar])
	}
	if m["CMUX_CLAUDE_HOOK_CMUX_BIN"] != "/home/agent/.local/share/remote-agents/hookbin/cmux" {
		t.Fatalf("hook bin = %q", m["CMUX_CLAUDE_HOOK_CMUX_BIN"])
	}
	if m["HOME"] != "/home/agent" || m["SHPOOL_SESSION_NAME"] != "x" {
		t.Fatalf("base env lost: %v", m)
	}
	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "TERM=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("TERM appears %d times", count)
	}
}

func TestAgentEnvWithoutHookRelay(t *testing.T) {
	env, err := AgentEnv(nil, boxlayout.Layout{Home: "/h", Name: boxlayout.DefaultName}, sessionSpec("/w", "claude"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envMap(env)["CMUX_CLAUDE_HOOK_CMUX_BIN"]; ok {
		t.Fatal("hook bin set without hook relay")
	}
}

func TestAgentEnvRejectsReservedKeys(t *testing.T) {
	s := sessionSpec("/w", "claude")
	s.Env["PATH"] = "/evil"
	if _, err := AgentEnv(nil, boxlayout.Layout{Home: "/h", Name: boxlayout.DefaultName}, s); err == nil {
		t.Fatal("expected error for reserved key")
	}
}

func TestLookPath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := lookPath("agent", "/nonexistent:"+dir)
	if err != nil || got != exe {
		t.Fatalf("lookPath = %q, %v", got, err)
	}
	if _, err := lookPath("missing", dir); err == nil {
		t.Fatal("expected not found")
	}
	if got, err := lookPath(exe, ""); err != nil || got != exe {
		t.Fatalf("absolute lookPath = %q, %v", got, err)
	}
}

func launch(t *testing.T, l boxlayout.Layout, s spec.Spec) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := Launch(l, s, IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out}, []string{"PATH=/usr/bin:/bin", "HOME=" + l.Home})
	return code, out.String()
}

func TestLaunchRunsAgentAndRecordsExit(t *testing.T) {
	l := testLayout(t)
	work := t.TempDir()
	code, out := launch(t, l, sessionSpec(work, "/bin/sh", "-c", `pwd; printf "%s\n" "$REMOTE_AGENTS_SESSION"; exit 7`))
	if code != 7 {
		t.Fatalf("code = %d, output %q", code, out)
	}
	resolvedWork, _ := filepath.EvalSymlinks(work)
	if !strings.Contains(out, resolvedWork) || !strings.Contains(out, "claude-aaaaaaaa") {
		t.Fatalf("agent did not run in workdir with session env: %q", out)
	}
	if _, err := os.Stat(l.EndedMarker(runID)); err != nil {
		t.Fatalf("ended marker missing: %v", err)
	}
	// A reconnect after the run ended must not start the agent again.
	code, out = launch(t, l, sessionSpec(work, "/bin/sh", "-c", "echo SHOULD-NOT-RUN"))
	if code != 7 || strings.Contains(out, "SHOULD-NOT-RUN") || !strings.Contains(out, "ended") {
		t.Fatalf("relaunch = %d %q", code, out)
	}
	// Every later reconnect of the same run keeps getting the same answer.
	code, out = launch(t, l, sessionSpec(work, "/bin/sh", "-c", "echo SHOULD-NOT-RUN"))
	if code != 7 || strings.Contains(out, "SHOULD-NOT-RUN") {
		t.Fatalf("second relaunch = %d %q", code, out)
	}
}

func TestLaunchReportsSignalDeath(t *testing.T) {
	l := testLayout(t)
	code, _ := launch(t, l, sessionSpec(t.TempDir(), "/bin/sh", "-c", "kill -TERM $$"))
	if code != 128+15 {
		t.Fatalf("code = %d, want 143", code)
	}
}

func TestLaunchMissingWorkdirFailsLoudly(t *testing.T) {
	l := testLayout(t)
	code, out := launch(t, l, sessionSpec("/nonexistent/work", "/bin/true"))
	if code == 0 || !strings.Contains(out, "/nonexistent/work") {
		t.Fatalf("launch = %d %q", code, out)
	}
}

func TestLaunchReattachSpecNeverRuns(t *testing.T) {
	l := testLayout(t)
	code, out := launch(t, l, spec.Spec{Mode: spec.ModeReattach, Session: "claude-x"})
	if code == 0 || !strings.Contains(out, "claude-x") {
		t.Fatalf("launch = %d %q", code, out)
	}
}

func TestPruneOldMarkers(t *testing.T) {
	l := testLayout(t)
	if err := os.MkdirAll(l.EndedDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	old := l.EndedMarker("old")
	fresh := l.EndedMarker("fresh")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte(`{"exit_code":0}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := pruneOld(l.EndedDir(), time.Now()); err != nil {
		t.Fatalf("pruneOld: %v", err)
	}
	entries, _ := os.ReadDir(l.EndedDir())
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"fresh.json"}) {
		t.Fatalf("remaining markers = %v", names)
	}
}
