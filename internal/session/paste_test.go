package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"remote-agents/internal/pastefile"
)

// fakeBox is an ssh that runs attach for the session and pasteFiles for an upload.
func fakeBox(t *testing.T, attach, pasteFiles string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\nfor a; do last=\"$a\"; done\ncase \"$last\" in\n*paste-files*) " + pasteFiles + " ;;\n*) " + attach + " ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// boxReplies is a box's side of paste-files for the fake ssh: it answers
// onBox, keeps what the laptop sends in received, and once the laptop's side
// ends sends stored as the paths of the uploads.
func boxReplies(received string, onBox string, stored ...string) string {
	var frames strings.Builder
	for _, p := range stored {
		fmt.Fprintf(&frames, "%d\n%s", len(p), p)
	}
	return fmt.Sprintf(`printf '%d\n%s'; cat > '%s'; printf '%%s' '%s'`, len(onBox), onBox, received, frames.String())
}

// received reads what the laptop sent the fake box: its query and its uploads as name, content pairs.
func received(t *testing.T, path string) ([]string, [][2]string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := pastefile.NewReader(f)
	query, err := r.Query()
	if err != nil {
		t.Fatal(err)
	}
	var uploads [][2]string
	for {
		name, err := r.Next()
		if errors.Is(err, io.EOF) {
			return query, uploads
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		uploads = append(uploads, [2]string{string(name), string(data)})
	}
}

var onePixelPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
}

// echoLine is a session that reports the first line of input it reads.
const echoLine = `echo ready; IFS= read -r line; printf 'got[%s]\n' "$line"`

func TestPastedLaptopFilesTheBoxLacksGoToItUnderTheirBoxPaths(t *testing.T) {
	h := newPTY(t)
	dir := t.TempDir()
	shot := filepath.Join(dir, "my shot.png")
	if err := os.WriteFile(shot, onePixelPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	onBox := filepath.Join(dir, "on box.txt")
	if err := os.WriteFile(onBox, []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	receivedAt := filepath.Join(dir, "received")
	ssh := fakeBox(t, echoLine+`; exit 0`, boxReplies(receivedAt, "01", "/home/agent/.local/state/ra-test/pasted/abc/my shot.png"))
	done := start(testOptions(ssh, h))
	h.waitFor(t, "ready")
	typed := strings.ReplaceAll(shot, " ", `\ `) + " /nonexistent/box.pdf " + strings.ReplaceAll(onBox, " ", `\ `)
	if _, err := h.master.Write([]byte("\x1b[200~" + typed + "\x1b[201~\n")); err != nil {
		t.Fatal(err)
	}
	if r := wait(t, done); r.err != nil || r.code != 0 {
		t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
	}
	want := "got[\x1b[200~/home/agent/.local/state/ra-test/pasted/abc/my\\ shot.png /nonexistent/box.pdf " + strings.ReplaceAll(onBox, " ", `\ `) + "\x1b[201~]"
	if !strings.Contains(h.output(), want) {
		t.Fatalf("output %q lacks %q", h.output(), want)
	}
	query, uploads := received(t, receivedAt)
	if !slices.Equal(query, []string{shot, onBox}) {
		t.Fatalf("query %q", query)
	}
	if len(uploads) != 1 || uploads[0] != [2]string{"my shot.png", string(onePixelPNG)} {
		t.Fatalf("uploads %q", uploads)
	}
}

func TestPastedPathsThatCannotUploadStayAsTypedAndSayWhy(t *testing.T) {
	h := newPTY(t)
	dir := t.TempDir()
	folder := filepath.Join(dir, "folder")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.zip")
	if err := os.WriteFile(big, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, pastefile.MaxSize+1); err != nil {
		t.Fatal(err)
	}
	receivedAt := filepath.Join(dir, "received")
	ssh := fakeBox(t, echoLine+`; sleep 1; exit 0`, boxReplies(receivedAt, "00"))
	done := start(testOptions(ssh, h))
	h.waitFor(t, "ready")
	paste := "\x1b[200~" + folder + " " + big + "\x1b[201~"
	if _, err := h.master.Write([]byte(paste + "\n")); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "got["+paste+"]")
	h.waitFor(t, "not uploaded: folder is a directory; big.zip is over 100 MB")
	if r := wait(t, done); r.err != nil {
		t.Fatal(r.err)
	}
	if _, uploads := received(t, receivedAt); len(uploads) != 0 {
		t.Fatalf("uploaded %q", uploads)
	}
}

func TestAFailedUploadShowsWhyAndPastesThePathUnchanged(t *testing.T) {
	h := newPTY(t)
	shot := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(shot, onePixelPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	ssh := fakeBox(t, echoLine+`; sleep 1; exit 0`, `echo "remote-agents-box: disk full" >&2; exit 1`)
	opts := testOptions(ssh, h)
	var logged []string
	opts.Logf = func(format string, args ...any) { logged = append(logged, format) }
	done := start(opts)
	h.waitFor(t, "ready")
	paste := "\x1b[200~" + shot + "\x1b[201~"
	sent := time.Now()
	if _, err := h.master.Write([]byte(paste + "\n")); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "got["+paste+"]")
	// The box ending the exchange must not wait out ssh's I/O grace.
	if elapsed := time.Since(sent); elapsed > time.Second {
		t.Fatalf("the failed upload held the paste for %s", elapsed)
	}
	h.waitFor(t, "disk full")
	if r := wait(t, done); r.err != nil {
		t.Fatal(r.err)
	}
	if len(logged) == 0 {
		t.Fatal("the failure was not logged")
	}
}

func TestAPasteSaysWhyEachFileStayedAndUploadsTheRest(t *testing.T) {
	h := newPTY(t)
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(locked, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	shot := filepath.Join(dir, "a.png")
	if err := os.WriteFile(shot, onePixelPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	receivedAt := filepath.Join(dir, "received")
	ssh := fakeBox(t, echoLine+`; sleep 1; exit 0`, boxReplies(receivedAt, "00", "/box/pasted/abc/a.png"))
	done := start(testOptions(ssh, h))
	h.waitFor(t, "ready")
	if _, err := h.master.Write([]byte("\x1b[200~" + locked + " " + shot + "\x1b[201~\n")); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "got[\x1b[200~"+locked+" /box/pasted/abc/a.png\x1b[201~]")
	h.waitFor(t, "not uploaded: locked.txt cannot be read")
	if r := wait(t, done); r.err != nil {
		t.Fatal(r.err)
	}
	if _, uploads := received(t, receivedAt); len(uploads) != 1 || uploads[0][0] != "a.png" {
		t.Fatalf("uploads %q", uploads)
	}
}

// failedPaste pastes a laptop file into a session whose box runs pasteFiles,
// and waits for the paste to reach the session unchanged with a notice
// holding why.
func failedPaste(t *testing.T, pasteFiles, why string) {
	t.Helper()
	h := newPTY(t)
	shot := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(shot, append(onePixelPNG, make([]byte, 1<<20)...), 0o600); err != nil {
		t.Fatal(err)
	}
	ssh := fakeBox(t, echoLine+`; sleep 1; exit 0`, pasteFiles)
	done := start(testOptions(ssh, h))
	h.waitFor(t, "ready")
	paste := "\x1b[200~" + shot + "\x1b[201~"
	if _, err := h.master.Write([]byte(paste + "\n")); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "got["+paste+"]")
	h.waitFor(t, why)
	if r := wait(t, done); r.err != nil {
		t.Fatal(r.err)
	}
}

func TestABoxThatEndsMidUploadFailsThePaste(t *testing.T) {
	failedPaste(t, `printf '1\n0'; echo "remote-agents-box: receiving a.png: no space left" >&2; exit 1`, "no space left")
}

func TestABoxThatAnswersNonsenseFailsThePastePromptly(t *testing.T) {
	failedPaste(t, `printf '1\n0'; cat >/dev/null; printf '3\nabc%020000d' 0`, `the box stored an upload at "abc"`)
}

func TestAStalledUploadFailsThePaste(t *testing.T) {
	defer func(was time.Duration) { stallTimeout = was }(stallTimeout)
	stallTimeout = 300 * time.Millisecond
	failedPaste(t, `printf '1\n0'; sleep 5`, "stalled")
}

// progressPaste pastes a 1 MB laptop file into a session whose box runs
// pasteFiles, and waits for the paste to reach the session as /box/p.png.
func progressPaste(t *testing.T, pasteFiles string) {
	t.Helper()
	h := newPTY(t)
	shot := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(shot, append(onePixelPNG, make([]byte, 1<<20)...), 0o600); err != nil {
		t.Fatal(err)
	}
	ssh := fakeBox(t, echoLine+`; exit 0`, pasteFiles)
	done := start(testOptions(ssh, h))
	h.waitFor(t, "ready")
	if _, err := h.master.Write([]byte("\x1b[200~" + shot + "\x1b[201~\n")); err != nil {
		t.Fatal(err)
	}
	if r := wait(t, done); r.err != nil || r.code != 0 {
		t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
	}
	if want := "got[\x1b[200~/box/p.png\x1b[201~]"; !strings.Contains(h.output(), want) {
		t.Fatalf("output %q lacks %q", h.output(), want)
	}
}

func TestProgressFromTheBoxKeepsASlowUploadAlive(t *testing.T) {
	defer func(was time.Duration) { stallTimeout = was }(stallTimeout)
	stallTimeout = 300 * time.Millisecond
	progressPaste(t, `printf '1\n0'; cat >/dev/null; for i in 1 2 3 4 5 6 7 8; do sleep 0.1; printf '0\n'; done; printf '10\n/box/p.png'`)
}

func TestTheLaptopReadsProgressWhileItSends(t *testing.T) {
	// More progress than the pipes hold, before the box reads any upload.
	progressPaste(t, `printf '1\n0'; awk 'BEGIN { for (i = 0; i < 200000; i++) printf "0\n" }'; cat >/dev/null; printf '10\n/box/p.png'`)
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
		`printf '1\n0'; cat >/dev/null; sleep 0.5; printf '10\n/box/p.png'`)
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

func TestCtrlCBeforeTheSessionDrawsDetachesWithoutReachingIt(t *testing.T) {
	h := newPTY(t)
	got := filepath.Join(t.TempDir(), "got")
	ssh := fakeBox(t, `sleep 1; echo ready; dd bs=1 count=1 of='`+got+`' 2>/dev/null; sleep 5`, `exit 9`)
	done := start(testOptions(ssh, h))
	time.Sleep(200 * time.Millisecond)
	if _, err := h.master.Write([]byte("\x03")); err != nil {
		t.Fatal(err)
	}
	if r := wait(t, done); r.err != nil || r.code != 130 {
		t.Fatalf("Run = %d, %v; output %q", r.code, r.err, h.output())
	}
	if data, err := os.ReadFile(got); err == nil && len(data) > 0 {
		t.Fatalf("the session got %q", data)
	}
}

func TestALoneEscReachesTheSessionPromptly(t *testing.T) {
	h := newPTY(t)
	ssh := fakeBox(t, `echo ready; c=$(dd bs=1 count=1 2>/dev/null | od -An -c); echo "key[$c]"; exit 0`, `exit 9`)
	done := start(testOptions(ssh, h))
	h.waitFor(t, "ready")
	sent := time.Now()
	if _, err := h.master.Write([]byte("\x1b")); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "033")
	if elapsed := time.Since(sent); elapsed > 300*time.Millisecond {
		t.Fatalf("Esc took %s", elapsed)
	}
	if r := wait(t, done); r.err != nil || r.code != 0 {
		t.Fatalf("Run = %d, %v", r.code, r.err)
	}
}
