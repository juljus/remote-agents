package box

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/cmuxcodex"
	"remote-agents/internal/jsonfile"
	"remote-agents/internal/reaper"
	"remote-agents/internal/spec"
)

// cmux 0.64.17 trusts its own Stop hook for Codex by this hash; Codex 0.166
// computes the same from the hook's declaration.
const (
	cmuxStopCommand = `cmux_cli="${CMUX_BUNDLED_CLI_PATH:-}"; if [ -z "$cmux_cli" ] || [ ! -x "$cmux_cli" ]; then cmux_cli="$(command -v cmux 2>/dev/null || true)"; fi; if [ -n "$CMUX_SURFACE_ID" ] && [ "$CMUX_CODEX_HOOKS_DISABLED" != "1" ] && [ -n "$cmux_cli" ]; then { if [ -n "${CMUX_SOCKET_PATH:-}" ]; then "$cmux_cli" --socket "$CMUX_SOCKET_PATH" hooks codex stop; else "$cmux_cli" hooks codex stop; fi; } || echo '{}'; else echo '{}'; fi`
	cmuxStopHash    = "sha256:3e746383de27a92b00122a59378d18810543e95de4d66d88ee4429cd2c9e019e"
)

func TestCodexTrustHashMatchesCodex(t *testing.T) {
	got, err := codexTrustHash(codexHook{event: "Stop", command: cmuxStopCommand, timeout: 5})
	if err != nil || got != cmuxStopHash {
		t.Fatalf("hash = %q, %v; want %q", got, err, cmuxStopHash)
	}
}

func TestCodexTrustHashRefusesCommandsItCannotEncodeAsCodexDoes(t *testing.T) {
	for _, command := range []string{"a\tb", "café", "a\x00"} {
		if _, err := codexTrustHash(codexHook{event: "Stop", command: command, timeout: 5}); err == nil {
			t.Fatalf("hash of %q accepted", command)
		}
	}
}

type codexGroup struct {
	Hooks []struct {
		Type    string `toml:"type"`
		Command string `toml:"command"`
		Timeout int    `toml:"timeout"`
	} `toml:"hooks"`
}

// codexOverride reads the hooks override a launch passes Codex.
func codexOverride(t *testing.T, argv []string) map[string]any {
	t.Helper()
	i := slices.Index(argv, "-c")
	if i < 0 || !strings.HasPrefix(argv[i+1], "hooks=") {
		t.Fatalf("no hooks override in %q", argv)
	}
	var doc map[string]any
	if err := toml.Unmarshal([]byte(argv[i+1]), &doc); err != nil {
		t.Fatalf("override is not TOML: %v\n%s", err, argv[i+1])
	}
	return doc["hooks"].(map[string]any)
}

func groupsOf(t *testing.T, hooks map[string]any, event string) []codexGroup {
	t.Helper()
	data, err := toml.Marshal(map[string]any{"g": hooks[event]})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		G []codexGroup `toml:"g"`
	}
	if err := toml.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out.G
}

// checkTrusted asserts that every hook the override declares is trusted by
// the hash Codex computes for it, under the key Codex looks it up by.
func checkTrusted(t *testing.T, hooks map[string]any) {
	t.Helper()
	state := hooks["state"].(map[string]any)
	n := 0
	for event := range hooks {
		if event == "state" {
			continue
		}
		for g, group := range groupsOf(t, hooks, event) {
			h := group.Hooks[0]
			want, err := codexTrustHash(codexHook{event: event, command: h.Command, timeout: h.Timeout})
			if err != nil {
				t.Fatal(err)
			}
			key, err := codexHookKey(event, g)
			if err != nil {
				t.Fatal(err)
			}
			entry, ok := state[key].(map[string]any)
			if !ok || entry["trusted_hash"] != want {
				t.Fatalf("state[%q] = %v, want trusted_hash %q", key, state[key], want)
			}
			n++
		}
	}
	if len(state) != n {
		t.Fatalf("state has %d entries for %d hooks", len(state), n)
	}
}

