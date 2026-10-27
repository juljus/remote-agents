package reaper

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte("idle_after = \"8s\"\npoll = \"1s\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg != (Config{IdleAfter: 8 * time.Second, WorkingCap: 36 * time.Hour, Poll: time.Second}) {
		t.Fatalf("config = %+v", cfg)
	}
	for src, want := range map[string]string{
		"bogus = 1\n":                         "bogus",
		"idle_after = \"soon\"\n":             "idle_after",
		"working_cap = \"-1h\"\n":             "positive",
		"idle_after = \"5s\"\npoll = \"10s\"": "longer",
	} {
		if _, err := ParseConfig([]byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", src, err, want)
		}
	}
}

func TestLoadConfigWithoutAFileIsTheDefault(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "reaper.toml"))
	if err != nil || cfg != Default {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
}

var t0 = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func apply(t *testing.T, h HookState, at time.Time, payload string) HookState {
	t.Helper()
	h, err := h.Apply([]byte(payload), at)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHookStateFollowsTurnsTasksAndCrons(t *testing.T) {
	var h HookState
	h = apply(t, h, t0, `{"hook_event_name":"SessionStart","session_id":"s1","transcript_path":"/p/s1.jsonl","source":"startup"}`)
	if h.ClaudeSession != "s1" || h.Transcript != "/p/s1.jsonl" || h.TurnOpen() {
		t.Fatalf("after start: %+v", h)
	}
	h = apply(t, h, t0.Add(time.Second), `{"hook_event_name":"UserPromptSubmit","session_id":"s1","prompt":"go"}`)
	if !h.TurnOpen() {
		t.Fatal("prompt did not open a turn")
	}
	h = apply(t, h, t0.Add(2*time.Second), `{"hook_event_name":"SubagentStop","session_id":"s1","background_tasks":[{"id":"b1","type":"shell","status":"running"}]}`)
	if !h.TurnOpen() || h.BackgroundTasks != 1 {
		t.Fatalf("subagent stop: %+v", h)
	}
	h = apply(t, h, t0.Add(3*time.Second), `{"hook_event_name":"Stop","session_id":"s1","background_tasks":[{"status":"running"},{"status":"completed"}],"session_crons":[{"id":"c1","schedule":"13 19 * * *","recurring":false}]}`)
	if h.TurnOpen() || h.BackgroundTasks != 1 || h.SessionCrons != 1 {
		t.Fatalf("stop: %+v", h)
	}
	h = apply(t, h, t0.Add(4*time.Second), `{"hook_event_name":"StopFailure","session_id":"s1"}`)
	if h.BackgroundTasks != 1 || h.SessionCrons != 1 {
		t.Fatalf("an event without the lists must keep them: %+v", h)
	}
	h = apply(t, h, t0.Add(5*time.Second), `{"hook_event_name":"Stop","session_id":"s1","background_tasks":[],"session_crons":[]}`)
	if h.BackgroundTasks != 0 || h.SessionCrons != 0 {
		t.Fatalf("emptied lists: %+v", h)
	}
	// /clear starts a new Claude session in the same process.
	h = apply(t, h, t0.Add(6*time.Second), `{"hook_event_name":"SessionStart","session_id":"s2","transcript_path":"/p/s2.jsonl","source":"clear"}`)
	if h.ClaudeSession != "s2" || h.Transcript != "/p/s2.jsonl" {
		t.Fatalf("after clear: %+v", h)
	}
}

func TestHookStateFollowsWaitingOnTheUser(t *testing.T) {
	var h HookState
	h = apply(t, h, t0, `{"hook_event_name":"UserPromptSubmit","session_id":"s1"}`)
	h = apply(t, h, t0.Add(time.Second), `{"hook_event_name":"Notification","session_id":"s1","notification_type":"auth_success"}`)
	if h.WaitingSince != 0 {
		t.Fatalf("auth_success counted as waiting: %+v", h)
	}
	for _, kind := range []string{"permission_prompt", "elicitation_dialog"} {
		h = apply(t, h, t0.Add(2*time.Second), `{"hook_event_name":"Notification","session_id":"s1","notification_type":"`+kind+`"}`)
		if h.WaitingSince != t0.Add(2*time.Second).UnixMilli() {
			t.Fatalf("%s: %+v", kind, h)
		}
	}
	if h = apply(t, h, t0.Add(3*time.Second), `{"hook_event_name":"Stop","session_id":"s1"}`); h.WaitingSince != 0 {
		t.Fatalf("Stop kept waiting: %+v", h)
	}
}

func TestHookStateRejectsUnknownPayloads(t *testing.T) {
	for _, p := range []string{`not json`, `{"hook_event_name":"PreToolUse"}`, `{}`} {
		if _, err := (HookState{}).Apply([]byte(p), t0); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

func TestRecordHookSerializesConcurrentHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity", "s.hooks.json")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := fmt.Sprintf(`{"hook_event_name":"Stop","session_id":"s","background_tasks":[%s]}`, strings.TrimSuffix(strings.Repeat(`{"status":"running"},`, i), ","))
			if err := RecordHook(path, []byte(p), t0); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	h, err := ReadHookState(path)
	if err != nil || h.ClaudeSession != "s" || h.TurnEnded != t0.UnixMilli() {
		t.Fatalf("state %+v, %v", h, err)
	}
}

func TestInterrupted(t *testing.T) {
	tail := []byte(`ial line from the middle"}
{"type":"user","timestamp":"2026-10-25T12:00:01Z","message":{"role":"user","content":"go"}}
{"type":"assistant","timestamp":"2026-10-25T12:00:02Z","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}
{"type":"user","timestamp":"2026-10-25T12:00:03Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}
`)
	if !Interrupted(tail, t0) {
		t.Fatal("interrupt after the turn started not seen")
	}
	if Interrupted(tail, t0.Add(4*time.Second)) {
		t.Fatal("an interrupt before the turn started counted")
	}
	if Interrupted([]byte(`{"type":"user","timestamp":"2026-10-25T12:00:05Z","message":{"content":"[Request interrupted by user]"}}`), t0) == false {
		t.Fatal("string content not seen")
	}
}

// fakeProc is a process as /proc shows it; cpu is its own time, childCPU its waited children's.
type fakeProc struct {
	comm     string
	state    string
	ppid     int
	cpu      uint64
	args     []string
	childCPU uint64
	sid      int
}

func stat(pid int, p fakeProc) string {
	// Fields 5 to 13 are irrelevant here; utime and stime split cpu, cutime and cstime childCPU.
	return fmt.Sprintf("%d (%s) %s %d 0 %d 0 0 0 0 0 0 0 %d %d %d %d 20 0 1 0 100", pid, p.comm, p.state, p.ppid, p.sid,
		p.cpu/2, p.cpu-p.cpu/2, p.childCPU/2, p.childCPU-p.childCPU/2)
}

func procFS(procs map[int]fakeProc) fstest.MapFS {
	fsys := fstest.MapFS{"self": &fstest.MapFile{Mode: 0o777}}
	for pid, p := range procs {
		cmdline := ""
		if len(p.args) > 0 {
			cmdline = strings.Join(p.args, "\x00") + "\x00"
		}
		fsys[fmt.Sprintf("%d/stat", pid)] = &fstest.MapFile{Data: []byte(stat(pid, p))}
		fsys[fmt.Sprintf("%d/cmdline", pid)] = &fstest.MapFile{Data: []byte(cmdline)}
	}
	return fsys
}

func readFixture(t *testing.T, procs map[int]fakeProc) []Proc {
	t.Helper()
	list, err := ReadProcs(procFS(procs))
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(list, func(a, b Proc) int { return a.PID - b.PID })
	return list
}

var toolShellArgs = []string{"/bin/bash", "-c", "source /home/agent/.claude/shell-snapshots/snapshot-bash-1-x.sh 2>/dev/null || true && sleep 25"}

func with(base map[int]fakeProc, extra map[int]fakeProc) map[int]fakeProc {
	out := map[int]fakeProc{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestReadProcsParsesStatAndCmdline(t *testing.T) {
	list := readFixture(t, map[int]fakeProc{
		10: {"claude", "S", 9, 7, []string{"claude", "--resume", "x"}, 0, 0},
		11: {"odd) name)", "Z", 10, 0, nil, 0, 0},
	})
	if len(list) != 2 || list[0].PPID != 9 || list[0].CPU != 7 || !slices.Equal(list[0].Args, []string{"claude", "--resume", "x"}) || list[0].Zombie {
		t.Fatalf("procs = %+v", list)
	}
	if list[1].PPID != 10 || !list[1].Zombie || list[1].Args != nil {
		t.Fatalf("odd proc = %+v", list[1])
	}
	short := fstest.MapFS{"1/stat": &fstest.MapFile{Data: []byte("1 (x) S 0 1 1 0 0 0 0 0 0 0 0 5 5")}, "1/cmdline": &fstest.MapFile{}}
	if _, err := ReadProcs(short); err == nil {
		t.Fatal("short stat accepted")
	}
}

func TestExited(t *testing.T) {
	fsys := procFS(map[int]fakeProc{10: {"claude", "S", 9, 0, []string{"claude"}, 0, 0}, 11: {"launcher", "Z", 9, 0, nil, 0, 0}})
	for pid, want := range map[int]bool{10: false, 11: true, 12: true} {
		if got, err := Exited(fsys, pid); err != nil || got != want {
			t.Errorf("Exited(%d) = %v, %v; want %v", pid, got, err, want)
		}
	}
}

const launcher, agent = 100, 101

var session = map[int]fakeProc{
	launcher: {"remote-agents-b", "S", 50, 0, []string{"remote-agents-box", "launch", "T"}, 0, 0},
	agent:    {"claude", "S", launcher, 0, []string{"claude"}, 0, 0},
	102:      {"node", "S", agent, 0, []string{"node", "mcp-server.js"}, 0, 0},
	103:      {"sh", "S", agent, 0, []string{"sh", "-c", "cmux hooks claude stop"}, 0, 0},
}

func TestToolShells(t *testing.T) {
	if n := ToolShells(readFixture(t, session), agent); n != 0 {
		t.Fatalf("MCP servers and hooks counted as tools: %d", n)
	}
	shell := with(session, map[int]fakeProc{
		200: {"bash", "S", agent, 0, toolShellArgs, 0, 0},
		201: {"sleep", "S", 200, 0, []string{"sleep", "25"}, 0, 0},
		202: {"bash", "Z", agent, 0, nil, 0, 0},
	})
	if n := ToolShells(readFixture(t, shell), agent); n != 1 {
		t.Fatalf("tool shell count = %d, want 1", n)
	}
	nested := with(session, map[int]fakeProc{200: {"bash", "S", 102, 0, toolShellArgs, 0, 0}})
	if n := ToolShells(readFixture(t, nested), agent); n != 1 {
		t.Fatalf("tool shell deeper below the agent: %d, want 1", n)
	}
}

func TestLeftoversSparesSelfDetachedDaemons(t *testing.T) {
	procs := readFixture(t, with(session, map[int]fakeProc{
		// A dev server left behind its tool shell stays in the shell's session.
		300: {"make", "S", launcher, 0, []string{"make"}, 0, 299},
		301: {"cc", "R", 300, 0, []string{"cc", "x.c"}, 0, 299},
		302: {"dead", "Z", launcher, 0, nil, 0, 299},
		// A tmux server leads its own session, and its panes lead theirs.
		400: {"tmux: server", "S", launcher, 0, []string{"tmux", "new", "-d"}, 0, 400},
		401: {"zsh", "S", 400, 0, []string{"-zsh"}, 0, 401},
		// A tool shell orphaned by the agent's exit leads its own session too, and is no daemon.
		500: {"bash", "S", launcher, 0, toolShellArgs, 0, 500},
		501: {"sleep", "S", 500, 0, []string{"sleep", "9"}, 0, 500},
		// An MCP server the agent's exit orphaned stays in the launcher's session.
		600: {"node", "S", launcher, 0, []string{"node", "mcp.js"}, 0, launcher},
	}))
	pids := func(ps []Proc) []int {
		var out []int
		for _, p := range ps {
			out = append(out, p.PID)
		}
		slices.Sort(out)
		return out
	}
	own, daemons := Leftovers(procs, launcher, agent)
	if got := pids(own); !slices.Equal(got, []int{300, 301, 500, 501, 600}) {
		t.Fatalf("own = %v", got)
	}
	if got := pids(daemons); !slices.Equal(got, []int{400}) {
		t.Fatalf("daemons = %v", got)
	}
}

func TestCPUMeterCountsOnlyCPUUsedSinceTheLastLook(t *testing.T) {
	var m CPUMeter
	if used := m.Used([]Proc{{PID: 1, CPU: 500}}); used != 0 {
		t.Fatalf("first look counted %d", used)
	}
	if used := m.Used([]Proc{{PID: 1, CPU: 530}, {PID: 2, CPU: 900}}); used != 30 {
		t.Fatalf("second look = %d, want 30", used)
	}
	if used := m.Used([]Proc{{PID: 2, CPU: 900}}); used != 0 {
		t.Fatalf("an idle daemon counted %d", used)
	}
	if used := m.Used([]Proc{{PID: 1, CPU: 10}}); used != 0 {
		t.Fatalf("a reused pid counted %d", used)
	}
}

// make itself barely runs; its compilers exit between polls, and the kernel
// adds their time to make's waited-children counters.
func TestCPUMeterSeesABuildThroughItsWaitedChildren(t *testing.T) {
	var m CPUMeter
	look := func(childCPU uint64) uint64 {
		t.Helper()
		procs := readFixture(t, map[int]fakeProc{300: {"make", "S", launcher, 3, []string{"make", "-j"}, childCPU, 0}})
		return m.Used(procs)
	}
	look(100)
	if used := look(400); used != 300 {
		t.Fatalf("build counted %d ticks, want 300", used)
	}
}

func TestBusyTicks(t *testing.T) {
	if got := BusyTicks(15 * time.Second); got != 30 {
		t.Fatalf("BusyTicks(15s) = %d", got)
	}
	if got := BusyTicks(100 * time.Millisecond); got != 1 {
		t.Fatalf("BusyTicks(100ms) = %d", got)
	}
}

func TestWorking(t *testing.T) {
	open := HookState{TurnStarted: 2, TurnEnded: 1}
	cases := []struct {
		o    Observation
		want []string
	}{
		{Observation{}, nil},
		{Observation{Hooks: open}, []string{"turn"}},
		{Observation{Hooks: open, Interrupted: true}, nil},
		// At a permission prompt, until the transcript shows the user's answer.
		{Observation{Hooks: HookState{TurnStarted: 2, TurnEnded: 1, WaitingSince: 3}, LastWrite: time.UnixMilli(3)}, nil},
		{Observation{Hooks: HookState{TurnStarted: 2, TurnEnded: 1, WaitingSince: 3}, LastWrite: time.UnixMilli(4)}, []string{"turn"}},
		{Observation{ToolShells: 2}, []string{"tool"}},
		{Observation{BusyLeftovers: true}, []string{"background process"}},
		{Observation{Hooks: HookState{BackgroundTasks: 1, SessionCrons: 2}}, []string{"background task", "scheduled"}},
	}
	for _, tc := range cases {
		if got := tc.o.Working(); !slices.Equal(got, tc.want) {
			t.Errorf("%+v: %q, want %q", tc.o, got, tc.want)
		}
	}
}

func TestTimer(t *testing.T) {
	cfg := Config{IdleAfter: 5 * time.Minute, WorkingCap: 36 * time.Hour, Poll: 15 * time.Second}
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	tm := NewTimer(cfg, t0)
	if d := tm.Step(at(time.Hour), true, time.Time{}, false, time.Time{}); d.Reap != "" || !d.IdleSince.IsZero() {
		t.Fatalf("attached: %+v", d)
	}
	detached := at(time.Hour + time.Minute)
	if d := tm.Step(at(time.Hour+4*time.Minute), false, detached, false, time.Time{}); d.Reap != "" || d.IdleSince != detached {
		t.Fatalf("idle 3m: %+v", d)
	}
	// Recent output pushes the idle start forward.
	wrote := at(time.Hour + 3*time.Minute)
	if d := tm.Step(at(time.Hour+7*time.Minute), false, detached, false, wrote); d.Reap != "" || d.IdleSince != wrote {
		t.Fatalf("idle 4m after output: %+v", d)
	}
	if d := tm.Step(at(time.Hour+8*time.Minute), false, detached, false, wrote); d.Reap != ReapedIdle {
		t.Fatalf("idle 5m: %+v", d)
	}
	// Working resets the idle start.
	tm = NewTimer(cfg, t0)
	tm.Step(at(10*time.Minute), false, t0, true, time.Time{})
	if d := tm.Step(at(14*time.Minute), false, t0, false, time.Time{}); d.Reap != "" || d.IdleSince != at(10*time.Minute) {
		t.Fatalf("idle 4m after working: %+v", d)
	}
	if d := tm.Step(at(36*time.Hour), false, t0, true, time.Time{}); d.Reap != ReapedCap {
		t.Fatalf("working 36h detached: %+v", d)
	}
	// A session launched detached gets a full idle period first.
	tm = NewTimer(cfg, at(time.Hour))
	if d := tm.Step(at(time.Hour+4*time.Minute), false, t0, false, time.Time{}); d.Reap != "" {
		t.Fatalf("reaped within idle_after of its start: %+v", d)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.status.json")
	if _, ok, err := ReadStatus(path); ok || err != nil {
		t.Fatalf("missing status: %v, %v", ok, err)
	}
	in := Status{Working: []string{"tool"}, Updated: 5}
	if err := WriteStatus(path, in); err != nil {
		t.Fatal(err)
	}
	out, ok, err := ReadStatus(path)
	if !ok || err != nil || !slices.Equal(out.Working, in.Working) || out.Updated != 5 {
		t.Fatalf("status %+v, %v, %v", out, ok, err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadStatus(path); err == nil {
		t.Fatal("corrupt status accepted")
	}
}
