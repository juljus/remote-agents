//go:build e2e

// End-to-end tests against a real box. They deploy the box side, then drive
// the laptop binaries through a pseudo-terminal with a fake agent.
//
//	RA_E2E_HOST=box.example go test -tags e2e -v ./e2e/
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/codexauth/codexfake"
	"remote-agents/internal/corner"
)

const boxTestDir = "remote-agents-test"

// boxInstall keeps the tests' shpool daemon, sockets and markers apart from the production install.
const boxInstall = "remote-agents-e2e"

var (
	host       string
	boxHome    string
	box        boxlayout.Layout
	binDir     string
	sshDir     string
	blockFile  string
	hookLog    string
	configPath string
	daemonLog  string
	laptopHome string
	daemonPid  int
	openedLog  string
	revealLog  string
	// approvalLog has a line per prompt the daemon showed; declineFile, while
	// it exists, has the user decline them.
	approvalLog string
	declineFile string
)

func TestMain(m *testing.M) {
	host = os.Getenv("RA_E2E_HOST")
	if host == "" {
		fmt.Fprintln(os.Stderr, "RA_E2E_HOST is required")
		os.Exit(2)
	}
	code, err := setup(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func sh(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return string(out), nil
}

func onBox(script string) (string, error) {
	return sh("ssh", "-o", "BatchMode=yes", host, script)
}

func setup(m *testing.M) (int, error) {
	root, err := filepath.Abs("..")
	if err != nil {
		return 0, err
	}
	// Socket paths must stay short, so the work dir lives under /tmp.
	work, err := os.MkdirTemp("/tmp", "ra-e2e-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(work)
	binDir = filepath.Join(work, "bin")
	laptopHome = filepath.Join(work, "home")
	sshDir = filepath.Join(work, "sshbin")
	blockFile = filepath.Join(work, "block")
	hookLog = filepath.Join(work, "hooks.log")
	configPath = filepath.Join(work, "config.toml")
	daemonLog = filepath.Join(work, "daemon.log")
	openedLog = filepath.Join(work, "opened.log")
	revealLog = filepath.Join(work, "revealed.log")
	approvalLog = filepath.Join(work, "approvals.log")
	declineFile = filepath.Join(work, "decline")
	for _, d := range []string{binDir, sshDir, laptopHome, filepath.Join(laptopHome, "Downloads")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return 0, err
		}
	}
	// The e2e build asks RA_E2E_APPROVER instead of Touch ID.
	build := exec.Command("go", "build", "-tags", "e2e", "-o", filepath.Join(binDir, "remote-agents"), "./cmd/remote-agents")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("build: %v: %s", err, out)
	}
	for _, agent := range []string{"fakeclaude", "fakecodex", "realclaude", "isolatedclaude"} {
		if err := os.Symlink("remote-agents", filepath.Join(binDir, "r"+agent)); err != nil {
			return 0, err
		}
	}
	if out, err := sh(filepath.Join(root, "scripts", "deploy-box.sh"), host, boxInstall); err != nil {
		return 0, err
	} else {
		fmt.Print(out)
	}
	if boxHome, err = onBox(`printf %s "$HOME"`); err != nil {
		return 0, err
	}
	box = boxlayout.Layout{Home: boxHome, Name: boxInstall}
	if _, err := onBox("mkdir -p " + boxTestDir + "/work"); err != nil {
		return 0, err
	}
	if _, err := onBox(fmt.Sprintf(`printf 'memory_max = "2G"\n' > %q`, box.SessionConfig())); err != nil {
		return 0, err
	}
	for _, fake := range []string{"fake-claude", "fake-codex", "fake-oauth"} {
		if _, err := sh("scp", "-q", "-p", filepath.Join(root, "testdata", fake), host+":"+boxTestDir+"/"+fake); err != nil {
			return 0, err
		}
	}
	// Real Claude Code with its own config dir: onboarding done, workdir trusted, not logged in.
	if _, err := onBox(fmt.Sprintf(`set -e
mkdir -p %[1]s/claude-config
printf '{"hasCompletedOnboarding":true,"theme":"dark","projects":{"%[2]s/%[1]s/work":{"hasTrustDialogAccepted":true}}}' > %[1]s/claude-config/.claude.json
printf '#!/bin/sh\nCLAUDE_CONFIG_DIR=%[2]s/%[1]s/claude-config exec claude "$@"\n' > %[1]s/claude-isolated
chmod +x %[1]s/claude-isolated`, boxTestDir, boxHome)); err != nil {
		return 0, err
	}
	realSSH, err := exec.LookPath("ssh")
	if err != nil {
		return 0, err
	}
	writeExec := func(path, body string) error { return os.WriteFile(path, []byte("#!/bin/bash\n"+body+"\n"), 0o755) }
	// The block file simulates an outage: every new connection fails like an unreachable network.
	if err := writeExec(filepath.Join(sshDir, "ssh"), fmt.Sprintf(`if [ -e %q ]; then sleep 0.5; echo "ssh: connect to host %s port 22: Network is unreachable" >&2; exit 255; fi
exec %q "$@"`, blockFile, host, realSSH)); err != nil {
		return 0, err
	}
	// Tests count hook and prompt lines before one may have come.
	for _, log := range []string{hookLog, approvalLog} {
		if err := os.WriteFile(log, nil, 0o644); err != nil {
			return 0, err
		}
	}
	approver := filepath.Join(work, "approver")
	if err := writeExec(approver, fmt.Sprintf(`printf '%%s\n' "$1" >> %q; [ ! -e %q ]`, approvalLog, declineFile)); err != nil {
		return 0, err
	}
	fakeCmux := filepath.Join(work, "cmux")
	if err := writeExec(fakeCmux, fmt.Sprintf(`echo "$* surface=$CMUX_SURFACE_ID pid=$CMUX_CLAUDE_PID custom=$CMUX_CUSTOM_CLAUDE_PATH codexpid=$CMUX_CODEX_PID launch=$CMUX_AGENT_LAUNCH_KIND stdin=$(cat)" >> %q; printf '{"ok":true}'`, hookLog)); err != nil {
		return 0, err
	}
	fakeWrapper := filepath.Join(work, "cmux-claude-wrapper")
	if err := writeExec(fakeWrapper, fmt.Sprintf(`export CMUX_SURFACE_ID=e2e-surface CMUX_CLAUDE_HOOK_CMUX_BIN=%q CMUX_CLAUDE_PID=$$ CMUX_AGENT_LAUNCH_EXECUTABLE="$CMUX_CUSTOM_CLAUDE_PATH"
exec "$CMUX_CUSTOM_CLAUDE_PATH" --settings '{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"\"${CMUX_CLAUDE_HOOK_CMUX_BIN:-cmux}\" hooks claude session-start"}]}],"Stop":[{"matcher":"","hooks":[{"type":"command","command":"\"${CMUX_CLAUDE_HOOK_CMUX_BIN:-cmux}\" hooks claude stop"}]}]}}' "$@"`, fakeCmux)); err != nil {
		return 0, err
	}
	// Stands in for the browser: records each URL and fetches the laptop-local ones.
	fakeBrowser := filepath.Join(work, "browser")
	if err := writeExec(fakeBrowser, fmt.Sprintf(`echo "$1" >> %q
case "$1" in http://localhost:*) curl -s -m 5 -o %q "$1" ;; esac`, openedLog, filepath.Join(work, "fetched"))); err != nil {
		return 0, err
	}
	config := fmt.Sprintf(`host = %q
box_install = %q
workdir = "%s/%s/work"

[link]
opener = %q
revealer = ["/bin/sh", "-c", "echo \"$0\" >> %s"]

[cmux]
wrapper = %q

[agents.fakeclaude]
command = "%s/%s/fake-claude"
kind = "claude"
args = ["--dangerously-skip-permissions"]

[agents.fakecodex]
command = "%s/%s/fake-codex"
kind = "codex"
args = ["--yolo"]

[agents.realclaude]
command = "claude"
kind = "claude"

[agents.isolatedclaude]
command = "%s/%s/claude-isolated"
kind = "claude"
`, host, boxInstall, boxHome, boxTestDir, fakeBrowser, revealLog, fakeWrapper, boxHome, boxTestDir, boxHome, boxTestDir, boxHome, boxTestDir)
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		return 0, err
	}

	logFile, err := os.Create(daemonLog)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	daemon := exec.Command(filepath.Join(binDir, "remote-agents"), "link")
	daemon.Env = append(testEnv(), "RA_E2E_APPROVER="+approver)
	daemon.Stdout, daemon.Stderr = logFile, logFile
	if err := daemon.Start(); err != nil {
		return 0, err
	}
	daemonPid = daemon.Process.Pid
	exited := make(chan struct{})
	go func() {
		// The log shows why the daemon ended; its exit status adds nothing.
		_ = daemon.Wait()
		close(exited)
	}()
	defer func() {
		daemon.Process.Signal(syscall.SIGTERM)
		<-exited
	}()
	if err := waitFile(daemonLog, "link up", 20*time.Second, exited); err != nil {
		return 0, err
	}
	if err := killTestSessions(); err != nil {
		return 0, err
	}
	code := m.Run()
	return code, killTestSessions()
}