func TestCodexHooksReportEveryEventToTheBox(t *testing.T) {
	l := testLayout(t)
	argv, err := withCodexHooks(l, []string{"codex", "resume", "x"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv[:4], []string{"codex", "--no-daemon", "--enable", "hooks"}) || !slices.Equal(argv[6:], []string{"resume", "x"}) {
		t.Fatalf("argv = %q", argv)
	}
	hooks := codexOverride(t, argv)
	for _, event := range reaper.CodexHookEvents {
		groups := groupsOf(t, hooks, event)
		if len(groups) != 1 || groups[0].Hooks[0].Command != shellQuote(l.Binary())+" activity" || groups[0].Hooks[0].Type != "command" {
			t.Fatalf("%s hooks = %+v", event, groups)
		}
	}
	if len(hooks) != len(reaper.CodexHookEvents)+1 {
		t.Fatalf("hooks = %v", hooks)
	}
	checkTrusted(t, hooks)
}

func TestCodexHooksRelayCmuxsHooksToTheLaptop(t *testing.T) {
	l := testLayout(t)
	argv, err := withCodexHooks(l, []string{"codex"}, true)
	if err != nil {
		t.Fatal(err)
	}
	hooks := codexOverride(t, argv)
	cmux := shellQuote(filepath.Join(l.HookBin(), "cmux"))
	for _, h := range cmuxcodex.Hooks {
		groups := groupsOf(t, hooks, h.Event)
		last := groups[len(groups)-1].Hooks[0]
		if last.Command != cmux+" "+strings.Join(h.Argv, " ") {
			t.Fatalf("%s relays %q", h.Event, last.Command)
		}
	}
	// Each event runs the box's hook as well as cmux's.
	if groups := groupsOf(t, hooks, "SessionStart"); len(groups) != 2 || !strings.HasSuffix(groups[0].Hooks[0].Command, " activity") {
		t.Fatalf("SessionStart hooks = %+v", groups)
	}
	checkTrusted(t, hooks)
}

// Codex refuses --no-daemon given twice at the same level; after -- it is prompt text.
func TestCodexHooksKeepAGivenNoDaemon(t *testing.T) {
	for args, want := range map[string]int{"--no-daemon hi": 1, "-- --no-daemon": 2} {
		argv, err := withCodexHooks(testLayout(t), append([]string{"codex"}, strings.Fields(args)...), false)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(strings.Join(argv, "\n"), "--no-daemon"); n != want {
			t.Fatalf("%s: argv = %q", args, argv)
		}
	}
}

// Every hook the box gives Codex has an event Codex keys trust under.
func TestEveryCodexHookEventHasATrustLabel(t *testing.T) {
	events := slices.Clone(reaper.CodexHookEvents)
	for _, h := range cmuxcodex.Hooks {
		events = append(events, h.Event)
	}
	for _, event := range events {
		if _, err := codexEventLabel(event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := codexTrustHash(codexHook{event: "Notification", command: "x", timeout: 1}); err == nil {
		t.Fatal("hashed a hook of an event Codex does not have")
	}
}

func lockFile(t *testing.T, path string) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return func() { f.Close() }
}

const codexSession = "01a1a1e4-7eb1-7753-927c-b59c2ec2f926"

func TestCodexHeldFollowsTheSessionsWriterLock(t *testing.T) {
	home := t.TempDir()
	if held, err := codexHeld(home, codexSession); err != nil || held {
		t.Fatalf("held = %v, %v before any Codex ran", held, err)
	}
	locks := filepath.Join(home, "thread-writer-locks")
	unlock := lockFile(t, filepath.Join(locks, codexSession+".lock"))
	if err := os.WriteFile(filepath.Join(locks, ".coordination.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if held, err := codexHeld(home, codexSession); err != nil || !held {
		t.Fatalf("held = %v, %v while locked", held, err)
	}
	if held, err := codexHeld(home, "01a1a1e4-0000-7753-927c-b59c2ec2f926"); err != nil || held {
		t.Fatalf("another session held = %v, %v", held, err)
	}
	unlock()
	if held, err := codexHeld(home, codexSession); err != nil || held {
		t.Fatalf("held = %v, %v after the writer exited", held, err)
	}
}

func TestAwaitCodexReleaseWaitsForTheWriter(t *testing.T) {
	home := t.TempDir()
	locks := filepath.Join(home, "thread-writer-locks")
	unlock := lockFile(t, filepath.Join(locks, codexSession+".lock"))
	if err := os.WriteFile(filepath.Join(locks, ".coordination.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		unlock()
	}()
	var logged []string
	start := time.Now()
	if err := awaitRelease(codexSession, 5*time.Second, codexProbe(home, codexSession), recordLog(&logged)); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 300*time.Millisecond || len(logged) != 2 {
		t.Fatalf("returned after %s, logged %q", time.Since(start), logged)
	}
}

// A Codex taking or giving up a session lock holds the coordination lock meanwhile.
func TestCodexHeldRetriesWhileACodexCoordinates(t *testing.T) {
	home := t.TempDir()
	unlock := lockFile(t, filepath.Join(home, "thread-writer-locks", ".coordination.lock"))
	defer unlock()
	if _, unsure, err := codexProbe(home, codexSession)(); err != nil || unsure == nil {
		t.Fatalf("probe = %v, %v; want unsure", unsure, err)
	}
}

func TestCodexHomeFollowsCodexHome(t *testing.T) {
	l := testLayout(t)
	if got := codexHome(l, nil); got != filepath.Join(l.Home, ".codex") {
		t.Fatalf("default home = %q", got)
	}
	if got := codexHome(l, []string{"CODEX_HOME=/elsewhere"}); got != "/elsewhere" {
		t.Fatalf("home = %q", got)
	}
}

func codexSpec(work string) spec.Spec {
	s := sessionSpec(work, "codex", "--yolo")
	s.Session = "codex-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	s.Codex = &spec.Agent{Resume: []string{"codex", "--yolo"}}
	return s
}

// Codex has written a session's rollout by the time its hooks report it.
func TestARelaunchResumesTheCodexSessionTheHooksReported(t *testing.T) {
	l := testLayout(t)
	s := codexSpec("/w")
	missing := filepath.Join(t.TempDir(), "rollout.jsonl")
	lostLaunch(t, l, s, runRecord{}, reaper.HookState{AgentSession: codexSession, Transcript: missing})
	if _, lost, err := resumeLostLaunch(l, &s, noLog); err != nil || !lost {
		t.Fatal(err)
	}
	if !slices.Equal(s.Argv, []string{"codex", "--yolo", "resume", codexSession}) {
		t.Fatalf("argv = %q", s.Argv)
	}
}

func TestReattachTargetResumesAReapedCodexRun(t *testing.T) {
	s := codexSpec("/w")
	target, err := reattachTarget(reapedSessionRecord{Reason: reaper.ReapedIdle, AgentSession: codexSession, Spec: s})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(target.Argv, []string{"codex", "--yolo", "resume", codexSession}) || target.Codex.Resumes != codexSession {
		t.Fatalf("target = %+v", target)
	}
	cmd, err := resumeCommand(reapedSessionRecord{AgentSession: codexSession, Spec: s})
	if err != nil || cmd != "rcodex resume "+codexSession {
		t.Fatalf("resume command = %q, %v", cmd, err)
	}
}

func TestTakesSessionPerKind(t *testing.T) {
	if got := (codexKind{}).takesSession([]string{"--no-daemon", "resume", codexSession}); got != codexSession {
		t.Fatalf("codex takes %q", got)
	}
	if got := (claudeKind{}).takesSession([]string{"--session-id", commandSession}); got != commandSession {
		t.Fatalf("claude takes %q", got)
	}
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// The trust identity is the declaration with sorted keys and nothing escaped beyond JSON's minimum.
func TestCodexTrustHashKeepsShellQuotesAndSlashesVerbatim(t *testing.T) {
	cmd := `'/home/a b/x' activity <"&">`
	want := `{"event_name":"session_start","hooks":[{"async":false,"command":"'/home/a b/x' activity <\"&\">","timeout":10,"type":"command"}]}`
	got, err := codexTrustHash(codexHook{event: "SessionStart", command: cmd, timeout: 10})
	if err != nil || got != "sha256:"+sha256Hex(want) {
		t.Fatalf("hash = %q, %v", got, err)
	}
}

// writeHooks records that the agent in shpool session name reports agentSession.
func writeHooks(t *testing.T, l boxlayout.Layout, name, agentSession string) {
	t.Helper()
	if err := os.MkdirAll(l.ActivityDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := jsonfile.Write(l.HookState(name), reaper.HookState{AgentSession: agentSession}); err != nil {
		t.Fatal(err)
	}
}

// Codex names a new session only once it runs, so the shpool session it runs
// in has another name than a later resume of it asks for.
func TestAResumeAttachesWhereItsSessionAlreadyRuns(t *testing.T) {
	l := testLayout(t)
	const running = "codex-bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	writeHooks(t, l, running, codexSession)
	writeHooks(t, l, "fakecodex-cccccccc-bbbb-4ccc-8ddd-eeeeeeeeeeee", codexSession)
	sessions := []shpoolSession{{Name: "fakecodex-cccccccc-bbbb-4ccc-8ddd-eeeeeeeeeeee"}, {Name: running}}
	s := codexSpec("/w")
	s.Session = "codex-" + codexSession
	s.Codex.Resumes = codexSession
	got, err := attachTarget(l, s, sessions)
	if err != nil || got.Session != running {
		t.Fatalf("target = %q, %v; want %s", got.Session, err, running)
	}
	for name, mutate := range map[string]func(*spec.Spec){
		"not a resume":            func(s *spec.Spec) { s.Codex.Resumes = "" },
		"its own name runs":       func(s *spec.Spec) { s.Session = running },
		"another session resumes": func(s *spec.Spec) { s.Codex.Resumes = "01a1a1e4-0000-7753-927c-b59c2ec2f926" },
	} {
		s := codexSpec("/w")
		s.Session = "codex-" + codexSession
		s.Codex.Resumes = codexSession
		mutate(&s)
		if got, err := attachTarget(l, s, sessions); err != nil || got.Session != s.Session {
			t.Fatalf("%s: target = %q, %v; want %s", name, got.Session, err, s.Session)
		}
	}
}

// Codex reports a resumed session only at its next turn, so the box follows
// the session its command line resumes from the start: a detached resume can
// be reaped, and found by a later resume of it.
func TestAWatcherFollowsTheSessionACodexResumes(t *testing.T) {
	l := testLayout(t)
	if (claudeKind{}).startsWith([]string{"--resume", commandSession}) != "" {
		t.Fatal("Claude reports its own session at startup")
	}
	seed := (codexKind{}).startsWith([]string{"--no-daemon", "resume", codexSession})
	if _, err := beginReports(l, "codex-x", runID, runRecord{}, seed); err != nil {
		t.Fatal(err)
	}
	hooks, err := reaper.ReadHookState(l.HookState("codex-x"))
	if err != nil || hooks.AgentSession != codexSession {
		t.Fatalf("hooks = %+v, %v", hooks, err)
	}
	if rec, _, err := readRunRecord(l, runID); err != nil || rec.AgentSession != codexSession {
		t.Fatalf("record = %+v, %v", rec, err)
	}
}

// A run that joined a session another run started is told how that session
// ended while its link was down, instead of resuming an agent that exited.
func TestAJoinedRunReportsTheEndOfTheSessionItJoined(t *testing.T) {
	l := testLayout(t)
	work := t.TempDir()
	const started = "11111111-0000-4333-8444-555555555555"
	first := sessionSpec(work, "/bin/sh", "-c", "exit 9")
	first.RunID, first.Session = started, "claude-bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	joined := time.Now()
	if err := recordJoined(l, runID, first.Session, joined); err != nil {
		t.Fatal(err)
	}
	if _, ended, err := joinedEnd(l, runID); err != nil || ended {
		t.Fatalf("ended = %v, %v before the session ended", ended, err)
	}
	if code, _ := launch(t, l, first); code != 9 {
		t.Fatalf("first run exit %d", code)
	}
	code, out := launch(t, l, sessionSpec(work, "/bin/sh", "-c", "echo SHOULD-NOT-RUN"))
	if code != 9 || strings.Contains(out, "SHOULD-NOT-RUN") || !strings.Contains(out, "ended") {
		t.Fatalf("joined run relaunch = %d %q", code, out)
	}
	// An end from before the join belongs to an earlier start of the session.
	if err := recordJoined(l, runID, first.Session, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, ended, err := joinedEnd(l, runID); err != nil || ended {
		t.Fatalf("ended = %v, %v for an end before the join", ended, err)
	}
}

func TestAttachingToARunningSessionJoinsIt(t *testing.T) {
	l := testLayout(t)
	const running = "codex-bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	writeHooks(t, l, running, codexSession)
	s := codexSpec("/w")
	s.Session, s.Codex.Resumes = "codex-"+codexSession, codexSession
	if _, err := attachTarget(l, s, []shpoolSession{{Name: running}}); err != nil {
		t.Fatal(err)
	}
	var j joinedRecord
	if found, err := jsonfile.Read(l.Joined(runID), &j); err != nil || !found || j.Session != running {
		t.Fatalf("joined = %+v %v %v", j, found, err)
	}
}
