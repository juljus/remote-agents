package session

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"remote-agents/internal/pastefile"
)

// fakeBox is an ssh that runs attach for the session and pasteStore for an upload.
func fakeBox(t *testing.T, attach, pasteStore string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\nfor a; do last=\"$a\"; done\ncase \"$last\" in\n*paste-store*) " + pasteStore + " ;;\n*) " + attach + " ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

var onePixelPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
}

// echoLine is a session that reports the first line of input it reads.
const echoLine = `echo ready; IFS= read -r line; printf 'got[%s]\n' "$line"`

func TestPastedLaptopImageGoesToTheBoxUnderItsBoxPath(t *testing.T) {
	h := newPTY(t)
	dir := t.TempDir()
	shot := filepath.Join(dir, "my shot.png")
	if err := os.WriteFile(shot, onePixelPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	uploaded := filepath.Join(dir, "uploaded")
	ssh := fakeBox(t, echoLine+`; exit 0`, `cat > '`+uploaded+`'; echo /home/agent/.local/state/ra-test/pasted/abc.png`)
	done := start(testOptions(ssh, h))
	h.waitFor(t, "ready")
	paste := "\x1b[200~" + strings.ReplaceAll(shot, " ", `\ `) + "\x1b[201~"
	if _, err := h.master.Write([]byte(paste + "\n")); err != nil {
		t.Fatal(err)
	}
	if r := wait(t, done); r.err != nil || r.code != 0 {
		t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
	}
	if want := "got[\x1b[200~/home/agent/.local/state/ra-test/pasted/abc.png\x1b[201~]"; !strings.Contains(h.output(), want) {
		t.Fatalf("output %q lacks %q", h.output(), want)
	}
	f, err := os.Open(uploaded)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got, err := pastefile.NewReader(f).Next(); err != nil || string(got) != string(onePixelPNG) {
		t.Fatalf("uploaded %q, %v", got, err)
	}
}

func TestAFailedUploadShowsWhyAndPastesThePathUnchanged(t *testing.T) {
	h := newPTY(t)
	shot := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(shot, onePixelPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	ssh := fakeBox(t, echoLine+`; sleep 1; exit 0`, `cat >/dev/null; echo "remote-agents-box: disk full" >&2; exit 1`)
	opts := testOptions(ssh, h)
	var logged []string
	opts.Logf = func(format string, args ...any) { logged = append(logged, format) }
	done := start(opts)
	h.waitFor(t, "ready")
	paste := "\x1b[200~" + shot + "\x1b[201~"
	if _, err := h.master.Write([]byte(paste + "\n")); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "got["+paste+"]")
	h.waitFor(t, "disk full")
	if r := wait(t, done); r.err != nil {
		t.Fatal(r.err)
	}
	if len(logged) == 0 {
		t.Fatal("the failure was not logged")
	}
}

func TestTheSessionSeesTheTerminalSizeAndItsChanges(t *testing.T) {
	h := newPTY(t)
	ssh := fakeBox(t, `trap 'echo "size $(stty size)"' WINCH; echo "size $(stty size)"; IFS= read -r line; exit 0`, `exit 9`)
	done := start(testOptions(ssh, h))
	h.waitFor(t, "size 24 80")
	if err := pty.Setsize(h.master, &pty.Winsize{Rows: 30, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	// The test's terminal is not this process's own, so it sends itself the resize signal.
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "size 30 100")
	if _, err := h.master.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if r := wait(t, done); r.err != nil || r.code != 0 {
		t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
	}
}

func TestKeystrokesAfterAPasteWaitForItsUpload(t *testing.T) {
	h := newPTY(t)
	shot := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(shot, onePixelPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	ssh := fakeBox(t, `echo ready; IFS= read -r a; IFS= read -r b; printf 'got[%s][%s]\n' "$a" "$b"; exit 0`,
		`cat >/dev/null; sleep 0.5; echo /box/p.png`)
	done := start(testOptions(ssh, h))
	h.waitFor(t, "ready")
	if _, err := h.master.Write([]byte("\x1b[200~" + shot + "\x1b[201~")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := h.master.Write([]byte("\ntyped after\n")); err != nil {
		t.Fatal(err)
	}
	if r := wait(t, done); r.err != nil || r.code != 0 {
		t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
	}
	if want := "got[\x1b[200~/box/p.png\x1b[201~][typed after]"; !strings.Contains(h.output(), want) {
		t.Fatalf("output %q lacks %q", h.output(), want)
	}
}