// killTestSessions ends every session of the tests' own install and forgets its reaps.
func killTestSessions() error {
	_, err := onBox(fmt.Sprintf(`set -e
shpool() { %q --socket %q --config-file %q --no-daemonize "$@"; }
for s in $(shpool list | awk 'NR > 1 {print $1}'); do shpool kill "$s"; done
rm -rf %q %q`, box.Shpool(), box.ShpoolSocket(), box.ShpoolConfig(), box.ReapedDir(), box.ReapedSessionsDir()))
	return err
}

func testEnv() []string {
	env := []string{
		"PATH=" + sshDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		// The laptop's own link daemon holds the real state dir.
		"HOME=" + laptopHome,
		"USER=" + os.Getenv("USER"),
		"SSH_AUTH_SOCK=" + os.Getenv("SSH_AUTH_SOCK"),
		"TERM=xterm-256color",
		"LANG=en_US.UTF-8",
		"REMOTE_AGENTS_CONFIG=" + configPath,
	}
	return env
}

func waitFile(path, needle string, timeout time.Duration, exited <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	for {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), needle) {
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("daemon exited:\n%s", data)
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s never contained %q:\n%s", path, needle, data)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?<>=:]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]|\x1b[=>78c]|\r`)

const termCols, termRows = 120, 40

// cursorQuery is the wrapper's cursor position query, which term answers from its screen.
const cursorQuery = "\x1b[6n"

// term is one laptop terminal running a wrapper command.
type term struct {
	t      *testing.T
	cmd    *exec.Cmd
	master *os.File
	mu     sync.Mutex
	out    bytes.Buffer
	// screen is what the terminal shows; the replies it makes to queries are dropped.
	screen   *vt.Emulator
	answered int
	done     chan struct{}
	code     int
}

func start(t *testing.T, name string, args ...string) *term {
	t.Helper()
	return startEnv(t, nil, name, args...)
}

// startEnv starts name with extra added to the test environment.
func startEnv(t *testing.T, extra []string, name string, args ...string) *term {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, name), args...)
	cmd.Env = append(testEnv(), extra...)
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: termRows, Cols: termCols})
	if err != nil {
		t.Fatal(err)
	}
	screen := vt.NewEmulator(termCols, termRows)
	tm := &term{t: t, cmd: cmd, master: master, screen: screen, done: make(chan struct{})}
	replies := screen.InputPipe().(*io.PipeWriter)
	go func() {
		if _, err := io.Copy(io.Discard, screen); err != nil {
			t.Errorf("dropping the screen's replies: %v", err)
		}
	}()
	go func() {
		defer replies.Close()
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			tm.received(buf[:n])
			if err != nil {
				return
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		tm.code = cmd.ProcessState.ExitCode()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			tm.code = 128 + int(ws.Signal())
		}
		_ = err
		close(tm.done)
	}()
	t.Cleanup(func() {
		select {
		case <-tm.done:
		default:
			cmd.Process.Kill()
			<-tm.done
		}
		master.Close()
	})
	return tm
}

// received records output and answers each new cursor position query as a terminal would.
func (tm *term) received(p []byte) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.out.Write(p)
	if _, err := tm.screen.Write(p); err != nil {
		tm.t.Errorf("screen: %v", err)
	}
	queries := bytes.Count(tm.out.Bytes(), []byte(cursorQuery))
	for ; tm.answered < queries; tm.answered++ {
		pos := tm.screen.CursorPosition()
		if _, err := fmt.Fprintf(tm.master, "\x1b[%d;%dR", pos.Y+1, pos.X+1); err != nil {
			tm.t.Errorf("answering the cursor query: %v", err)
		}
	}
}

// text is the output without control sequences or the remote mark.
func (tm *term) text() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return strings.ReplaceAll(ansi.ReplaceAllString(tm.out.String(), ""), corner.Mark, "")
}

// cloud starts the cell that shows the remote mark; the emulator may keep its variation selector apart.
const cloud = "\u2601"

// resize gives the terminal a new size, as dragging its window would.
func (tm *term) resize(cols, rows int) {
	tm.t.Helper()
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.screen.Resize(cols, rows)
	if err := pty.Setsize(tm.master, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
		tm.t.Fatal(err)
	}
}

// marks lists where the screen and its scrollback show the remote mark, as "screen x,y" or "scrollback x,y".
func (tm *term) marks() []string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	var found []string
	for y := range tm.screen.Height() {
		for x := range tm.screen.Width() {
			if c := tm.screen.CellAt(x, y); c != nil && strings.HasPrefix(c.Content, cloud) {
				found = append(found, fmt.Sprintf("screen %d,%d", x, y))
			}
		}
	}
	for y := range tm.screen.ScrollbackLen() {
		for x := range tm.screen.Width() {
			if c := tm.screen.ScrollbackCellAt(x, y); c != nil && strings.HasPrefix(c.Content, cloud) {
				found = append(found, fmt.Sprintf("scrollback %d,%d", x, y))
			}
		}
	}
	return found
}

// cornerOf is where marks finds the remote mark when it is in the corner of a cols by rows screen.
func cornerOf(cols, rows int) []string {
	return []string{fmt.Sprintf("screen %d,%d", cols-2, rows-1)}
}

