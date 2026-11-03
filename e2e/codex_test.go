//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// codexStarted waits for a fake Codex to start after mark and returns its
// session id, agent pid and shpool session, once the fake has checked that
// Codex would trust every hook it was given.
func codexStarted(t *testing.T, tm *term, mark int) (id, pid, session string) {
	t.Helper()
	tm.expectAfter(mark, `fake-codex hooks trusted: \d+`, 20*time.Second)
	m := tm.expectAfter(mark, `fake-codex session=(`+uuidRe+`) pid=(\d+) cwd=\S+\s+fake-codex env BROWSER=\S+ SESSION=(\S+)`, 5*time.Second)
	return m[1], m[2], m[3]
}

// turn runs a Codex turn, whose first one reports the session to the box.
func turn(t *testing.T, tm *term, commands ...string) {
	t.Helper()
	for _, cmd := range commands {
		mark := len(tm.text())
		tm.send(cmd + "\r")
		tm.expectAfter(mark, `activity exit=0`, 5*time.Second)
	}
}

func TestCodexSessionLifecycle(t *testing.T) {
	tm := start(t, "rfakecodex", "-m", "x")
	args := tm.expect(`fake-codex args=(.*)\r?\n`, 20*time.Second)[1]
	id, pid, session := codexStarted(t, tm, 0)
	// The box keeps Codex off its shared server and adds its hooks; configured args go first.
	if !strings.HasPrefix(args, "--no-daemon --enable hooks -c hooks={") || !strings.HasSuffix(strings.TrimSpace(args), "} --yolo -m x") {
		t.Fatalf("agent args = %q", args)
	}
	browser := tm.expect(`fake-codex env BROWSER=(\S+)`, 5*time.Second)[1]
	if browser != filepath.Join(box.Shims(), "xdg-open") || !strings.HasPrefix(session, "fakecodex-") {
		t.Fatalf("agent env: BROWSER=%s SESSION=%s", browser, session)
	}
	turn(t, tm, "prompt", "stop")
	tm.expectMarks(cornerOf(termCols, termRows), 10*time.Second)

	mark := len(tm.text())
	if err := syscall.Kill(tm.sshChild(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	tm.expectAfter(mark, `reconnecting`, 5*time.Second)
	tm.drawnAfter(mark)
	mark = len(tm.text())
	tm.send("whoami\r")
	if got := tm.expectAfter(mark, `fake-codex pid=(\d+)`, 15*time.Second)[1]; got != pid {
		t.Fatalf("agent restarted: pid %s -> %s", pid, got)
	}

	// Outside cmux the laptop never hears the id Codex picked, so only Codex's own hint shows.
	tm.send("exit 7\r")
	tm.expect(`To continue this session, run:\s*codex resume `+id, 10*time.Second)
	if code := tm.wait(10 * time.Second); code != 7 {
		t.Fatalf("exit code %d, want 7", code)
	}
	if strings.Contains(tm.text(), "Resume on the box") {
		t.Fatalf("resume line for an id the laptop never learned:\n%s", tm.text())
	}

	again := start(t, "rfakecodex", "resume", id)
	againArgs := again.expect(`fake-codex args=(.*)\r?\n`, 20*time.Second)[1]
	if !strings.HasSuffix(strings.TrimSpace(againArgs), "--yolo resume "+id) {
		t.Fatalf("resume args = %q", againArgs)
	}
	again.expect(`fake-codex session=`+id, 5*time.Second)
	again.send("exit 0\r")
	again.expect(`Resume on the box with: rfakecodex resume `+id, 10*time.Second)
	if code := again.wait(10 * time.Second); code != 0 {
		t.Fatalf("resume exit %d", code)
	}
}

// killLauncher kills an agent's whole scope while the link is down, as a kill
// of the shpool unit would: the launcher dies with its agent and writes no ended marker.
func killLauncher(t *testing.T, tm *term, pid string) {
	t.Helper()
	block(t, true)
	if err := syscall.Kill(tm.sshChild(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	tm.expect(`network unreachable`, 10*time.Second)
	if _, err := onBox(fmt.Sprintf(`set -e
kill -KILL $(cat "/sys/fs/cgroup$(sed -n 's|^0::||p' /proc/%s/cgroup)/cgroup.procs")
while [ -e /proc/%s ]; do sleep 0.1; done`, pid, pid)); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRunWhoseLauncherWasKilledResumesItsSessionOnReconnect(t *testing.T) {
	tm := start(t, "rfakecodex")
	id, pid, _ := codexStarted(t, tm, 0)
	turn(t, tm, "prompt")
	defer block(t, false)
	killLauncher(t, tm, pid)
	mark := len(tm.text())
	block(t, false)
	args := tm.expectAfter(mark, `fake-codex args=(.*)\r?\n`, 20*time.Second)[1]
	if !strings.HasSuffix(strings.TrimSpace(args), "--yolo resume "+id) {
		t.Fatalf("relaunch args = %q, want a resume of %s", args, id)
	}
	tm.expectAfter(mark, `fake-codex session=`+id+` `, 5*time.Second)
	tm.send("exit 0\r")
	if code := tm.wait(10 * time.Second); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// Before its first turn Codex has reported no session, and has none to resume.
func TestCodexRunLostBeforeItsFirstTurnStartsAgain(t *testing.T) {
	tm := start(t, "rfakecodex")
	id, pid, _ := codexStarted(t, tm, 0)
	defer block(t, false)
	killLauncher(t, tm, pid)
	mark := len(tm.text())
	block(t, false)
	args := tm.expectAfter(mark, `fake-codex args=(.*)\r?\n`, 20*time.Second)[1]
	if strings.Contains(args, "resume") || !strings.HasSuffix(strings.TrimSpace(args), "--yolo") {
		t.Fatalf("relaunch args = %q, want the run's own command", args)
	}
	if again, _, _ := codexStarted(t, tm, mark); again == id {
		t.Fatalf("relaunch kept session %s", id)
	}
	tm.send("exit 0\r")
	tm.wait(10 * time.Second)
}

func TestCodexLosingTheShpoolDaemonResumesItsSession(t *testing.T) {
	tm := start(t, "rfakecodex")
	id, pid, _ := codexStarted(t, tm, 0)
	turn(t, tm, "prompt", "stop")
	mark := len(tm.text())
	if _, err := onBox("systemctl --user kill -s KILL --kill-whom=main " + box.ShpoolService()); err != nil {
		t.Fatal(err)
	}
	args := tm.expectAfter(mark, `fake-codex args=(.*)\r?\n`, 20*time.Second)[1]
	if !strings.HasSuffix(strings.TrimSpace(args), "resume "+id) {
		t.Fatalf("args after losing the daemon = %q, want a resume of %s", args, id)
	}
	again := tm.expectAfter(mark, `fake-codex session=(`+uuidRe+`) pid=(\d+)`, 5*time.Second)
	if again[1] != id || again[2] == pid {
		t.Fatalf("after losing the daemon: session %s pid %s, was %s pid %s", again[1], again[2], id, pid)
	}
	tm.send("exit 0\r")
	if code := tm.wait(10 * time.Second); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// An interrupted Codex turn sends no Stop; its Interrupt hook ends the turn.
func TestCodexIdleDetachedSessionIsReapedThenResumedOnReconnect(t *testing.T) {
	shortReaper(t)
	tm := start(t, "rfakecodex")
	id, pid, session := codexStarted(t, tm, 0)
	turn(t, tm, "prompt", "interrupt")
	tm.send("leave 4444\r")
	tm.expect(`leave started`, 5*time.Second)
	dropLink(t, tm)
	defer block(t, false)
	waitGone(t, session, 20*time.Second)
	log, err := onBox("cat " + box.ReaperLog())
	if err != nil || !strings.Contains(log, "ending agent session "+id+" (reaped-idle)") {
		t.Fatalf("reaper log: %v\n%s", err, log)
	}
	if left, _ := onBox("pgrep -a -x sleep | grep -c ' 4444$'"); strings.TrimSpace(left) != "0" {
		t.Fatalf("left-behind sleep survived the reap: %q", left)
	}
	if ls := boxListing(t); !strings.Contains(ls, "rfakecodex resume "+id) {
		t.Fatalf("reaped session not listed:\n%s", ls)
	}
	mark := len(tm.text())
	block(t, false)
	tm.expectAfter(mark, `resumed; the session had been ended for inactivity`, 20*time.Second)
	args := tm.expectAfter(mark, `fake-codex args=(.*)\r?\n`, 10*time.Second)[1]
	if !strings.HasSuffix(strings.TrimSpace(args), "--yolo resume "+id) {
		t.Fatalf("resumed with %q", args)
	}
	if got := tm.expectAfter(mark, `fake-codex session=`+id+` pid=(\d+)`, 10*time.Second)[1]; got == pid {
		t.Fatal("the reaped agent is still the one running")
	}
	tm.send("exit 0\r")
	if code := tm.wait(10 * time.Second); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestCodexToolShellKeepsADetachedSessionRunning(t *testing.T) {
	shortReaper(t)
	tm := start(t, "rfakecodex")
	id, pid, _ := codexStarted(t, tm, 0)
	turn(t, tm, "prompt", "stop")
	tm.send("bg 20\r")
	tm.expect(`bg started`, 5*time.Second)
	dropLink(t, tm)
	defer block(t, false)
	time.Sleep(9 * time.Second)
	if ls := boxSessions(t); !strings.Contains(ls, "working: tool") {
		t.Fatalf("working session %s not kept:\n%s", id, ls)
	}
	mark := len(tm.text())
	block(t, false)
	tm.drawnAfter(mark)
	tm.send("whoami\r")
	if got := tm.expectAfter(mark, `fake-codex pid=(\d+)`, 15*time.Second)[1]; got != pid {
		t.Fatalf("reattached to pid %s, want %s", got, pid)
	}
	tm.send("exit 0\r")
	tm.wait(10 * time.Second)
}

// In cmux the box relays cmux's Codex hooks, run as if the wrapper were
// Codex, and the laptop learns the session they report.
func TestCodexHookRelayReachesTheTerminal(t *testing.T) {
	cmux := filepath.Join(filepath.Dir(hookLog), "cmux")
	tm := startEnv(t, []string{"CMUX_SURFACE_ID=e2e-surface", "CMUX_BUNDLED_CLI_PATH=" + cmux, "CMUX_AGENT_LAUNCH_KIND=claude"}, "rfakecodex")
	id, _, _ := codexStarted(t, tm, 0)
	mark := countLines(t, hookLog, "hooks codex prompt-submit ")
	tm.send("prompt\r")
	tm.expect(`cmux exit=0 out=\{"ok":true\}`, 10*time.Second)
	permission := len(tm.text())
	tm.send("permission\r")
	tm.expectAfter(permission, `activity exit=0\s+cmux exit=0 out=\{"ok":true\}`, 10*time.Second)
	data, err := os.ReadFile(hookLog)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := "codexpid=" + strconv.Itoa(tm.cmd.Process.Pid) + " launch=codex "
	for _, want := range []string{"hooks codex session-start surface=e2e-surface", "hooks feed --source codex --event PermissionRequest surface=e2e-surface"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("hook log lacks %q:\n%s", want, data)
		}
	}
	if !strings.Contains(string(data), wrapper) || countLines(t, hookLog, "hooks codex prompt-submit ") != mark+1 {
		t.Fatalf("hooks did not run as the wrapper's Codex (%q):\n%s", wrapper, data)
	}
	tm.send("exit 0\r")
	tm.expect(`Resume on the box with: rfakecodex resume `+id, 10*time.Second)
	tm.wait(10 * time.Second)
}

// A new Codex session runs under a name the box picked before Codex named the
// session; a resume of it by id still reattaches there.
func TestCodexResumeFromAnotherTerminalTakesOver(t *testing.T) {
	first := start(t, "rfakecodex")
	id, pid, session := codexStarted(t, first, 0)
	turn(t, first, "prompt", "stop")
	second := start(t, "rfakecodex", "resume", id)
	first.expect(`another terminal took over `+session, 20*time.Second)
	if code := first.wait(10 * time.Second); code != 75 {
		t.Fatalf("first terminal exit %d, want 75", code)
	}
	second.drawnAfter(0)
	second.send("whoami\r")
	if got := second.expect(`fake-codex pid=(\d+)`, 15*time.Second)[1]; got != pid {
		t.Fatalf("takeover started a second agent: %s -> %s", pid, got)
	}
	second.send("exit 0\r")
	second.expect(`Resume on the box with: rfakecodex resume `+id, 10*time.Second)
	if code := second.wait(10 * time.Second); code != 0 {
		t.Fatalf("second terminal exit %d", code)
	}
}
