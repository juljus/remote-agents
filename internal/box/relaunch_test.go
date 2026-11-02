package box

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/jsonfile"
	"remote-agents/internal/reaper"
)

const commandSession = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func TestAFirstLaunchStartsItsCommand(t *testing.T) {
	l := testLayout(t)
	s := claudeSpec("/w")
	argv := slices.Clone(s.Argv)
	if err := resumeLostLaunch(l, &s, noLog); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Argv, argv) {
		t.Fatalf("argv = %q, want %q", s.Argv, argv)
	}
}

func TestARelaunchAfterALostLaunchResumesTheSessionItsHooksReported(t *testing.T) {
	l := testLayout(t)
	s := claudeSpec("/w")
	if err := markStarted(l, s.RunID); err != nil {
		t.Fatal(err)
	}
	// /clear had moved the agent on to another session.
	const cleared = "cccccccc-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if err := os.MkdirAll(l.ActivityDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := jsonfile.Write(l.HookState(s.Session), reaper.HookState{ClaudeSession: cleared}); err != nil {
		t.Fatal(err)
	}
	var logged []string
	if err := resumeLostLaunch(l, &s, recordLog(&logged)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Argv, []string{"claude", "--dangerously-skip-permissions", "--resume", cleared}) {
		t.Fatalf("argv = %q", s.Argv)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], cleared) {
		t.Fatalf("logged %q", logged)
	}
}

func TestARelaunchWithoutHookReportsResumesTheSessionItsCommandStarted(t *testing.T) {
	l := testLayout(t)
	s := claudeSpec("/w")
	if err := markStarted(l, s.RunID); err != nil {
		t.Fatal(err)
	}
	if err := resumeLostLaunch(l, &s, noLog); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Argv, []string{"claude", "--dangerously-skip-permissions", "--resume", commandSession}) {
		t.Fatalf("argv = %q", s.Argv)
	}
}

func TestARelaunchOfAPickerRunWithoutHookReportsStartsItsCommand(t *testing.T) {
	l := testLayout(t)
	s := claudeSpec("/w")
	s.Argv = []string{"claude", "--continue"}
	if err := markStarted(l, s.RunID); err != nil {
		t.Fatal(err)
	}
	var logged []string
	if err := resumeLostLaunch(l, &s, recordLog(&logged)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Argv, []string{"claude", "--continue"}) || len(logged) != 1 {
		t.Fatalf("argv = %q, logged %q", s.Argv, logged)
	}
}

func TestLaunchRecordsTheRunsStart(t *testing.T) {
	l := testLayout(t)
	if code, out := launch(t, l, sessionSpec(t.TempDir(), "/bin/sh", "-c", "exit 0")); code != 0 {
		t.Fatalf("launch = %d %q", code, out)
	}
	if started, err := launchStarted(l, runID); err != nil || !started {
		t.Fatalf("started = %v, %v", started, err)
	}
}

func TestClaudeSessionsDirFollowsClaudeConfigDir(t *testing.T) {
	l := boxlayout.Layout{Home: "/home/agent", Name: boxlayout.DefaultName}
	if got := claudeSessionsDir(l, []string{"HOME=/home/agent"}); got != "/home/agent/.claude/sessions" {
		t.Fatalf("default = %q", got)
	}
	if got := claudeSessionsDir(l, []string{"CLAUDE_CONFIG_DIR=/cfg"}); got != "/cfg/sessions" {
		t.Fatalf("with CLAUDE_CONFIG_DIR = %q", got)
	}
}

const heldSession = "dddddddd-bbbb-4ccc-8ddd-eeeeeeeeeeee"

// liveProcs has processes 10 and 11 running, both started at tick 100.
var liveProcs = fstest.MapFS{
	"10/stat":    &fstest.MapFile{Data: []byte("10 (claude) S 1 0 1 " + strings.Repeat("0 ", 11) + "20 0 1 0 100")},
	"10/cmdline": &fstest.MapFile{},
	"11/stat":    &fstest.MapFile{Data: []byte("11 (claude) S 1 0 1 " + strings.Repeat("0 ", 11) + "20 0 1 0 100")},
	"11/cmdline": &fstest.MapFile{},
}

func register(t *testing.T, dir string, pid int, session, procStart string) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("%d.json", pid))
	body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":"/w","procStart":%q,"status":"idle"}`, pid, session, procStart)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAwaitReleaseIgnoresClaudesThatDoNotHoldTheSession(t *testing.T) {
	dir := t.TempDir()
	register(t, dir, 10, commandSession, "100")
	register(t, dir, 11, heldSession, "99")
	register(t, dir, 12, heldSession, "100")
	if err := os.WriteFile(filepath.Join(dir, "10."+strings.Repeat("0", 64)+".key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged []string
	if err := awaitRelease(dir, liveProcs, heldSession, time.Second, recordLog(&logged)); err != nil || len(logged) != 0 {
		t.Fatalf("await = %v, logged %q", err, logged)
	}
	if err := awaitRelease(filepath.Join(dir, "missing"), liveProcs, heldSession, time.Second, noLog); err != nil {
		t.Fatalf("no registry: %v", err)
	}
}

func TestAwaitReleaseWaitsForTheHolderToExit(t *testing.T) {
	dir := t.TempDir()
	path := register(t, dir, 10, heldSession, "100")
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Remove(path)
	}()
	var logged []string
	start := time.Now()
	if err := awaitRelease(dir, liveProcs, heldSession, 5*time.Second, recordLog(&logged)); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("returned while the holder still ran")
	}
	if len(logged) != 2 || !strings.Contains(logged[0], "pid 10") {
		t.Fatalf("logged %q", logged)
	}
}

func TestAwaitReleaseFailsNamingAHolderThatStays(t *testing.T) {
	dir := t.TempDir()
	register(t, dir, 10, heldSession, "100")
	var logged []string
	err := awaitRelease(dir, liveProcs, heldSession, 300*time.Millisecond, recordLog(&logged))
	if err == nil || !strings.Contains(err.Error(), "pid 10") || !strings.Contains(err.Error(), heldSession) {
		t.Fatalf("await = %v", err)
	}
	if len(logged) != 2 || !strings.Contains(logged[1], "pid 10") {
		t.Fatalf("logged %q", logged)
	}
}

func TestAwaitReleaseRejectsARegistrationWithoutAStartTime(t *testing.T) {
	dir := t.TempDir()
	register(t, dir, 10, heldSession, "")
	if err := awaitRelease(dir, liveProcs, heldSession, time.Second, noLog); err == nil {
		t.Fatal("a registration that cannot be told from a reused pid was accepted")
	}
}

func noLog(string, ...any) {}

func recordLog(lines *[]string) func(string, ...any) {
	return func(format string, args ...any) { *lines = append(*lines, fmt.Sprintf(format, args...)) }
}
