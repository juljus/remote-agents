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
	"remote-agents/internal/reaper"
	"remote-agents/internal/spec"
)

func TestShpoolAttachArgv(t *testing.T) {
	l := boxlayout.Layout{Home: "/home/agent", Name: boxlayout.DefaultName}
	argv := shpoolAttachArgv(l, "claude-abc", "TOKEN", "remote-agents-claude-abc-k3.scope", 2<<30)
	want := []string{
		"/home/agent/.local/share/remote-agents/bin/shpool",
		"--socket", "/home/agent/.local/state/remote-agents/shpool.sock",
		"--config-file", "/home/agent/.config/remote-agents/shpool.toml",
		"--no-daemonize",
		"attach", "--force",
		"--cmd", "/usr/bin/systemd-run --user --scope --quiet --collect --unit=remote-agents-claude-abc-k3.scope" +
			" --description=claude-abc" +
			" --property=MemoryMax=2147483648 --property=MemorySwapMax=0 --property=OOMPolicy=continue" +
			" --property=BindsTo=remote-agents-shpool.service --property=StopPropagatedFrom=remote-agents-shpool.service" +
			" --property=After=remote-agents-shpool.service --property=TimeoutStopSec=10s" +
			" /home/agent/.local/share/remote-agents/bin/remote-agents-box launch TOKEN",
		"claude-abc",
	}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", argv, want)
	}
}

func TestFormatSessions(t *testing.T) {
	raw := []byte(`{"sessions":[
	 {"name":"claude-b","started_at_unix_ms":1761300000000,"last_connected_at_unix_ms":1761300000000,"last_disconnected_at_unix_ms":1761303000000,"status":"Disconnected","shell_pid":12,"attachments":[]},
	 {"name":"claude-a","started_at_unix_ms":1761290000000,"last_connected_at_unix_ms":1761290000000,"last_disconnected_at_unix_ms":null,"status":"Attached","shell_pid":11,"attachments":[{"x":1}]},
	 {"name":"claude-c","started_at_unix_ms":1761295000000,"status":"Disconnected"}
	]}`)
	sessions, err := parseSessions(raw)
	if err != nil {
		t.Fatalf("parseSessions: %v", err)
	}
	now := time.UnixMilli(1761303000000).Add(2*time.Hour + 5*time.Minute)
	statuses := map[string]reaper.Status{
		"claude-a": {Working: []string{"turn", "tool"}},
		"claude-b": {IdleSince: now.Add(-3 * time.Minute).UnixMilli()},
	}
	var out bytes.Buffer
	if err := formatSessions(&out, sessions, statuses, now, time.UTC); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := [][]string{
		{"SESSION", "STATUS", "ACTIVITY", "DETACHED", "STARTED"},
		{"claude-a", "attached", "working:", "turn,", "tool", "-"},
		{"claude-c", "detached", "-", "4h18m"},
		{"claude-b", "detached", "idle", "3m", "2h05m"},
	}
	if len(lines) != len(want) {
		t.Fatalf("output:\n%s", out.String())
	}
	for i, w := range want {
		if f := strings.Fields(lines[i]); len(f) < len(w) || !slices.Equal(f[:len(w)], w) {
			t.Fatalf("line %d = %q, want prefix %q\n%s", i, lines[i], w, out.String())
		}
	}
}

func TestFormatReapedShowsRecentReapsWithTheirResumeCommand(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	rec := func(name, id string, r reaper.Reason, ago time.Duration) reapedSessionRecord {
		s := spec.Spec{Session: name, Claude: &spec.Agent{Resume: []string{"claude"}}}
		if strings.HasPrefix(name, "codex-") {
			s.Claude, s.Codex = nil, &spec.Agent{Resume: []string{"codex"}}
		}
		return reapedSessionRecord{Reason: r, AgentSession: id, At: now.Add(-ago).UnixMilli(), Spec: s}
	}
	var out bytes.Buffer
	err := formatReaped(&out, []reapedSessionRecord{
		rec("claude-11111111-bbbb-4ccc-8ddd-eeeeeeeeeeee", "aaa", reaper.ReapedIdle, time.Hour),
		rec("fakeclaude-22222222-bbbb-4ccc-8ddd-eeeeeeeeeeee", "bbb", reaper.ReapedCap, 2*time.Hour),
		rec("claude-33333333-bbbb-4ccc-8ddd-eeeeeeeeeeee", "ccc", reaper.ReapedIdle, 8*24*time.Hour),
		rec("codex-44444444-bbbb-4ccc-8ddd-eeeeeeeeeeee", "ddd", reaper.ReapedIdle, 3*time.Hour),
	}, now, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"REAPED", "2026-10-27 11:00  idle               rclaude --resume aaa", "detached too long  rfakeclaude --resume bbb", "idle               rcodex resume ddd"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--resume ccc") {
		t.Fatalf("a reap older than a week is listed:\n%s", got)
	}
	out.Reset()
	if err := formatReaped(&out, nil, now, time.UTC); err != nil || out.Len() != 0 {
		t.Fatalf("no reaps printed %q, %v", out.String(), err)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0s", 59 * time.Second: "59s", 61 * time.Second: "1m", 59 * time.Minute: "59m",
		time.Hour: "1h00m", 36*time.Hour + 7*time.Minute: "36h07m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestParseSessionsRejectsUnknownStatus(t *testing.T) {
	if _, err := parseSessions([]byte(`{"sessions":[{"name":"x","status":"Weird"}]}`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestListingAWedgedDaemonFailsNamingTheTimeout(t *testing.T) {
	l := testLayout(t)
	if err := os.MkdirAll(filepath.Dir(l.Shpool()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Shpool(), []byte("#!/bin/sh\nsleep 5 2>/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := listSessionsWithin(l, 200*time.Millisecond)
	if err == nil || err.Error() != "shpool list: no answer within 200ms" {
		t.Fatalf("list = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("list returned after %s", time.Since(start))
	}
}
