package box

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestActivitySettingsRunTheBoxBinaryForEveryEvent(t *testing.T) {
	l := boxlayout.Layout{Home: "/home/agent", Name: "remote-agents-e2e"}
	argv, err := withActivityHooks(l, []string{"claude", "--model", "x"}, "/home/agent/work")
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 5 || argv[0] != "claude" || argv[1] != "--settings" || !slices.Equal(argv[3:], []string{"--model", "x"}) {
		t.Fatalf("argv = %q", argv)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal([]byte(argv[2]), &settings); err != nil {
		t.Fatal(err)
	}
	for _, event := range reaper.HookEvents {
		m := settings.Hooks[event]
		if len(m) != 1 || len(m[0].Hooks) != 1 || m[0].Hooks[0].Type != "command" ||
			m[0].Hooks[0].Command != activityCommand {
			t.Fatalf("%s hooks = %+v", event, m)
		}
	}
	if len(settings.Hooks) != len(reaper.HookEvents) {
		t.Fatalf("hooks = %+v", settings.Hooks)
	}
}

const activityCommand = "'/home/agent/.local/share/remote-agents-e2e/bin/remote-agents-box' activity"

// cmuxSettings is the shape of the --settings cmux's wrapper passes.
const cmuxSettings = `{"preferredNotifChannel":"notifications_disabled","hooks":{` +
	`"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"cmux hooks claude session-start","timeout":10}]}],` +
	`"PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":"cmux hooks claude pre-tool-use","timeout":5,"async":true}]}]}}`

// hookCommands lists the commands of each event's hooks in settings, in order.
func hookCommands(t *testing.T, settings string) map[string][]string {
	t.Helper()
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if err := json.Unmarshal([]byte(settings), &s); err != nil {
		t.Fatalf("settings %s: %v", settings, err)
	}
	out := map[string][]string{}
	for event, matchers := range s.Hooks {
		for _, m := range matchers {
			for _, h := range m.Hooks {
				out[event] = append(out[event], h.Command)
			}
		}
	}
	return out
}

func TestActivityHooksJoinTheSettingsClaudeApplies(t *testing.T) {
	l := boxlayout.Layout{Home: "/home/agent", Name: "remote-agents-e2e"}
	// Claude applies only the last --settings, so a second flag would drop cmux's hooks or ours.
	for name, tc := range map[string]struct {
		argv  []string
		value func([]string) string
	}{
		"separate value": {[]string{"claude", "--settings", "{}", "--settings", cmuxSettings, "fix it"},
			func(a []string) string { return a[4] }},
		"joined": {[]string{"claude", "--settings=" + cmuxSettings, "fix it"},
			func(a []string) string {
				value, ok := strings.CutPrefix(a[1], "--settings=")
				if !ok {
					t.Fatalf("joined --settings lost its option: %q", a[1])
				}
				return value
			}},
	} {
		t.Run(name, func(t *testing.T) {
			argv, err := withActivityHooks(l, tc.argv, "/home/agent/work")
			if err != nil {
				t.Fatal(err)
			}
			if len(argv) != len(tc.argv) || argv[len(argv)-1] != "fix it" || (name == "separate value" && argv[2] != "{}") {
				t.Fatalf("argv = %q", argv)
			}
			value := tc.value(argv)
			got := hookCommands(t, value)
			if !slices.Equal(got["SessionStart"], []string{"cmux hooks claude session-start", activityCommand}) ||
				!slices.Equal(got["PreToolUse"], []string{"cmux hooks claude pre-tool-use"}) {
				t.Fatalf("hooks = %v", got)
			}
			for _, event := range reaper.HookEvents {
				if !slices.Contains(got[event], activityCommand) {
					t.Fatalf("%s hooks = %v", event, got[event])
				}
			}
			var rest map[string]json.RawMessage
			if err := json.Unmarshal([]byte(value), &rest); err != nil {
				t.Fatal(err)
			}
			if string(rest["preferredNotifChannel"]) != `"notifications_disabled"` {
				t.Fatalf("settings lost other keys: %s", value)
			}
			if !strings.Contains(value, `"timeout":5,"async":true`) {
				t.Fatalf("settings lost hook fields: %s", value)
			}
		})
	}
}

func TestActivityHooksJoinASettingsFile(t *testing.T) {
	l := boxlayout.Layout{Home: "/home/agent", Name: "remote-agents-e2e"}
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "s.json"), []byte(cmuxSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	argv, err := withActivityHooks(l, []string{"claude", "--settings", "s.json"}, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if got := hookCommands(t, argv[2]); !slices.Equal(got["SessionStart"], []string{"cmux hooks claude session-start", activityCommand}) {
		t.Fatalf("hooks = %v", got)
	}
}

func TestActivityHooksJoinNullHooks(t *testing.T) {
	l := boxlayout.Layout{Home: "/home/agent", Name: "remote-agents-e2e"}
	argv, err := withActivityHooks(l, []string{"claude", "--settings", `{"hooks":null}`}, "/home/agent/work")
	if err != nil {
		t.Fatal(err)
	}
	if got := hookCommands(t, argv[2]); !slices.Equal(got["Stop"], []string{activityCommand}) {
		t.Fatalf("hooks = %v", got)
	}
}

func TestActivityHooksRejectBrokenSettings(t *testing.T) {
	l := boxlayout.Layout{Home: "/home/agent", Name: "remote-agents-e2e"}
	for _, value := range []string{"{not json", "[]", "null", `{"hooks":[]}`, "missing.json"} {
		if argv, err := withActivityHooks(l, []string{"claude", "--settings", value}, t.TempDir()); err == nil {
			t.Fatalf("--settings %s: argv = %q, want an error", value, argv)
		}
	}
}

func TestRecordActivity(t *testing.T) {
	l := testLayout(t)
	env := map[string]string{SessionEnvVar: "claude-abc"}
	payload := `{"hook_event_name":"UserPromptSubmit","session_id":"s1","transcript_path":"/t.jsonl"}`
	if err := RecordActivity(l, func(k string) string { return env[k] }, strings.NewReader(payload), time.UnixMilli(5)); err != nil {
		t.Fatal(err)
	}
	h, err := reaper.ReadHookState(l.HookState("claude-abc"))
	if err != nil || h.ClaudeSession != "s1" || h.TurnStarted != 5 {
		t.Fatalf("hook state %+v, %v", h, err)
	}
	// A broken hook fails and leaves a line in the reaper log.
	err = RecordActivity(l, func(string) string { return "" }, strings.NewReader(payload), time.Now())
	if err == nil || !strings.Contains(err.Error(), SessionEnvVar) {
		t.Fatalf("missing session: %v", err)
	}
	log, _ := os.ReadFile(l.ReaperLog())
	if !strings.Contains(string(log), "activity hook: "+SessionEnvVar) {
		t.Fatalf("reaper log = %q", log)
	}
}

func TestReapedRecordIsTakenOnce(t *testing.T) {
	l := testLayout(t)
	if r, err := takeReaped(l, "s1"); r != "" || err != nil {
		t.Fatalf("no record: %q, %v", r, err)
	}
	if err := writeReaped(l, claudeSpec(t.TempDir()), "s1", reaper.ReapedCap, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r, err := takeReaped(l, "s1"); r != reaper.ReapedCap || err != nil {
		t.Fatalf("record: %q, %v", r, err)
	}
	if _, found, err := readReapedSession(l, claudeSpec("/w").Session); found || err != nil {
		t.Fatalf("session record left: %v, %v", found, err)
	}
	if r, err := takeReaped(l, "s1"); r != "" || err != nil {
		t.Fatalf("record taken twice: %q, %v", r, err)
	}
}

func claudeSpec(work string) spec.Spec {
	s := sessionSpec(work, "claude", "--session-id", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	s.Claude = &spec.Claude{Resume: []string{"claude", "--dangerously-skip-permissions"}}
	return s
}

func TestResumeAfterReapRunsTheReapedSession(t *testing.T) {
	l := testLayout(t)
	const id = "bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	s := claudeSpec(t.TempDir())
	if err := writeReaped(l, s, id, reaper.ReapedIdle, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err := resumeAfterReap(l, &s, marker{Reason: reaper.ReapedIdle, ClaudeSession: id}, true)
	if err != nil || r != reaper.ReapedIdle {
		t.Fatalf("resume: %q, %v", r, err)
	}
	if !slices.Equal(s.Argv, []string{"claude", "--dangerously-skip-permissions", "--resume", id}) {
		t.Fatalf("argv = %q", s.Argv)
	}
	if _, err := os.Stat(l.Reaped(id)); !os.IsNotExist(err) {
		t.Fatalf("reaped record left: %v", err)
	}
}

func TestResumeAfterReapAnnouncesAnExplicitResume(t *testing.T) {
	l := testLayout(t)
	const id = "bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	s := claudeSpec(t.TempDir())
	s.Claude.Resumes = id
	argv := slices.Clone(s.Argv)
	if r, err := resumeAfterReap(l, &s, marker{}, false); r != "" || err != nil {
		t.Fatalf("never reaped: %q, %v", r, err)
	}
	if err := writeReaped(l, s, id, reaper.ReapedIdle, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r, err := resumeAfterReap(l, &s, marker{}, false); r != reaper.ReapedIdle || err != nil {
		t.Fatalf("reaped: %q, %v", r, err)
	}
	if !slices.Equal(s.Argv, argv) {
		t.Fatalf("an explicit resume keeps its own command line: %q", s.Argv)
	}
}

func TestAnnounceResume(t *testing.T) {
	for r, want := range map[reaper.Reason]string{reaper.ReapedIdle: "for inactivity", reaper.ReapedCap: "long time detached"} {
		var out bytes.Buffer
		announceResume(&out, r)
		if line := out.String(); !strings.HasPrefix(line, "\x1b[2m[remote-agents] resumed") || !strings.Contains(line, want) || strings.Count(line, "\n") != 1 {
			t.Errorf("%s: %q", r, line)
		}
	}
}

func TestLaunchReportsAReapedRunWithoutClaudeAsEnded(t *testing.T) {
	l := testLayout(t)
	if err := writeMarker(l, runID, marker{ExitCode: 0, Reason: reaper.ReapedIdle, ClaudeSession: "s1"}); err != nil {
		t.Fatal(err)
	}
	code, out := launch(t, l, sessionSpec(t.TempDir(), "/bin/sh", "-c", "echo SHOULD-NOT-RUN"))
	if code != 0 || strings.Contains(out, "SHOULD-NOT-RUN") || !strings.Contains(out, "ended while the link was down") {
		t.Fatalf("launch = %d %q", code, out)
	}
}

func TestObserveReadsHooksTranscriptsAndInterrupts(t *testing.T) {
	l := testLayout(t)
	dir := t.TempDir()
	transcript := filepath.Join(dir, "s1.jsonl")
	turn := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	w := &watcher{l: l, session: "claude-abc", self: 100}
	record := func(p string) {
		t.Helper()
		if err := reaper.RecordHook(l.HookState("claude-abc"), []byte(p), turn); err != nil {
			t.Fatal(err)
		}
	}
	record(`{"hook_event_name":"UserPromptSubmit","session_id":"s1","transcript_path":"` + transcript + `"}`)
	obs, err := w.observe(nil, 101)
	if err != nil || !obs.Hooks.TurnOpen() || obs.Interrupted || !obs.LastWrite.IsZero() {
		t.Fatalf("before any transcript: %+v, %v", obs, err)
	}
	entry := `{"type":"user","timestamp":"` + turn.Add(time.Second).UTC().Format(time.RFC3339Nano) + `","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "s1", "subagents")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.WriteFile(filepath.Join(sub, "agent-a.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(sub, "agent-a.jsonl"), later, later); err != nil {
		t.Fatal(err)
	}
	obs, err = w.observe(nil, 101)
	if err != nil || !obs.Interrupted || !obs.LastWrite.Equal(later) || len(obs.Working()) != 0 {
		t.Fatalf("after an interrupt: %+v, %v", obs, err)
	}
}

func TestReattachTargetResumesTheReapedRun(t *testing.T) {
	l := testLayout(t)
	const id = "dddddddd-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	s := claudeSpec("/w")
	s.Argv = []string{"claude", "--settings", "{activity}", "--session-id", "x"}
	if err := writeReaped(l, s, id, reaper.ReapedIdle, time.UnixMilli(7)); err != nil {
		t.Fatal(err)
	}
	rec, found, err := readReapedSession(l, s.Session)
	if err != nil || !found || rec.ClaudeSession != id || rec.At != 7 {
		t.Fatalf("record %+v, %v, %v", rec, found, err)
	}
	target, err := reattachTarget(rec)
	if err != nil {
		t.Fatal(err)
	}
	if target.Mode != spec.ModeSession || target.RunID != s.RunID || target.Claude.Resumes != id ||
		!slices.Equal(target.Argv, []string{"claude", "--dangerously-skip-permissions", "--resume", id}) {
		t.Fatalf("target = %+v", target)
	}
	if cmd, err := resumeCommand(rec); err != nil || cmd != "rclaude --resume "+id {
		t.Fatalf("resume command %q, %v", cmd, err)
	}
}

func TestReattachingAMissingSessionStillFails(t *testing.T) {
	code, out := launch(t, testLayout(t), spec.Spec{Mode: spec.ModeReattach, Session: claudeSpec("/w").Session})
	if code != 1 || !strings.Contains(out, "is not running") {
		t.Fatalf("launch = %d %q", code, out)
	}
}

func TestReapedSessionsNewestFirst(t *testing.T) {
	l := testLayout(t)
	for i, name := range []string{"claude-11111111-bbbb-4ccc-8ddd-eeeeeeeeeeee", "claude-22222222-bbbb-4ccc-8ddd-eeeeeeeeeeee"} {
		s := claudeSpec("/w")
		s.Session = name
		if err := writeReaped(l, s, fmt.Sprintf("c%d", i), reaper.ReapedIdle, time.UnixMilli(int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := reapedSessions(l)
	if err != nil || len(recs) != 2 || recs[0].ClaudeSession != "c1" || recs[1].ClaudeSession != "c0" {
		t.Fatalf("reaped = %+v, %v", recs, err)
	}
}

func TestTakeReapedKeepsANewerReapOfTheSameName(t *testing.T) {
	l := testLayout(t)
	s := claudeSpec("/w")
	if err := writeReaped(l, s, "old", reaper.ReapedIdle, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := writeReaped(l, s, "new", reaper.ReapedCap, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := takeReaped(l, "old"); err != nil {
		t.Fatal(err)
	}
	if rec, found, err := readReapedSession(l, s.Session); !found || err != nil || rec.ClaudeSession != "new" {
		t.Fatalf("newer record lost: %+v, %v, %v", rec, found, err)
	}
}
