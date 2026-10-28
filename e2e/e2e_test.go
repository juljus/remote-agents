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
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"remote-agents/internal/boxlayout"
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
	for _, d := range []string{binDir, sshDir, laptopHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return 0, err
		}
	}
	build := exec.Command("go", "build", "-o", filepath.Join(binDir, "remote-agents"), "./cmd/remote-agents")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("build: %v: %s", err, out)
	}
	for _, agent := range []string{"fakeclaude", "realclaude", "isolatedclaude"} {
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
	if _, err := sh("scp", "-q", "-p", filepath.Join(root, "testdata", "fake-claude"), host+":"+boxTestDir+"/fake-claude"); err != nil {
		return 0, err
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
	fakeCmux := filepath.Join(work, "cmux")
	if err := writeExec(fakeCmux, fmt.Sprintf(`echo "$* surface=$CMUX_SURFACE_ID pid=$CMUX_CLAUDE_PID custom=$CMUX_CUSTOM_CLAUDE_PATH stdin=$(cat)" >> %q; printf '{"ok":true}'`, hookLog)); err != nil {
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

[cmux]
wrapper = %q

[agents.fakeclaude]
command = "%s/%s/fake-claude"
kind = "claude"
args = ["--dangerously-skip-permissions"]

[agents.realclaude]
command = "claude"
kind = "claude"

[agents.isolatedclaude]
command = "%s/%s/claude-isolated"
kind = "claude"
`, host, boxInstall, boxHome, boxTestDir, fakeBrowser, fakeWrapper, boxHome, boxTestDir, boxHome, boxTestDir)
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		return 0, err
	}

	logFile, err := os.Create(daemonLog)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	daemon := exec.Command(filepath.Join(binDir, "remote-agents"), "link")
	daemon.Env = testEnv()
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

// term is one laptop terminal running a wrapper command.
type term struct {
	t      *testing.T
	cmd    *exec.Cmd
	master *os.File
	mu     sync.Mutex
	out    bytes.Buffer
	done   chan struct{}
	code   int
}

func start(t *testing.T, name string, args ...string) *term {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, name), args...)
	cmd.Env = testEnv()
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	tm := &term{t: t, cmd: cmd, master: master, done: make(chan struct{})}
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			tm.mu.Lock()
			tm.out.Write(buf[:n])
			tm.mu.Unlock()
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

func (tm *term) text() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return ansi.ReplaceAllString(tm.out.String(), "")
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
	time.Sleep(3 * time.Second)
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
	time.Sleep(2 * time.Second)
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
	time.Sleep(2 * time.Second)
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
// dropped file, reaches Claude on the box as the path of an uploaded copy.
func TestRealClaudeAttachesAPastedLaptopImagePath(t *testing.T) {
	shot := filepath.Join(t.TempDir(), "pasted shot.png")
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
	out, err := onBox(fmt.Sprintf("sha256sum %s/.local/state/%s/pasted/%s.png", boxHome, boxInstall, sum))
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
	time.Sleep(time.Second)
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
	if err != nil || !strings.Contains(log, "ending Claude session "+id+" (reaped-idle)") || !regexp.MustCompile(`left-behind processes: \d+ sleep`).MatchString(log) {
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
			time.Sleep(2 * time.Second)
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
