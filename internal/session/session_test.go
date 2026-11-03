package session

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
	"golang.org/x/term"

	"remote-agents/internal/corner"
	"remote-agents/internal/spec"
	"remote-agents/internal/sshrun"
)

type ptyHarness struct {
	master *os.File
	slave  *os.File
	mu     sync.Mutex
	out    bytes.Buffer
}

func newPTY(t *testing.T) *ptyHarness {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	h := &ptyHarness{master: master, slave: slave}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			h.mu.Lock()
			h.out.Write(buf[:n])
			h.mu.Unlock()
			// Like a terminal, it says where the cursor is when asked; the session starts at the top.
			if bytes.Contains(buf[:n], []byte("\x1b[6n")) {
				master.Write([]byte("\x1b[1;1R"))
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { slave.Close(); master.Close() })
	return h
}

func (h *ptyHarness) output() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.out.String()
}

// marking is what the session adds to the output for the remote mark: the
// cursor query, the mark and the moves to the corner and back.
var marking = regexp.MustCompile(`\x1b\[6n|` + corner.Mark + `|\x1b\[\d+;\d+H|\x1b\[2X|\x1b\[0(;[\d;]+)?m`)

// text is the output without the remote mark.
func (h *ptyHarness) text() string { return marking.ReplaceAllString(h.output(), "") }

