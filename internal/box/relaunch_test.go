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
	"remote-agents/internal/spec"
)

const commandSession = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func TestAFirstLaunchStartsItsCommand(t *testing.T) {
	l := testLayout(t)
	s := claudeSpec("/w")
	argv := slices.Clone(s.Argv)
	rec, lost, err := resumeLostLaunch(l, &s, noLog)
	if err != nil {
		t.Fatal(err)
	}
	if lost || !slices.Equal(s.Argv, argv) || rec != (runRecord{}) {
		t.Fatalf("argv = %q, record %+v; want %q and none", s.Argv, rec, argv)
	}
}

// lostLaunch leaves what a launcher killed with its agent leaves: the run's
// record and, unless hooks is empty, the hook state.
func lostLaunch(t *testing.T, l boxlayout.Layout, s spec.Spec, rec runRecord, hooks reaper.HookState) {
	t.Helper()
	if err := writeRunRecord(l, s.RunID, rec); err != nil {
		t.Fatal(err)
	}
	if hooks == (reaper.HookState{}) {
		return
	}
	if err := os.MkdirAll(l.ActivityDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := jsonfile.Write(l.HookState(s.Session), hooks); err != nil {
		t.Fatal(err)
	}
}

func transcriptIn(t *testing.T, dir, id string) string {
	t.Helper()
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const cleared = "cccccccc-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func TestARelaunchResumesTheSessionTheHooksLastReported(t *testing.T) {
	l := testLayout(t)
	s := claudeSpec("/w")
	dir := t.TempDir()
	// /clear had moved the agent on to another session, which has had a turn since.
	hooks := reaper.HookState{ClaudeSession: cleared, Transcript: transcriptIn(t, dir, cleared)}
	lostLaunch(t, l, s, runRecord{ClaudeSession: commandSession, Transcript: transcriptIn(t, dir, commandSession)}, hooks)
	var logged []string
	rec, lost, err := resumeLostLaunch(l, &s, recordLog(&logged))
	if err != nil || !lost {
		t.Fatal(err)
	}
	if !slices.Equal(s.Argv, []string{"claude", "--dangerously-skip-permissions", "--resume", cleared}) {
		t.Fatalf("argv = %q", s.Argv)
	}
	if rec != (runRecord{ClaudeSession: cleared, Transcript: hooks.Transcript}) {
		t.Fatalf("record = %+v", rec)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], cleared) {
		t.Fatalf("logged %q", logged)
	}
}

func TestARelaunchStartsAReportedSessionWithoutATranscriptAfresh(t *testing.T) {
	l := testLayout(t)
	s := claudeSpec("/w")
	// Claude writes a transcript only once a turn starts.
	missing := filepath.Join(t.TempDir(), cleared+".jsonl")
	lostLaunch(t, l, s, runRecord{}, reaper.HookState{ClaudeSession: cleared, Transcript: missing})
	if _, lost, err := resumeLostLaunch(l, &s, noLog); err != nil || !lost {
		t.Fatal(err)
	}
	if !slices.Equal(s.Argv, []string{"claude", "--dangerously-skip-permissions", "--session-id", cleared}) {
		t.Fatalf("argv = %q", s.Argv)
	}
}

func TestARelaunchWithoutHookStateFollowsTheRunRecord(t *testing.T) {
	l := testLayout(t)
	s := claudeSpec("/w")
	// The previous relaunch died too, before its agent reported a session.
	want := runRecord{ClaudeSession: cleared, Transcript: transcriptIn(t, t.TempDir(), cleared)}
	lostLaunch(t, l, s, want, reaper.HookState{})
	rec, lost, err := resumeLostLaunch(l, &s, noLog)
	if err != nil || !lost {
		t.Fatal(err)
	}
	if !slices.Equal(s.Argv, []string{"claude", "--dangerously-skip-permissions", "--resume", cleared}) || rec != want {
		t.Fatalf("argv = %q, record %+v", s.Argv, rec)
	}
}

func TestARelaunchBeforeAnySessionWasReportedRunsItsCommandAgain(t *testing.T) {
	l := testLayout(t)
	s := claudeSpec("/w")
	argv := slices.Clone(s.Argv)
	lostLaunch(t, l, s, runRecord{}, reaper.HookState{})
	// Not lost as far as resuming goes: the launch may still follow a reap, whose resume then applies.
	var logged []string
	if _, lost, err := resumeLostLaunch(l, &s, recordLog(&logged)); err != nil || lost {
		t.Fatalf("lost = %v, %v", lost, err)
	}
	if !slices.Equal(s.Argv, argv) || len(logged) != 1 {
		t.Fatalf("argv = %q, logged %q", s.Argv, logged)
	}
}

func TestTheWatcherKeepsTheRunRecordCurrent(t *testing.T) {
	l := testLayout(t)
	w := &watcher{l: l, session: "claude-abc", runID: runID}
	read := func() runRecord {
		t.Helper()
		rec, found, err := readRunRecord(l, runID)
		if err != nil || !found {
			t.Fatalf("record %v, %v", found, err)
		}
		return rec
	}
	if err := w.keepRecord(reaper.HookState{}); err != nil || read() != (runRecord{}) {
		t.Fatalf("before any report: %+v, %v", read(), err)
	}
	reported := runRecord{ClaudeSession: cleared, Transcript: "/t/" + cleared + ".jsonl"}
	if err := w.keepRecord(reaper.HookState{ClaudeSession: reported.ClaudeSession, Transcript: reported.Transcript}); err != nil || read() != reported {
		t.Fatalf("after a report: %+v, %v", read(), err)
	}
	old := time.Now().Add(-markerTTL - time.Hour)
	if err := os.Chtimes(l.RunRecord(runID), old, old); err != nil {
		t.Fatal(err)
	}
	if err := w.keepRecord(reaper.HookState{}); err != nil || read() != reported {
		t.Fatalf("hook state cleared: %+v, %v", read(), err)
	}
	if info, err := os.Stat(l.RunRecord(runID)); err != nil || time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("record not kept fresh against pruning: %v, %v", info, err)
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

func TestAwaitReleaseRereadsARegistrationCaughtMidWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "11.json")
	if err := os.WriteFile(path, []byte(`{"pid":11,"sess`), 0o600); err != nil {
		t.Fatal(err)
	}
	written := `{"pid":11,"sessionId":"` + commandSession + `","procStart":"100"}`
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.WriteFile(path, []byte(written), 0o600)
	}()
	if err := awaitRelease(dir, liveProcs, heldSession, 5*time.Second, noLog); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitReleaseSkipsAnUnreadableRegistrationOfAnExitedClaude(t *testing.T) {
	dir := t.TempDir()
	// Killed while it rewrote its registration: nothing removes the file.
	if err := os.WriteFile(filepath.Join(dir, "12.json"), []byte(`{"pid":12,"sess`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := awaitRelease(dir, liveProcs, heldSession, time.Second, noLog); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitReleaseFailsOnARegistrationThatStaysUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "11.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := awaitRelease(dir, liveProcs, heldSession, 300*time.Millisecond, noLog); err == nil || !strings.Contains(err.Error(), "11.json") {
		t.Fatalf("await = %v", err)
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