// expectMarks waits until the screen and scrollback show the remote mark exactly at want.
func (tm *term) expectMarks(want []string, timeout time.Duration) {
	tm.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := tm.marks()
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			tm.t.Fatalf("remote mark at %q, want %q; output:\n%s", got, want, tm.text())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (tm *term) expect(pattern string, timeout time.Duration) []string {
	tm.t.Helper()
	re := regexp.MustCompile(pattern)
	deadline := time.Now().Add(timeout)
	for {
		if m := re.FindStringSubmatch(tm.text()); m != nil {
			return m
		}
		if time.Now().After(deadline) {
			tm.t.Fatalf("terminal never matched %q; output:\n%s", pattern, tm.text())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// expectAfter matches only output produced after mark (a length of text()).
func (tm *term) expectAfter(mark int, pattern string, timeout time.Duration) []string {
	tm.t.Helper()
	re := regexp.MustCompile(pattern)
	deadline := time.Now().Add(timeout)
	for {
		if txt := tm.text(); len(txt) > mark {
			if m := re.FindStringSubmatch(txt[mark:]); m != nil {
				return m
			}
		}
		if time.Now().After(deadline) {
			tm.t.Fatalf("terminal never matched %q after mark; output:\n%s", pattern, tm.text()[mark:])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// drawnAfter waits for a fake agent's session to draw its screen after mark.
// Keys typed before a session draws, while it is still connecting, are dropped.
func (tm *term) drawnAfter(mark int) {
	tm.t.Helper()
	tm.expectAfter(mark, `fake-(claude|codex) session=`, 20*time.Second)
}

func (tm *term) send(s string) {
	tm.t.Helper()
	if _, err := tm.master.Write([]byte(s)); err != nil {
		tm.t.Fatal(err)
	}
}

func (tm *term) wait(timeout time.Duration) int {
	tm.t.Helper()
	select {
	case <-tm.done:
		return tm.code
	case <-time.After(timeout):
		tm.t.Fatalf("wrapper did not exit; output:\n%s", tm.text())
	}
	return 0
}

// sshChild returns the pid of the wrapper's current ssh process.
func (tm *term) sshChild() int {
	tm.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := exec.Command("pgrep", "-P", strconv.Itoa(tm.cmd.Process.Pid), "ssh").Output()
		if err == nil {
			pid, err := strconv.Atoi(strings.Fields(string(out))[0])
			if err == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			tm.t.Fatalf("no ssh child of %d", tm.cmd.Process.Pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func block(t *testing.T, on bool) {
	t.Helper()
	if on {
		if err := os.WriteFile(blockFile, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.Remove(blockFile); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// boxListing is the box's whole ls output, reaped sessions included.
func boxListing(t *testing.T) string {
	t.Helper()
	out, err := onBox(box.Binary() + " ls")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// boxSessions is the part of the box's ls that lists running sessions.
func boxSessions(t *testing.T) string {
	t.Helper()
	running, _, _ := strings.Cut(boxListing(t), "\nREAPED")
	return running
}

const uuidRe = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

func TestSessionLifecycle(t *testing.T) {
	tm := start(t, "rfakeclaude", "--model", "x")
	args := tm.expect(`fake-claude args=(.*)\r?\n`, 20*time.Second)[1]
	m := tm.expect(`fake-claude session=(`+uuidRe+`) pid=(\d+) cwd=(\S+)`, 20*time.Second)
	id, pid, cwd := m[1], m[2], m[3]
	// Configured args go first, before the user's own.
	if !strings.HasSuffix(strings.TrimSpace(args), "--session-id "+id+" --dangerously-skip-permissions --model x") {
		t.Fatalf("agent args = %q", args)
	}
	if cwd != boxHome+"/"+boxTestDir+"/work" {
		t.Fatalf("agent cwd = %s", cwd)
	}
	env := tm.expect(`fake-claude env BROWSER=(\S+) SESSION=(\S+) TERM=(\S+) HOOKBIN=(\S+)`, 5*time.Second)
	if env[1] != filepath.Join(box.Shims(), "xdg-open") || env[2] != "fakeclaude-"+id || env[3] != "xterm-256color" {
		t.Fatalf("agent env = %q", env)
	}
	if !strings.Contains(boxSessions(t), "fakeclaude-"+id) {
		t.Fatal("session not listed on the box")
	}

	tm.send("hello there\r")
	tm.expect(`got: hello there`, 5*time.Second)

	// Abrupt link loss: the session must survive and the wrapper reattach on its own.
	mark := len(tm.text())
	if err := syscall.Kill(tm.sshChild(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	tm.expectAfter(mark, `reconnecting`, 5*time.Second)
	tm.drawnAfter(mark)
	mark = len(tm.text())
	tm.send("whoami\r")
	if got := tm.expectAfter(mark, `fake-claude pid=(\d+)`, 15*time.Second)[1]; got != pid {
		t.Fatalf("agent restarted: pid %s -> %s", pid, got)
	}

	// Explicit exit: the agent's resume hint reaches the laptop and its exit code is ours.
	tm.send("exit 7\r")
	tm.expect(`Resume this session with:\s*claude --resume `+id, 10*time.Second)
	tm.expect(`Resume on the box with: rfakeclaude --resume `+id, 10*time.Second)
	if code := tm.wait(10 * time.Second); code != 7 {
		t.Fatalf("exit code %d, want 7", code)
	}
	if strings.Contains(boxSessions(t), "fakeclaude-"+id) {
		t.Fatal("session still listed after exit")
	}

	// Resume starts the agent again with the same id.
	again := start(t, "rfakeclaude", "--resume", id)
	again.expect(`fake-claude session=`+id, 20*time.Second)
	again.send("exit 0\r")
	if code := again.wait(10 * time.Second); code != 0 {
		t.Fatalf("resume exit %d", code)
	}
}

// The terminal's bottom-right corner shows the remote mark while the session
// runs and after it reattaches, scrolled output leaves no copies of it, and it
// goes when the session ends.
func TestTheCornerMarksTheRemoteSession(t *testing.T) {
	bottomRight := cornerOf(termCols, termRows)
	tm := start(t, "rfakeclaude")
	tm.drawnAfter(0)
	tm.expectMarks(bottomRight, 10*time.Second)

	mark := len(tm.text())
	tm.send("lines 100\r")
	tm.expectAfter(mark, `\n100\n`, 10*time.Second)
	tm.expectMarks(bottomRight, 5*time.Second)

	mark = len(tm.text())
	if err := syscall.Kill(tm.sshChild(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	tm.expectAfter(mark, `reconnecting`, 5*time.Second)
	// Reattaching redraws the session's screen, which still ends with the lines.
	tm.expectAfter(mark, `\n100`, 20*time.Second)
	tm.expectMarks(bottomRight, 5*time.Second)
	mark = len(tm.text())
	tm.send("lines 60\r")
	tm.expectAfter(mark, `\n60\n`, 10*time.Second)
	tm.expectMarks(bottomRight, 5*time.Second)

	tm.send("exit 0\r")
	if code := tm.wait(10 * time.Second); code != 0 {
		t.Fatalf("exit %d", code)
	}
	tm.expectMarks(nil, time.Second)
}

// Claude redraws its whole screen on a resize, and the mark comes back in
// the new corner. The emulator does not reflow as Ghostty does, so this
// shows the redraw, not that Ghostty's moved copy is erased.
func TestRealClaudeGetsTheMarkInTheCornerAfterAResize(t *testing.T) {
	tm := start(t, "risolatedclaude")
	tm.expectCompact(0, "Not logged in", 30*time.Second)
	tm.expectMarks(cornerOf(termCols, termRows), 10*time.Second)
	tm.resize(termCols+10, termRows+10)
	tm.expectMarks(cornerOf(termCols+10, termRows+10), 10*time.Second)
	tm.send("\x03")
	time.Sleep(300 * time.Millisecond)
	tm.send("\x03")
	time.Sleep(300 * time.Millisecond)
	tm.send("\x03")
	tm.wait(15 * time.Second)
}

func TestASessionOverItsMemoryCapLosesOnlyTheProcessThatOverran(t *testing.T) {
	tm := start(t, "rfakeclaude")
	m := tm.expect(`fake-claude session=(`+uuidRe+`) pid=(\d+)`, 20*time.Second)
	id, pid := m[1], m[2]
	out, err := onBox(fmt.Sprintf(`set -e
unit=$(sed -n 's|^0::.*/||p' /proc/%s/cgroup)
echo "$unit"
systemctl --user show "$unit" -p MemoryMax -p MemorySwapMax -p OOMPolicy -p BindsTo -p StopPropagatedFrom`, pid))
	if err != nil {
		t.Fatal(err)
	}
	unit, props, _ := strings.Cut(out, "\n")
	if !strings.HasPrefix(unit, box.Name+"-fakeclaude-"+id+"-") || !strings.HasSuffix(unit, ".scope") {
		t.Fatalf("agent runs in unit %q", unit)
	}
	for _, want := range []string{"MemoryMax=2147483648", "MemorySwapMax=0", "OOMPolicy=continue", "BindsTo=" + box.ShpoolService(), "StopPropagatedFrom=" + box.ShpoolService()} {
		if !strings.Contains(props, want+"\n") {
			t.Fatalf("scope %s lacks %s:\n%s", unit, want, props)
		}
	}

	tm.send("hog\r")
	tm.expect(`hog exit=137`, 30*time.Second)
	mark := len(tm.text())
	tm.send("whoami\r")
	if got := tm.expectAfter(mark, `fake-claude pid=(\d+)`, 5*time.Second)[1]; got != pid {
		t.Fatalf("agent restarted: pid %s -> %s", pid, got)
	}
	tm.send("exit 0\r")
	if code := tm.wait(10 * time.Second); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestARunWhoseLauncherWasKilledResumesItsSessionOnReconnect(t *testing.T) {
	tm := start(t, "rfakeclaude")
	m := tm.expect(`fake-claude session=(`+uuidRe+`) pid=(\d+)`, 20*time.Second)
	id, pid := m[1], m[2]
	block(t, true)
	defer block(t, false)
	if err := syscall.Kill(tm.sshChild(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	tm.expect(`network unreachable`, 10*time.Second)
	// As a kill of the whole shpool unit did: the launcher dies with its agent and writes no ended marker.
	if _, err := onBox(fmt.Sprintf(`set -e
kill -KILL $(cat "/sys/fs/cgroup$(sed -n 's|^0::||p' /proc/%s/cgroup)/cgroup.procs")
while [ -e /proc/%s ]; do sleep 0.1; done`, pid, pid)); err != nil {
		t.Fatal(err)
	}
	mark := len(tm.text())
	block(t, false)
	args := tm.expectAfter(mark, `fake-claude args=(.*)\r?\n`, 20*time.Second)[1]
	if !strings.Contains(args, "--resume "+id) || strings.Contains(args, "--session-id") {
		t.Fatalf("relaunch args = %q, want a resume of %s", args, id)
	}
	tm.expectAfter(mark, `fake-claude session=`+id+` `, 5*time.Second)
	tm.send("exit 0\r")
	if code := tm.wait(10 * time.Second); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestLosingTheShpoolDaemonResumesItsSessionsOnTheNewOne(t *testing.T) {
	for name, lose := range map[string]string{
		"restart": "systemctl --user restart " + box.ShpoolService(),
		"crash":   "systemctl --user kill -s KILL --kill-whom=main " + box.ShpoolService(),
	} {
		t.Run(name, func(t *testing.T) {
			tm := start(t, "rfakeclaude")
			m := tm.expect(`fake-claude session=(`+uuidRe+`) pid=(\d+)`, 20*time.Second)
			id, pid := m[1], m[2]
			mark := len(tm.text())
			if _, err := onBox(lose); err != nil {
				t.Fatal(err)
			}
			args := tm.expectAfter(mark, `fake-claude args=(.*)\r?\n`, 20*time.Second)[1]
			if !strings.Contains(args, "--resume "+id) {
				t.Fatalf("args after losing the daemon = %q, want a resume of %s", args, id)
			}
			again := tm.expectAfter(mark, `fake-claude session=(`+uuidRe+`) pid=(\d+)`, 5*time.Second)
			if again[1] != id || again[2] == pid {
				t.Fatalf("after losing the daemon: session %s pid %s, was %s pid %s", again[1], again[2], id, pid)
			}
			if out, err := onBox(fmt.Sprintf("test ! -e /proc/%s && echo gone", pid)); err != nil || out != "gone\n" {
				t.Fatalf("the old agent %s outlived its daemon: %q, %v", pid, out, err)
			}
			tm.send("exit 0\r")
			if code := tm.wait(10 * time.Second); code != 0 {
				t.Fatalf("exit %d", code)
			}
		})
	}
}

func TestDetachWhileDownThenResumeReattaches(t *testing.T) {
	tm := start(t, "rfakeclaude")
	m := tm.expect(`fake-claude session=(`+uuidRe+`) pid=(\d+)`, 20*time.Second)
	id, pid := m[1], m[2]

	block(t, true)
	defer block(t, false)
	if err := syscall.Kill(tm.sshChild(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	tm.expect(`network unreachable`, 10*time.Second)
	tm.send("\x03")
	tm.expect(`reattach with: rfakeclaude --resume `+id, 5*time.Second)
	if code := tm.wait(5 * time.Second); code != 130 {
		t.Fatalf("detach exit %d, want 130", code)
	}
	if !strings.Contains(boxSessions(t), "fakeclaude-"+id) {
		t.Fatal("session did not survive the detach")
	}
	block(t, false)

	again := start(t, "rfakeclaude", "--resume", id)
	again.drawnAfter(0)
	again.send("whoami\r")
	if got := again.expect(`fake-claude pid=(\d+)`, 20*time.Second)[1]; got != pid {
		t.Fatalf("resume started a second agent: pid %s -> %s", pid, got)
	}
	again.send("exit 3\r")
	if code := again.wait(10 * time.Second); code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
}

func TestSessionEndingDuringOutageReportsItsExit(t *testing.T) {
	tm := start(t, "rfakeclaude")
	tm.expect(`fake-claude session=(`+uuidRe+`)`, 20*time.Second)
	tm.send("exitin 2 5\r")
	block(t, true)
	defer block(t, false)
	if err := syscall.Kill(tm.sshChild(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	tm.expect(`network unreachable`, 10*time.Second)
	time.Sleep(4 * time.Second)
	block(t, false)
	tm.expect(`ended while the link was down \(exit code 5\)`, 15*time.Second)
	if code := tm.wait(10 * time.Second); code != 5 {
		t.Fatalf("exit %d, want 5", code)
	}
	if strings.Contains(tm.text()[strings.Index(tm.text(), "ended while"):], "fake-claude session=") {
		t.Fatal("the agent was started again after it ended")
	}
}

func TestClosingTheTerminalDetaches(t *testing.T) {
	tm := start(t, "rfakeclaude")
	id := tm.expect(`fake-claude session=(`+uuidRe+`)`, 20*time.Second)[1]
	if err := tm.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if code := tm.wait(10 * time.Second); code != 129 {
		t.Fatalf("exit %d, want 129", code)
	}
	time.Sleep(time.Second)
	if !strings.Contains(boxSessions(t), "fakeclaude-"+id) {
		t.Fatal("session did not survive SIGHUP")
	}
	ls := start(t, "remote-agents", "ls")
	ls.expect(`fakeclaude-`+id+`\s+detached`, 15*time.Second)
	ls.wait(10 * time.Second)

	back := start(t, "remote-agents", "attach", "fakeclaude-"+id)
	back.drawnAfter(0)
	back.send("exit 0\r")
	back.expect(`claude --resume `+id, 15*time.Second)
	if code := back.wait(10 * time.Second); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestOneShotCommandUsesRealClaude(t *testing.T) {
	tm := start(t, "rrealclaude", "--version")
	tm.expect(`\d+\.\d+\.\d+ \(Claude Code\)`, 30*time.Second)
	if code := tm.wait(10 * time.Second); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestHookRelayReachesTheTerminal(t *testing.T) {
	tm := start(t, "rfakeclaude")
	m := tm.expect(`fake-claude env BROWSER=\S+ SESSION=(\S+) TERM=\S+ HOOKBIN=(\S+)`, 20*time.Second)
	if m[2] != filepath.Join(box.HookBin(), "cmux") {
		t.Fatalf("hook bin = %s", m[2])
	}
	tm.send("hook stop\r")
	tm.expect(`\{"ok":true\}hook exit=0`, 10*time.Second)
	data, err := os.ReadFile(hookLog)
	if err != nil {
		t.Fatal(err)
	}
	wantPID := "pid=" + strconv.Itoa(tm.cmd.Process.Pid)
	// cmux restores the session with the claude binary SessionStart reports;
	// other hooks keep the terminal's own value, here none.
	if !strings.Contains(string(data), "hooks claude session-start surface=e2e-surface "+wantPID+" custom="+filepath.Join(binDir, "rfakeclaude")+" ") ||
		!strings.Contains(string(data), "hooks claude stop surface=e2e-surface "+wantPID+" custom= ") || !strings.Contains(string(data), `"hook_event_name":"stop"`) {
		t.Fatalf("hook log:\n%s", data)
	}
	tm.send("exit 0\r")
	tm.wait(10 * time.Second)
}

func TestLinkServesClipboard(t *testing.T) {
	out, err := onBox(box.Shims() + "/xclip -selection clipboard -t TARGETS -o")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "TARGETS\n") {
		t.Fatalf("TARGETS = %q", out)
	}
}

func openedURLs(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(openedLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestOpeningLoginURLsForwardsTheirCallbackPorts(t *testing.T) {
	if out, err := onBox(box.Shims() + "/xdg-open file:///etc/passwd"); err == nil || !strings.Contains(out, "only http(s)") {
		t.Fatalf("file url should be refused: %q, %v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", host, fmt.Sprintf(`cd %s && {
timeout 60 python3 -m http.server 18000 --bind 127.0.0.1 & timeout 60 python3 -m http.server 18001 --bind 127.0.0.1 & echo $! > 18001.pid; wait; }`, boxTestDir))
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		server.Wait()
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := onBox("curl -sf -o /dev/null http://127.0.0.1:18000/ && curl -sf -o /dev/null http://127.0.0.1:18001/"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("box servers never came up")
		}
		time.Sleep(300 * time.Millisecond)
	}

	// kubelogin style: the url itself is laptop-local, and the browser fetches it at once.
	if out, err := onBox(box.Shims() + "/xdg-open http://localhost:18000/fake-claude"); err != nil {
		t.Fatalf("opening a localhost url: %v %s", err, out)
	}
	fetched, err := os.ReadFile(filepath.Join(filepath.Dir(openedLog), "fetched"))
	if err != nil || !strings.Contains(string(fetched), "fake-claude session=") {
		t.Fatalf("browser did not reach the box server: %v %q", err, fetched)
	}
	// A retried login reuses the running forward.
	if out, err := onBox(box.Shims() + "/xdg-open http://localhost:18000/fake-claude"); err != nil {
		t.Fatalf("reopening a forwarded port: %v %s", err, out)
	}
	if err := waitFile(daemonLog, "forward localhost:18000 restarted", 5*time.Second, nil); err != nil {
		t.Fatal(err)
	}

	// bao style: the callback is the redirect_uri of an external url.
	if out, err := onBox(box.Shims() + `/xdg-open 'https://example.com/auth?client_id=x&redirect_uri=http%3A%2F%2Flocalhost%3A18001%2Foidc%2Fcallback'`); err != nil {
		t.Fatalf("opening a url with a loopback redirect_uri: %v %s", err, out)
	}
	if !strings.Contains(openedURLs(t), "https://example.com/auth?client_id=x&redirect_uri=") {
		t.Fatalf("external url not opened:\n%s", openedURLs(t))
	}
	body, err := exec.Command("curl", "-s", "-m", "5", "http://localhost:18001/fake-claude").Output()
	if err != nil || !strings.Contains(string(body), "fake-claude session=") {
		t.Fatalf("redirect_uri port not forwarded: %v %q", err, body)
	}
	// The 18001 server stops, like a login server once its callback arrived.
	if out, err := onBox("kill $(cat " + boxTestDir + "/18001.pid)"); err != nil {
		t.Fatalf("stopping the 18001 server: %v %s", err, out)
	}
	if err := waitFile(daemonLog, "forward localhost:18001 closed: the box stopped listening", 30*time.Second, nil); err != nil {
		t.Fatal(err)
	}

	// A laptop program holding the port wins; the url is not opened.
	busy, err := net.Listen("tcp4", "127.0.0.1:18002")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	out, err := onBox(box.Shims() + "/xdg-open http://localhost:18002/")
	if err == nil || !strings.Contains(out, "laptop port 18002 is in use") {
		t.Fatalf("busy laptop port should refuse the url: %q, %v", out, err)
	}
	if strings.Contains(openedURLs(t), "18002") {
		t.Fatalf("refused url was opened:\n%s", openedURLs(t))
	}
}

var redirectPort = regexp.MustCompile(`redirect_uri=http%3A%2F%2Flocalhost%3A(\d+)%2F`)

// Goes as far as Claude's own callback server; completing a login needs a person.
func TestRealClaudeLoginCallbackReachesTheBox(t *testing.T) {
	tm := start(t, "risolatedclaude")
	tm.expectCompact(0, "Not logged in", 30*time.Second)
	before := len(openedURLs(t))
	tm.send("/login")
	time.Sleep(500 * time.Millisecond)
	tm.send("\r")
	time.Sleep(time.Second)
	tm.send("\r")
	deadline := time.Now().Add(20 * time.Second)
	var port string
	for {
		if m := redirectPort.FindStringSubmatch(openedURLs(t)[before:]); m != nil {
			port = m[1]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("claude opened no url with a loopback redirect_uri:\n%s\nscreen:\n%s", openedURLs(t)[before:], tail(tm.text(), 1500))
		}
		time.Sleep(200 * time.Millisecond)
	}
	code, err := exec.Command("curl", "-s", "-m", "5", "-o", "/dev/null", "-w", "%{http_code}", "http://localhost:"+port+"/callback").Output()
	if err != nil || string(code) == "000" {
		t.Fatalf("claude's callback port %s not reachable from the laptop: %v %q", port, err, code)
	}
	t.Logf("claude callback on port %s answered HTTP %s", port, code)
	tm.send("\x1b")
	time.Sleep(300 * time.Millisecond)
	for range 3 {
		tm.send("\x03")
		time.Sleep(300 * time.Millisecond)
	}
	tm.wait(15 * time.Second)
}

func TestLinkRecoversAndBoxFailsFastWhileDown(t *testing.T) {
	sshPid := func() int {
		// Only the test daemon's own ssh: the laptop's real link daemon runs a link-hold too.
		out, err := exec.Command("pgrep", "-P", strconv.Itoa(daemonPid), "-f", "link-hold").Output()
		if err != nil {
			t.Fatalf("no link ssh: %v", err)
		}
		pid, err := strconv.Atoi(strings.Fields(string(out))[0])
		if err != nil {
			t.Fatal(err)
		}
		return pid
	}
	before, _ := os.ReadFile(daemonLog)
	ups := strings.Count(string(before), "link up")

	// A frozen ssh looks like a half-dead connection: the box must notice from missing heartbeats.
	pid := sshPid()
	block(t, true)
	defer block(t, false)
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	// A failure below must not leave the link frozen for the tests after it.
	defer syscall.Kill(pid, syscall.SIGCONT)
	defer syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(8 * time.Second)
	out, err := onBox(box.Shims() + "/xclip -selection clipboard -t TARGETS -o")
	if err == nil || !strings.Contains(out, "laptop link is down") {
		t.Fatalf("box should fail fast with link down: %q, %v", out, err)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	syscall.Kill(pid, syscall.SIGCONT)
	block(t, false)
	deadline := time.Now().Add(20 * time.Second)
	for {
		data, _ := os.ReadFile(daemonLog)
		if strings.Count(string(data), "link up") > ups {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("link did not come back:\n%s", data)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := onBox(box.Shims() + "/xclip -selection clipboard -t TARGETS -o"); err != nil {
		t.Fatalf("clipboard after recovery: %v", err)
	}
	socks, err := onBox("ls " + filepath.Dir(box.LaptopSocket()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(socks, "laptop.sock\n") {
		t.Fatalf("laptop socket missing after recovery:\n%s", socks)
	}
}

// expectCompact matches literal text after mark, ignoring whitespace: Claude's
// renderer moves the cursor instead of printing spaces.
func (tm *term) expectCompact(mark int, literal string, timeout time.Duration) {
	tm.t.Helper()
	squash := func(s string) string { return strings.Join(strings.Fields(s), "") }
	want := squash(literal)
	deadline := time.Now().Add(timeout)
	for {
		if txt := tm.text(); len(txt) > mark && strings.Contains(squash(txt[mark:]), want) {
			return
		}
		if time.Now().After(deadline) {
			tm.t.Fatalf("terminal never showed %q after mark; output:\n%s", literal, tm.text()[mark:])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRealClaudeTUISurvivesDrop(t *testing.T) {
	tm := start(t, "risolatedclaude")
	tm.expectCompact(0, "Not logged in", 30*time.Second)
	tm.send("typed before drop")
	tm.expectCompact(0, "typed before drop", 10*time.Second)

	mark := len(tm.text())
	if err := syscall.Kill(tm.sshChild(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	tm.expectAfter(mark, `reconnecting`, 5*time.Second)
	// shpool redraws the screen on reattach, including the unsent input.
	tm.expectCompact(mark, "typed before drop", 15*time.Second)
	mark = len(tm.text())
	tm.send(" and after")
	tm.expectCompact(mark, "and after", 10*time.Second)

	// Claude exits on a double Ctrl+C; its exit code is the wrapper's.
	tm.send("\x03")
	time.Sleep(300 * time.Millisecond)
	tm.send("\x03")
	time.Sleep(300 * time.Millisecond)
	tm.send("\x03")
	if code := tm.wait(15 * time.Second); code != 0 {
		t.Fatalf("exit %d; output:\n%s", code, tm.text())
	}
	t.Logf("tail after exit:\n%s", tail(tm.text(), 600))
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// Replaces the laptop clipboard for a few seconds, so it only runs when asked.
func TestRealClaudePastesLaptopClipboardImage(t *testing.T) {
	if os.Getenv("RA_E2E_CLIPBOARD") != "1" {
		t.Skip("set RA_E2E_CLIPBOARD=1 to let this test replace the clipboard briefly")
	}
	saved, err := exec.Command("pbpaste").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		restore := exec.Command("pbcopy")
		restore.Stdin = bytes.NewReader(saved)
		if err := restore.Run(); err != nil {
			t.Errorf("restoring clipboard: %v", err)
		}
	})
	png := filepath.Join(t.TempDir(), "dot.png")
	if err := os.WriteFile(png, onePixelPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := sh("osascript", "-e", fmt.Sprintf(`set the clipboard to (read (POSIX file %q) as «class PNGf»)`, png)); err != nil {
		t.Fatalf("setting clipboard image: %v %s", err, out)
	}
	tm := start(t, "risolatedclaude")
	tm.expectCompact(0, "Not logged in", 30*time.Second)
	mark := len(tm.text())
	tm.send("\x16")
	tm.expectCompact(mark, "[Image #1]", 15*time.Second)
	tm.send("\x03")
	time.Sleep(300 * time.Millisecond)
	tm.send("\x03")
	time.Sleep(300 * time.Millisecond)
	tm.send("\x03")
	tm.wait(15 * time.Second)
}

// A pasted laptop image path, as cmux pastes a clipboard image or a terminal a
// dropped file, reaches Claude on the box as the path of an uploaded copy. The
// name is a macOS screenshot's, with a narrow no-break space before PM.
func TestRealClaudeAttachesAPastedLaptopImagePath(t *testing.T) {
	const name = "Screenshot 2026-11-03 at 4.52.10\u202fPM.png"
	shot := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(shot, onePixelPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	tm := start(t, "risolatedclaude")
	tm.expectCompact(0, "Not logged in", 30*time.Second)
	mark := len(tm.text())
	tm.send("\x1b[200~" + strings.ReplaceAll(shot, " ", `\ `) + "\x1b[201~")
	tm.expectCompact(mark, "[Image #1]", 15*time.Second)
	digest := sha256.Sum256(onePixelPNG)
	sum := hex.EncodeToString(digest[:])
	out, err := onBox(fmt.Sprintf("sha256sum '%s/.local/state/%s/pasted/%s/%s'", boxHome, boxInstall, sum, name))
	if err != nil || !strings.HasPrefix(out, sum+" ") {
		t.Fatalf("the upload is not on the box: %v %s", err, out)
	}
	tm.send("\x03")
	time.Sleep(300 * time.Millisecond)
	tm.send("\x03")
	time.Sleep(300 * time.Millisecond)
	tm.send("\x03")
	tm.wait(15 * time.Second)
}

// Any pasted laptop file the box lacks reaches the session as the path of an
// uploaded copy under its own name. A path the box has stays as typed, as
// does one that cannot upload, with a notice saying why.
func TestPastedLaptopPathsReachTheSessionAsTheBoxShouldSeeThem(t *testing.T) {
	dir := t.TempDir()
	notes := filepath.Join(dir, "report notes.csv")
	content := []byte("a,b\n1,2\n")
	if err := os.WriteFile(notes, content, 0o644); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(dir, "folder")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	tm := start(t, "rfakeclaude")
	tm.drawnAfter(0)
	mark := len(tm.text())
	// /tmp is on the laptop and the box both.
	tm.send("\x1b[200~" + strings.ReplaceAll(notes, " ", `\ `) + " /tmp " + folder + "\x1b[201~\r")
	// The session gets a copy of its own, under the file's name.
	got := tm.expectAfter(mark, `got: (\S+)\\ (notes\.csv) /tmp `+regexp.QuoteMeta(folder), 15*time.Second)
	boxCopy := got[1] + " " + got[2]
	if dir := filepath.Dir(filepath.Dir(boxCopy)); filepath.Dir(dir) != box.SessionPastesDir() || !strings.HasPrefix(filepath.Base(dir), "fakeclaude-") || filepath.Base(boxCopy) != "report notes.csv" {
		t.Fatalf("pasted as %s", boxCopy)
	}
	tm.expectAfter(mark, "not uploaded: folder is a directory", 5*time.Second)
	digest := sha256.Sum256(content)
	sum := hex.EncodeToString(digest[:])
	out, err := onBox(fmt.Sprintf("sha256sum '%s'", boxCopy))
	if err != nil || !strings.HasPrefix(out, sum+" ") {
		t.Fatalf("the upload is not on the box: %v %s", err, out)
	}
	tm.send("exit 0\r")
	if code := tm.wait(15 * time.Second); code != 0 {
		t.Fatalf("exit %d; output:\n%s", code, tm.text())
	}
}

// An agent's save of a pasted laptop file goes back over that file, the old
// version to the Trash, without asking. Once the laptop's copy changed since,
// a save goes beside it instead, with a notice. A hidden file is written back
// once the user approves, then without asking. laptop-save asks for each new
// place, ~/Downloads included, and a declined one fails.
func TestAgentSavesReachTheLaptop(t *testing.T) {
	t.Cleanup(func() { emptyTrashed(t) })
	docs := filepath.Join(laptopHome, "docs")
	hidden := filepath.Join(laptopHome, "Downloads", ".ra-test")
	for _, dir := range []string{docs, hidden} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	notes := filepath.Join(docs, "notes.txt")
	if err := os.WriteFile(notes, []byte("laptop-v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(hidden, "conf.txt")
	if err := os.WriteFile(conf, []byte("laptop-conf"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm := start(t, "rfakeclaude")
	tm.drawnAfter(0)
	mark := len(tm.text())
	tm.send("\x1b[200~" + notes + "\x1b[201~\r")
	boxCopy := tm.expectAfter(mark, `got: (\S+/session-pastes/\S+/notes\.txt)`, 15*time.Second)[1]
	session := filepath.Base(filepath.Dir(filepath.Dir(boxCopy)))
	prompts := approvals(t)

	tm.send("edit " + boxCopy + " agent-v2\r")
	waitContent(t, notes, "agent-v2", 10*time.Second)

	if err := os.WriteFile(notes, []byte("edited-on-the-laptop"), 0o644); err != nil {
		t.Fatal(err)
	}
	mark = len(tm.text())
	tm.send("edit " + boxCopy + " agent-v3\r")
	waitContent(t, filepath.Join(docs, "notes (box).txt"), "agent-v3", 10*time.Second)
	tm.expectAfter(mark, regexp.QuoteMeta("[remote-agents] notes.txt changed on the laptop"), 10*time.Second)
	if data, err := os.ReadFile(notes); err != nil || string(data) != "edited-on-the-laptop" {
		t.Fatalf("the laptop's edit became %q, %v", data, err)
	}
	if got := approvals(t)[len(prompts):]; len(got) != 0 {
		t.Fatalf("plain write-backs asked %q", got)
	}

	mark = len(tm.text())
	tm.send("\x1b[200~" + conf + "\x1b[201~\r")
	confCopy := tm.expectAfter(mark, `got: (\S+/session-pastes/\S+/conf\.txt)`, 15*time.Second)[1]
	tm.send("edit " + confCopy + " agent-conf\r")
	waitContent(t, conf, "agent-conf", 10*time.Second)
	tm.send("edit " + confCopy + " agent-conf-2\r")
	waitContent(t, conf, "agent-conf-2", 10*time.Second)
	// The prompt names the file where the write lands: /tmp is /private/tmp.
	confPath, err := filepath.EvalSymlinks(conf)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"write back the edits of box session " + session + " to " + strconv.Quote(confPath) + " (hidden): replace it now, the old version to the Trash"}
	if got := approvals(t)[len(prompts):]; !slices.Equal(got, want) {
		t.Fatalf("asked %q, want %q", got, want)
	}

	mark = len(tm.text())
	tm.send("save " + boxCopy + "\r")
	saved := tm.expectAfter(mark, `saved to (\S+) on the laptop`, 15*time.Second)[1]
	if filepath.Base(filepath.Dir(saved)) != "Downloads" || filepath.Base(saved) != "notes.txt" {
		t.Fatalf("saved to %s", saved)
	}
	waitContent(t, saved, "agent-v3", time.Second)
	waitContent(t, revealLog, saved+"\n", 5*time.Second)
	want = append(want, "create "+strconv.Quote(saved)+" with a file from box session "+session)
	if got := approvals(t)[len(prompts):]; !slices.Equal(got, want) {
		t.Fatalf("asked %q, want %q", got, want)
	}

	if err := os.WriteFile(declineFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A failure before the test removes it must not decline the later tests' prompts.
	t.Cleanup(func() { _ = os.Remove(declineFile) })
	mark = len(tm.text())
	tm.send("save " + boxCopy + " ~/declined.txt\r")
	tm.expectAfter(mark, `not approved`, 15*time.Second)
	tm.expectAfter(mark, `save exit=1`, 5*time.Second)
	if _, err := os.Stat(filepath.Join(laptopHome, "declined.txt")); !os.IsNotExist(err) {
		t.Fatalf("a declined save landed: %v", err)
	}
	// Asking again at once is refused without a prompt, though the user would now say yes.
	if err := os.Remove(declineFile); err != nil {
		t.Fatal(err)
	}
	prompted := len(approvals(t))
	mark = len(tm.text())
	tm.send("save " + boxCopy + " ~/declined.txt\r")
	tm.expectAfter(mark, `you declined`, 15*time.Second)
	tm.expectAfter(mark, `save exit=1`, 5*time.Second)
	if got := approvals(t)[prompted:]; len(got) != 0 {
		t.Fatalf("asked %q right after a decline", got)
	}

	tm.send("exit 0\r")
	if code := tm.wait(15 * time.Second); code != 0 {
		t.Fatalf("exit %d; output:\n%s", code, tm.text())
	}
}

// approvals lists the prompts the daemon has shown.
func approvals(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(approvalLog)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// waitContent waits for path to hold content.
func waitContent(t *testing.T, path, content string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == content {
			return
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(daemonLog)
			t.Fatalf("%s holds %q, %v; want %q; daemon log:\n%s", path, data, err, content, log)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// emptyTrashed removes from the Trash what the daemon moved there from the tests' laptop home.
func emptyTrashed(t *testing.T) {
	data, err := os.ReadFile(daemonLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range trashedLine.FindAllStringSubmatch(string(data), -1) {
		if strings.Contains(m[1], filepath.Base(filepath.Dir(laptopHome))) {
			if err := os.Remove(m[2]); err != nil && !os.IsNotExist(err) {
				t.Error(err)
			}
		}
	}
}

var trashedLine = regexp.MustCompile(`moved (.+) to the Trash as (.+)\n`)

// Laptops built before paste-files keep uploading pasted images with paste-store.
func TestTheBoxStillStoresImagesForOlderLaptops(t *testing.T) {
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", host, `exec "$HOME/`+boxlayout.BinaryRel(boxInstall)+`" paste-store`)
	cmd.Stdin = strings.NewReader(fmt.Sprintf("%d\n%s", len(onePixelPNG), onePixelPNG))
	out, err := cmd.CombinedOutput()
	digest := sha256.Sum256(onePixelPNG)
	want := fmt.Sprintf("%s/.local/state/%s/pasted/%s.png\n", boxHome, boxInstall, hex.EncodeToString(digest[:]))
	if err != nil || string(out) != want {
		t.Fatalf("paste-store = %q, %v; want %q", out, err, want)
	}
}

var onePixelPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0xda, 0x63, 0xfc, 0xcf, 0xc0, 0x50,
	0x0f, 0x00, 0x04, 0x85, 0x01, 0x80, 0x84, 0xa9, 0x8c, 0x21, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
	0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func TestResumeFromAnotherTerminalTakesOver(t *testing.T) {
	first := start(t, "rfakeclaude")
	m := first.expect(`fake-claude session=(`+uuidRe+`) pid=(\d+)`, 20*time.Second)
	id, pid := m[1], m[2]
	second := start(t, "rfakeclaude", "--resume", id)
	first.expect(`another terminal took over fakeclaude-`+id, 20*time.Second)
	if code := first.wait(10 * time.Second); code != 75 {
		t.Fatalf("first terminal exit %d, want 75", code)
	}
	second.drawnAfter(0)
	second.send("whoami\r")
	if got := second.expect(`fake-claude pid=(\d+)`, 15*time.Second)[1]; got != pid {
		t.Fatalf("takeover started a second agent: %s -> %s", pid, got)
	}
	second.send("exit 0\r")
	if code := second.wait(10 * time.Second); code != 0 {
		t.Fatalf("second terminal exit %d", code)
	}
	if strings.Contains(second.text(), "took over") {
		t.Fatalf("clean exit reported as takeover:\n%s", second.text())
	}
}

// cmux restores a tab by running its claude wrapper with the recorded binary
// and --resume; the wrapper has injected its hooks by the time rclaude starts.
func TestCmuxRestoreResumesOnTheBox(t *testing.T) {
	first := start(t, "rfakeclaude")
	id := first.expect(`fake-claude session=(`+uuidRe+`)`, 20*time.Second)[1]
	first.send("exit 0\r")
	first.wait(10 * time.Second)

	restore := filepath.Join(binDir, "cmux-restore")
	if err := os.WriteFile(restore, []byte(fmt.Sprintf("#!/bin/sh\nCMUX_CUSTOM_CLAUDE_PATH=%q exec %q --resume %s\n",
		filepath.Join(binDir, "rfakeclaude"), filepath.Join(filepath.Dir(hookLog), "cmux-claude-wrapper"), id)), 0o755); err != nil {
		t.Fatal(err)
	}
	tm := start(t, "cmux-restore")
	args := tm.expect(`fake-claude args=(.*)\r?\n`, 20*time.Second)[1]
	tm.expect(`fake-claude session=`+id, 10*time.Second)
	if n := strings.Count(args, "hooks claude session-start"); n != 1 {
		t.Fatalf("restored run got cmux's hooks %d times: %q", n, args)
	}
	mark := countLines(t, hookLog, "hooks claude stop ")
	tm.send("hook stop\r")
	tm.expect(`hook exit=0`, 10*time.Second)
	if got := countLines(t, hookLog, "hooks claude stop "); got != mark+1 {
		t.Fatalf("one hook ran %d times", got-mark)
	}
	tm.send("exit 0\r")
	tm.expect(`Resume on the box with: rfakeclaude --resume `+id, 10*time.Second)
	tm.wait(10 * time.Second)
}

func countLines(t *testing.T, path, needle string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), needle)
}

// With --continue Claude picks the session; only a hook payload names it.
func TestContinueLearnsTheSessionFromHooks(t *testing.T) {
	const picked = "0f1e2d3c-4b5a-4697-8877-665544332211"
	tm := start(t, "rfakeclaude", "--continue")
	tm.expect(`fake-claude session= pid=`, 20*time.Second)
	tm.send("adopt " + picked + "\r")
	tm.expect(`fake-claude adopted `+picked, 5*time.Second)
	tm.send("hook session-start SessionStart\r")
	tm.expect(`hook exit=0`, 10*time.Second)
	// A late hook of an earlier session must not move the id back.
	tm.send("adopt 11111111-2222-4333-8444-555555555555\r")
	tm.expect(`fake-claude adopted 1111`, 5*time.Second)
	tm.send("hook stop Stop\r")
	tm.expect(`hook exit=0`, 10*time.Second)
	tm.send("exit 0\r")
	tm.expect(`Resume on the box with: rfakeclaude --resume `+picked, 10*time.Second)
	tm.wait(10 * time.Second)

	// Without a hook the id is unknown, so no resume line rather than a wrong one.
	quiet := start(t, "rfakeclaude", "--continue")
	quiet.expect(`fake-claude session= pid=`, 20*time.Second)
	quiet.send("exit 0\r")
	if code := quiet.wait(10 * time.Second); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(quiet.text(), "Resume on the box") {
		t.Fatalf("resume line without a known id:\n%s", quiet.text())
	}
}

func TestHookRelayRefusesHooksCmuxDidNotInject(t *testing.T) {
	tm := start(t, "rfakeclaude")
	tm.expect(`fake-claude session=`, 20*time.Second)
	tm.send("hook notification\r")
	tm.expect(`not a hook cmux injected`, 10*time.Second)
	tm.expect(`hook exit=1`, 5*time.Second)
	tm.send("exit 0\r")
	tm.wait(10 * time.Second)
}

// shortReaper makes sessions started during the test end after 5 s idle while detached.
func shortReaper(t *testing.T) {
	t.Helper()
	if _, err := onBox(fmt.Sprintf(`printf 'idle_after = "5s"\npoll = "1s"\n' > %q`, box.ReaperConfig())); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := onBox("rm -f " + box.ReaperConfig()); err != nil {
			t.Error(err)
		}
	})
}

// dropLink cuts the terminal's link so the box sees the session detached.
func dropLink(t *testing.T, tm *term) {
	t.Helper()
	block(t, true)
	if err := syscall.Kill(tm.sshChild(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	tm.expect(`network unreachable`, 10*time.Second)
}

func waitGone(t *testing.T, session string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for strings.Contains(boxSessions(t), session) {
		if time.Now().After(deadline) {
			t.Fatalf("%s still running:\n%s", session, boxSessions(t))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestIdleDetachedSessionIsReapedThenResumedOnReconnect(t *testing.T) {
	shortReaper(t)
	tm := start(t, "rfakeclaude")
	m := tm.expect(`fake-claude session=(`+uuidRe+`) pid=(\d+)`, 20*time.Second)
	id, pid := m[1], m[2]
	for _, cmd := range []string{"prompt", "stop"} {
		mark := len(tm.text())
		tm.send(cmd + "\r")
		tm.expectAfter(mark, `activity exit=0`, 5*time.Second)
	}
	// An idle process a tool left behind neither keeps the session nor outlives
	// it; a daemon that detached itself may serve others and is left running.
	tm.send("leave 4242\r")
	tm.expect(`leave started`, 5*time.Second)
	tm.send("daemon 4343\r")
	tm.expect(`daemon started`, 5*time.Second)
	defer onBox("pkill -x -f 'sleep 4343'")
	dropLink(t, tm)
	defer block(t, false)
	waitGone(t, "fakeclaude-"+id, 20*time.Second)
	log, err := onBox("cat " + box.ReaperLog())
	if err != nil || !strings.Contains(log, "ending agent session "+id+" (reaped-idle)") || !regexp.MustCompile(`left-behind processes: \d+ sleep`).MatchString(log) {
		t.Fatalf("reaper log: %v\n%s", err, log)
	}
	if left, _ := onBox("pgrep -a -x sleep | grep -c ' 4242$'"); strings.TrimSpace(left) != "0" {
		t.Fatalf("left-behind sleep survived the reap: %q", left)
	}
	if left, _ := onBox("pgrep -a -x sleep | grep -c ' 4343$'"); strings.TrimSpace(left) != "1" || !regexp.MustCompile(`left running: \d+ sleep \(daemon\)`).MatchString(log) {
		t.Fatalf("the detached daemon was not left running (%q):\n%s", left, log)
	}
	if ls := boxListing(t); !strings.Contains(ls, "rfakeclaude --resume "+id) {
		t.Fatalf("reaped session not listed:\n%s", ls)
	}
	mark := len(tm.text())
	block(t, false)
	tm.expectAfter(mark, `resumed; the session had been ended for inactivity`, 20*time.Second)
	args := tm.expectAfter(mark, `fake-claude args=(.*)\r?\n`, 10*time.Second)[1]
	if !strings.HasSuffix(strings.TrimSpace(args), "--dangerously-skip-permissions --resume "+id) {
		t.Fatalf("resumed with %q", args)
	}
	if got := tm.expectAfter(mark, `fake-claude session=`+id+` pid=(\d+)`, 10*time.Second)[1]; got == pid {
		t.Fatal("the reaped agent is still the one running")
	}
	tm.send("exit 0\r")
	if code := tm.wait(10 * time.Second); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestToolProcessesKeepADetachedSessionRunning(t *testing.T) {
	for tool, reason := range map[string]string{"bg": "working: tool", "busy": "working: background process"} {
		t.Run(tool, func(t *testing.T) {
			shortReaper(t)
			tm := start(t, "rfakeclaude")
			m := tm.expect(`fake-claude session=(`+uuidRe+`) pid=(\d+)`, 20*time.Second)
			id, pid := m[1], m[2]
			tm.send(tool + " 20\r")
			tm.expect(tool+` started`, 5*time.Second)
			dropLink(t, tm)
			defer block(t, false)
			time.Sleep(9 * time.Second)
			if ls := boxSessions(t); !strings.Contains(ls, "fakeclaude-"+id) || !strings.Contains(ls, reason) {
				t.Fatalf("working session not kept:\n%s", ls)
			}
			mark := len(tm.text())
			block(t, false)
			tm.drawnAfter(mark)
			tm.send("whoami\r")
			if got := tm.expectAfter(mark, `fake-claude pid=(\d+)`, 15*time.Second)[1]; got != pid {
				t.Fatalf("reattached to pid %s, want %s", got, pid)
			}
			tm.send("exit 0\r")
			tm.wait(10 * time.Second)
		})
	}
}

// reapDetached starts a session, detaches it and waits for the reaper to end it.
func reapDetached(t *testing.T) string {
	t.Helper()
	tm := start(t, "rfakeclaude")
	id := tm.expect(`fake-claude session=(`+uuidRe+`)`, 20*time.Second)[1]
	dropLink(t, tm)
	waitGone(t, "fakeclaude-"+id, 20*time.Second)
	tm.send("\x03")
	if code := tm.wait(5 * time.Second); code != 130 {
		t.Fatalf("detach exit %d, want 130", code)
	}
	block(t, false)
	return id
}

// A reaped session comes back whether it is attached by name or resumed by
// id, as a cmux restore does, and says it had been reaped.
func TestReapedSessionResumesByNameAndByID(t *testing.T) {
	shortReaper(t)
	defer block(t, false)
	for name, args := range map[string]func(id string) []string{
		"attach": func(id string) []string { return []string{"remote-agents", "attach", "fakeclaude-" + id} },
		"resume": func(id string) []string { return []string{"rfakeclaude", "--resume", id} },
	} {
		t.Run(name, func(t *testing.T) {
			id := reapDetached(t)
			argv := args(id)
			again := start(t, argv[0], argv[1:]...)
			again.expect(`resumed; the session had been ended for inactivity`, 20*time.Second)
			again.expect(`fake-claude session=`+id, 10*time.Second)
			again.send("exit 0\r")
			if code := again.wait(10 * time.Second); code != 0 {
				t.Fatalf("exit %d", code)
			}
		})
	}
}

// Account commands run against a Claude config dir of their own and a
// stand-in for Anthropic's token and usage endpoints on the box.
// startFakeOAuth starts testdata/fake-oauth on the box and returns its URL.
func startFakeOAuth(t *testing.T) string {
	t.Helper()
	dir := boxHome + "/" + boxTestDir
	portFile, pidFile := dir+"/fake-oauth.port", dir+"/fake-oauth.pid"
	if _, err := onBox(fmt.Sprintf(`set -e
rm -f %[2]q
nohup %[1]q/fake-oauth %[2]q >/dev/null 2>&1 &
echo $! > %[3]q
for i in $(seq 50); do [ -s %[2]q ] && break; sleep 0.1; done
[ -s %[2]q ]`, dir, portFile, pidFile)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if out, err := onBox(fmt.Sprintf(`kill "$(cat %[1]q)"; rm -f %[1]q %[2]q`, pidFile, portFile)); err != nil {
			t.Logf("cleanup: %v %s", err, out)
		}
	})
	port, err := onBox("cat " + portFile)
	if err != nil {
		t.Fatal(err)
	}
	return "http://127.0.0.1:" + strings.TrimSpace(port)
}

// writeAccountsConfig gives the test install's account commands their own
// config dir and the fake's endpoints in section; without it they refuse to run.
func writeAccountsConfig(t *testing.T, section, dir, tokenURL, apiURL string) {
	t.Helper()
	if _, err := onBox(fmt.Sprintf(`printf '[%%s]\nconfig_dir = "%%s"\ntoken_url = "%%s"\napi_url = "%%s"\n' %q %q %q %q > %q`, section, dir, tokenURL, apiURL, box.AccountsConfig())); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if out, err := onBox("rm -f " + box.AccountsConfig()); err != nil {
			t.Logf("cleanup: %v %s", err, out)
		}
	})
}

// waitOK waits for tm to exit 0.
func waitOK(t *testing.T, tm *term) {
	t.Helper()
	if code := tm.wait(30 * time.Second); code != 0 {
		t.Fatalf("exit %d:\n%s", code, tm.text())
	}
}

func TestAccountsAddListAndSwitchTheBoxLogin(t *testing.T) {
	cfg := boxHome + "/" + boxTestDir + "/accounts-claude"
	vault := box.AccountsDir("claude")
	if _, err := onBox(fmt.Sprintf(`set -e
rm -rf %[1]q %[2]q && find %[3]q -maxdepth 1 -name 'started-*' -delete && mkdir -p %[1]q
expires=$(( $(date +%%s) * 1000 + 3600000 ))
printf '{"claudeAiOauth":{"accessToken":"fake-access-work-1","refreshToken":"fake-refresh-work-1","expiresAt":%%s}}' "$expires" > %[1]q/.credentials.json
printf '{"numStartups":1,"oauthAccount":{"accountUuid":"acc-work","organizationUuid":"org-work","emailAddress":"work@example.com"}}' > %[1]q/.claude.json
echo home > %[3]q/next-login`, cfg, vault, boxHome+"/"+boxTestDir)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if out, err := onBox(fmt.Sprintf(`rm -rf %q %q && find %q -maxdepth 1 -name 'started-*' -delete`, cfg, vault, boxHome+"/"+boxTestDir)); err != nil {
			t.Logf("cleanup: %v %s", err, out)
		}
	})
	api := startFakeOAuth(t)
	writeAccountsConfig(t, "claude", cfg, api+"/token", api)
	run := func(args ...string) *term {
		t.Helper()
		return start(t, "rfakeclaude", append([]string{"account"}, args...)...)
	}

	save := run("save", "work")
	save.expect(`saved the box's login as work`, 20*time.Second)
	waitOK(t, save)

	before := len(openedURLs(t))
	add := run("add", "home")
	add.expect(`saved the new login as home`, 30*time.Second)
	waitOK(t, add)
	if !strings.Contains(openedURLs(t)[before:], "https://claude.com/cai/oauth/authorize?account=home") {
		t.Fatalf("sign-in url not opened on the laptop:\n%s", openedURLs(t)[before:])
	}

	list := run("list")
	list.expect(`(?s)home\s+home@example.com\s+▰▰▰▰▰ 100% left · not started\n.*work\s+work@example.com\s+▰▰▰▰▰ 100% left · not started\s+active`, 20*time.Second)
	waitOK(t, list)

	// The picker starts home's week as home, then switches the box to it.
	pick := run()
	pick.expect(`(?s)▸ work .*s start week · q quit`, 20*time.Second)
	pick.send("k")
	mark := len(pick.text())
	pick.expectAfter(mark, `▸ home `, 10*time.Second)
	pick.send("s")
	pick.expectAfter(mark, `started home's week; it resets \w{3} \d\d:\d\d`, 60*time.Second)
	pick.send("\r")
	pick.expect(`switched the box to home; running sessions use it from their next request`, 20*time.Second)
	waitOK(t, pick)

	out, err := onBox(fmt.Sprintf(`set -e
grep -o 'fake-refresh-[a-z]*-[0-9]*' %[1]q/.credentials.json
grep -o '"emailAddress": "[^"]*"' %[1]q/.claude.json
stat -c '%%a %%n' %[2]q %[2]q/work.json %[2]q/home.json | sed 's|%[2]s|VAULT|'
find ~/.config/systemd/user -name '%[3]s-accounts-refresh*' | wc -l
ls -A %[2]q | LC_ALL=C sort
cat %[4]q/started-home`, cfg, vault, boxInstall, boxHome+"/"+boxTestDir))
	if err != nil {
		t.Fatal(err)
	}
	want := "fake-refresh-home-1\n\"emailAddress\": \"home@example.com\"\n700 VAULT\n600 VAULT/work.json\n600 VAULT/home.json\n0\n" +
		".live.json\n.lock\n.start\nhome.json\nwork.json\n" +
		"-p hi --model haiku --no-session-persistence --strict-mcp-config --setting-sources project\n"
	if out != want {
		t.Fatalf("box state:\n%s\nwant\n%s", out, want)
	}

	js := run("list", "--json")
	js.expect(`"name":"home","email":"home@example.com","active":true`, 20*time.Second)
	waitOK(t, js)
}

func TestCodexAccountsAddListAndSwitchTheBoxLogin(t *testing.T) {
	cfg := boxHome + "/" + boxTestDir + "/accounts-codex"
	vault := box.AccountsDir("codex")
	live := base64.StdEncoding.EncodeToString([]byte(codexfake.Login("work", 1, time.Now().Add(48*time.Hour))))
	if _, err := onBox(fmt.Sprintf(`set -e
rm -rf %[1]q %[2]q && mkdir -p %[1]q
echo %[4]s | base64 -d > %[1]q/auth.json
echo home > %[3]q/next-login`, cfg, vault, boxHome+"/"+boxTestDir, live)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if out, err := onBox(fmt.Sprintf(`rm -rf %q %q`, cfg, vault)); err != nil {
			t.Logf("cleanup: %v %s", err, out)
		}
	})
	api := startFakeOAuth(t)
	writeAccountsConfig(t, "codex", cfg, api+"/oauth/token", api+"/backend-api")
	run := func(args ...string) *term {
		t.Helper()
		return start(t, "rfakecodex", append([]string{"account"}, args...)...)
	}

	save := run("save", "work")
	save.expect(`saved the box's login as work`, 20*time.Second)
	waitOK(t, save)

	before := len(openedURLs(t))
	add := run("add", "home")
	add.expect(`saved the new login as home; switch to it with: rcodex account use home`, 30*time.Second)
	waitOK(t, add)
	if !strings.Contains(openedURLs(t)[before:], "https://auth.openai.com/oauth/authorize?account=home") {
		t.Fatalf("sign-in url not opened on the laptop:\n%s", openedURLs(t)[before:])
	}

	list := run("list")
	list.expect(`(?s)home\s+home@example.com\s+▰▰▰▱▱ 60% left · resets \w{3} \d\d:\d\d\n.*work\s+work@example.com\s+▰▰▰▱▱ 60% left · resets \w{3} \d\d:\d\d\s+active`, 20*time.Second)
	waitOK(t, list)

	pick := run()
	pick.expect(`(?s)▸ work .*d remove · q quit`, 20*time.Second)
	pick.send("\x1b[A")
	mark := len(pick.text())
	pick.expectAfter(mark, `▸ home `, 10*time.Second)
	pick.send("\r")
	pick.expect(`switched the box's Codex to home; Codex sessions started from now on use it`, 20*time.Second)
	waitOK(t, pick)

	out, err := onBox(fmt.Sprintf(`set -e
grep -o 'fake-refresh-[a-z]*-[0-9]*' %[1]q/auth.json
stat -c '%%a %%n' %[1]q/auth.json %[2]q %[2]q/work.json %[2]q/home.json | sed 's|%[1]s|CODEX|; s|%[2]s|VAULT|'
find ~/.config/systemd/user -name '%[3]s-accounts-refresh*' | wc -l`, cfg, vault, boxInstall))
	if err != nil {
		t.Fatal(err)
	}
	want := "fake-refresh-home-1\n600 CODEX/auth.json\n700 VAULT\n600 VAULT/work.json\n600 VAULT/home.json\n0\n"
	if out != want {
		t.Fatalf("box state:\n%s\nwant\n%s", out, want)
	}
}