func (h *ptyHarness) waitFor(t *testing.T, s string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(h.text(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("output never contained %q:\n%q", s, h.output())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fakeSSH runs the n-th body (1-based) on the n-th invocation, repeating the last.
func fakeSSH(t *testing.T, bodies ...string) string {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	var script strings.Builder
	script.WriteString("#!/bin/sh\nn=$(($(cat " + counter + " 2>/dev/null || echo 0) + 1))\necho $n > " + counter + "\ncase $n in\n")
	for i, b := range bodies {
		pattern := string(rune('1' + i))
		if i == len(bodies)-1 {
			pattern = "*"
		}
		script.WriteString(pattern + ") " + b + " ;;\n")
	}
	script.WriteString("esac\n")
	path := filepath.Join(dir, "ssh")
	if err := os.WriteFile(path, []byte(script.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testOptions(ssh string, h *ptyHarness) Options {
	return Options{
		SSH: ssh,
		Box: sshrun.Box{Host: "box", Install: "ra-test"},
		Spec: spec.Spec{
			Mode: spec.ModeSession, Session: "claude-abc", RunID: "11111111-2222-4333-8444-555555555555",
			Workdir: "/w", Argv: []string{"claude"},
		},
		ReattachHint: "claude --resume abc",
		Triggers:     make(chan string),
		Stdin:        h.slave, Stdout: h.slave, Stderr: h.slave,
		Logf: func(string, ...any) {},
	}
}

type result struct {
	code int
	err  error
}

func start(opts Options) chan result {
	done := make(chan result, 1)
	go func() {
		code, err := Run(context.Background(), opts)
		done <- result{code, err}
	}()
	return done
}

func wait(t *testing.T, done chan result) result {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("session did not finish")
	}
	return result{}
}

func TestReconnectsAfterDropAndReturnsAgentExit(t *testing.T) {
	h := newPTY(t)
	ssh := fakeSSH(t,
		`echo first; echo "client_loop: send disconnect: Broken pipe" >&2; exit 255`,
		`echo second; exit 5`)
	r := wait(t, start(testOptions(ssh, h)))
	if r.err != nil || r.code != 5 {
		t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
	}
	out := h.output()
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") || !strings.Contains(out, "connection lost") {
		t.Fatalf("output %q", out)
	}
	if strings.Contains(out, "detached") {
		t.Fatalf("unexpected detach message: %q", out)
	}
	if !strings.HasSuffix(out, resetModes) {
		t.Fatalf("terminal modes not reset after an exit that followed a drop: %q", out)
	}
}

func TestOutageStatusNeverWrapsOnANarrowTerminal(t *testing.T) {
	h := newPTY(t)
	if err := pty.Setsize(h.master, &pty.Winsize{Rows: 24, Cols: 40}); err != nil {
		t.Fatal(err)
	}
	ssh := fakeSSH(t,
		`echo first; echo "ssh: connect to host 198.51.100.76 port 22: Operation timed out after a long wait" >&2; exit 255`,
		`sleep 1; echo second; exit 5`)
	if r := wait(t, start(testOptions(ssh, h))); r.err != nil || r.code != 5 {
		t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
	}
	out := h.text()
	if n := strings.Count(out, "reconnecting"); n < 10 {
		t.Fatalf("status drawn %d times, want an animation: %q", n, out)
	}
	scr := &recordingScreen{screen: newScreen(40)}
	for i, chunk := range strings.SplitAfter(out, "\x1b[2K") {
		scr.Write([]byte(chunk))
		if rows := scr.used(); len(rows) > 2 {
			t.Fatalf("redraw %d left rows %q", i, rows)
		}
	}
	if rows := scr.used(); len(rows) != 2 || rows[0] != "first" || rows[1] != "second" {
		t.Fatalf("final rows %q", rows)
	}
}

func TestCtrlCWhileDisconnectedDetaches(t *testing.T) {
	for name, key := range map[string]string{"plain": "\x03", "kitty": "\x1b[99;5u"} {
		t.Run(name, func(t *testing.T) {
			h := newPTY(t)
			ssh := fakeSSH(t, `echo "ssh: connect to host box port 22: Operation timed out" >&2; exit 255`)
			done := start(testOptions(ssh, h))
			h.waitFor(t, "reconnecting")
			if _, err := h.master.Write([]byte(key)); err != nil {
				t.Fatal(err)
			}
			r := wait(t, done)
			if r.err != nil || r.code != 130 {
				t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
			}
			h.waitFor(t, "reattach with: claude --resume abc")
			// Detaching leaves the detach lines in place of the status, nothing behind them.
			scr := newScreen(80)
			scr.Write([]byte(h.text()))
			rows := scr.used()
			if len(rows) != 2 || !strings.HasPrefix(rows[0], "[remote-agents] detached;") {
				t.Fatalf("rows after detach %q", rows)
			}
		})
	}
}

func TestTriggerReconnectsImmediately(t *testing.T) {
	h := newPTY(t)
	ssh := fakeSSH(t, `echo up; exec sleep 30`, `echo again; exit 0`)
	opts := testOptions(ssh, h)
	triggers := make(chan string, 1)
	opts.Triggers = triggers
	done := start(opts)
	h.waitFor(t, "up")
	triggers <- "wake (clock jumped 10m0s)"
	r := wait(t, done)
	if r.err != nil || r.code != 0 || !strings.Contains(h.output(), "again") {
		t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
	}
}

func TestRequiresTerminal(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	opts := Options{Stdin: devnull}
	if _, err := Run(context.Background(), opts); err == nil {
		t.Fatal("expected error without a terminal")
	}
}

func TestRejectsZeroSizedTerminal(t *testing.T) {
	h := newPTY(t)
	if err := pty.Setsize(h.master, &pty.Winsize{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), testOptions(fakeSSH(t, "exit 0"), h)); err == nil || !strings.Contains(err.Error(), "0x0") {
		t.Fatalf("err = %v, want size error", err)
	}
}

func TestExitPrintsTheResumeHint(t *testing.T) {
	h := newPTY(t)
	opts := testOptions(fakeSSH(t, `echo bye; exit 3`), h)
	opts.ExitHint = func() string { return "Resume on the box with: rclaude --resume abc" }
	r := wait(t, start(opts))
	if r.err != nil || r.code != 3 {
		t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
	}
	h.waitFor(t, "bye")
	h.waitFor(t, "Resume on the box with: rclaude --resume abc")
}

func TestNoResumeHintUnlessTheAgentExited(t *testing.T) {
	for name, body := range map[string]string{
		"unknown id": `exit 0`,
		"taken over": `exit 75`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newPTY(t)
			opts := testOptions(fakeSSH(t, `echo started; `+body), h)
			hint := "Resume on the box with: rclaude --resume abc"
			if name == "unknown id" {
				hint = ""
			}
			opts.ExitHint = func() string { return hint }
			if r := wait(t, start(opts)); r.err != nil {
				t.Fatal(r.err)
			}
			h.waitFor(t, "started")
			if strings.Contains(h.output(), "Resume on the box") {
				t.Fatalf("hint printed: %q", h.output())
			}
		})
	}
	h := newPTY(t)
	opts := testOptions(fakeSSH(t, `echo "ssh: connect to host box port 22: Operation timed out" >&2; exit 255`), h)
	opts.ExitHint = func() string { return "Resume on the box with: rclaude --resume abc" }
	done := start(opts)
	h.waitFor(t, "reconnecting")
	if _, err := h.master.Write([]byte("\x03")); err != nil {
		t.Fatal(err)
	}
	wait(t, done)
	h.waitFor(t, "reattach with")
	if strings.Contains(h.output(), "Resume on the box") {
		t.Fatalf("hint printed on detach: %q", h.output())
	}
}

func TestEachAttemptCarriesTheNextAttemptNumber(t *testing.T) {
	h := newPTY(t)
	log := filepath.Join(t.TempDir(), "tokens")
	record := `for a; do last="$a"; done; echo "${last##* }" >> ` + log + `; `
	ssh := fakeSSH(t,
		record+`echo "client_loop: send disconnect: Broken pipe" >&2; exit 255`,
		// A stale attempt's refusal is a lost link, not the agent's exit.
		record+`exit `+strconv.Itoa(spec.ExitStaleAttempt),
		// So is a session the box lost.
		record+`exit `+strconv.Itoa(spec.ExitSessionLost),
		record+`echo done; exit 0`)
	if r := wait(t, start(testOptions(ssh, h))); r.err != nil || r.code != 0 {
		t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	tokens := strings.Fields(string(data))
	if len(tokens) != 4 {
		t.Fatalf("tokens %q", tokens)
	}
	for i, token := range tokens {
		s, err := spec.Decode(token)
		if err != nil {
			t.Fatal(err)
		}
		if s.Attempt != i+1 || s.RunID != testOptions(ssh, h).Spec.RunID {
			t.Fatalf("attempt %d sent run %s attempt %d", i+1, s.RunID, s.Attempt)
		}
	}
}

// screenOf is what a terminal shows after the harness's output.
func screenOf(t *testing.T, h *ptyHarness) *vt.Emulator {
	t.Helper()
	e := vt.NewEmulator(80, 24)
	// The emulator would answer the cursor query to no one and block.
	e.WriteString(strings.ReplaceAll(h.output(), "\x1b[6n", ""))
	return e
}

func TestTheMarkShowsWhileTheSessionRunsAndGoesWithIt(t *testing.T) {
	h := newPTY(t)
	ssh := fakeSSH(t, `for i in $(seq 40); do echo line $i; done; sleep 1; exit 0`)
	done := start(testOptions(ssh, h))
	h.waitFor(t, "line 40")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.HasPrefix(screenOf(t, h).CellAt(78, 23).Content, "☁") {
		if time.Now().After(deadline) {
			t.Fatalf("no mark in the corner:\n%s", screenOf(t, h).String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r := wait(t, done); r.err != nil || r.code != 0 {
		t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
	}
	e := screenOf(t, h)
	if strings.Contains(e.String(), corner.Mark) {
		t.Fatalf("a mark left behind:\n%s", e.String())
	}
	for y := range e.ScrollbackLen() {
		for x := range 80 {
			if c := e.ScrollbackCellAt(x, y); c != nil && strings.Contains(c.Content, "☁") {
				t.Fatalf("a mark in scrollback line %d", y)
			}
		}
	}
	if e.ScrollbackLen() == 0 {
		t.Fatal("the session never scrolled")
	}
}

func TestCursorPositionSkipsTypeahead(t *testing.T) {
	h := newPTY(t)
	if _, err := term.MakeRaw(int(h.slave.Fd())); err != nil {
		t.Fatal(err)
	}
	if _, err := h.master.Write([]byte("typed")); err != nil {
		t.Fatal(err)
	}
	row, col, err := cursorPosition(int(h.slave.Fd()), h.slave, time.Second)
	if err != nil || row != 1 || col != 1 {
		t.Fatalf("cursorPosition = %d, %d, %v", row, col, err)
	}
}
