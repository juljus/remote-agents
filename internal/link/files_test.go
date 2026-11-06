package link

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"remote-agents/internal/control"
	"remote-agents/internal/laptopfiles"
	"remote-agents/internal/linkproto"
	"remote-agents/internal/sshrun"
)

const filesSession = "claude-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

// fakeFetcher serves box files from a map.
type fakeFetcher map[string]string

func (f fakeFetcher) Fetch(_ context.Context, boxPath string, w io.Writer) error {
	content, ok := f[boxPath]
	if !ok {
		return errors.New("box: no such file")
	}
	_, err := io.WriteString(w, content)
	return err
}

type recordRevealer struct{ paths []string }

func (r *recordRevealer) Reveal(_ context.Context, path string) error {
	r.paths = append(r.paths, path)
	return nil
}

type filesFixture struct {
	h        *Handler
	reg      *control.Registry
	home     string
	revealed *recordRevealer
	// declined, when set, is the user's answer to every prompt; else they approve.
	declined error
}

func newFilesFixture(t *testing.T, box fakeFetcher) *filesFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &filesFixture{home: filepath.Join(root, "home"), revealed: &recordRevealer{}}
	for _, dir := range []string{filepath.Join(f.home, "Downloads"), filepath.Join(f.home, "docs"), filepath.Join(root, "trash")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h, _, reg := newHandler(t, &fakePasteboard{})
	h.Files = laptopfiles.Files{
		Records: laptopfiles.Records{Dir: filepath.Join(root, "pasted")},
		Home:    f.home,
		Trash:   func(p string) error { return os.Rename(p, filepath.Join(root, "trash", filepath.Base(p))) },
		Gate:    laptopfiles.NewGate(laptopfiles.ApproverFunc(func(context.Context, string) error { return f.declined }), time.Minute),
	}
	h.Fetcher = box
	h.Revealer = f.revealed
	h.Logf = func(format string, args ...any) {}
	f.h, f.reg = h, reg
	return f
}

// paste makes a laptop file holding content, pasted into filesSession.
func (f *filesFixture) paste(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(f.home, "docs", name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.h.Files.Records.Pasted(filesSession, path, sha(content)); err != nil {
		t.Fatal(err)
	}
	return path
}

// wrapper registers a wrapper for filesSession that collects its notices.
func (f *filesFixture) wrapper(t *testing.T) chan string {
	t.Helper()
	notices := make(chan string, 10)
	serveAndRegisterWrapper(t, f.reg, filesSession, control.Wrapper{Notice: func(text string) { notices <- text }})
	return notices
}

func TestWriteBackTakesTheBoxCopyOverThePastedFile(t *testing.T) {
	f := newFilesFixture(t, fakeFetcher{"/box/notes.md": "agent"})
	origin := f.paste(t, "notes.md", "laptop")
	notices := f.wrapper(t)
	res, err := f.h.WriteBack(context.Background(), linkproto.WriteBackRequest{Session: filesSession, Origin: origin, BoxPath: "/box/notes.md"})
	if err != nil {
		t.Fatal(err)
	}
	if res != (linkproto.WriteBackResult{Outcome: "written", Path: origin, SHA256: sha("agent")}) {
		t.Fatalf("result %+v", res)
	}
	if data, _ := os.ReadFile(origin); string(data) != "agent" {
		t.Fatalf("origin holds %q", data)
	}
	select {
	case n := <-notices:
		t.Fatalf("a clean write-back showed %q", n)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAConflictedWriteBackSaysWhereTheBoxVersionWent(t *testing.T) {
	f := newFilesFixture(t, fakeFetcher{"/box/notes.md": "agent"})
	origin := f.paste(t, "notes.md", "laptop")
	if err := os.WriteFile(origin, []byte("edited on the laptop"), 0o600); err != nil {
		t.Fatal(err)
	}
	notices := f.wrapper(t)
	res, err := f.h.WriteBack(context.Background(), linkproto.WriteBackRequest{Session: filesSession, Origin: origin, BoxPath: "/box/notes.md"})
	if err != nil || res.Outcome != "conflict" || filepath.Base(res.Path) != "notes (box).md" {
		t.Fatalf("result %+v, %v", res, err)
	}
	if n := <-notices; !strings.Contains(n, "notes.md changed on the laptop") || !strings.Contains(n, "notes (box).md") {
		t.Fatalf("notice %q", n)
	}
}

func TestARefusedWriteBackFailsAndSaysWhy(t *testing.T) {
	f := newFilesFixture(t, fakeFetcher{"/box/x": "agent"})
	notices := f.wrapper(t)
	other := filepath.Join(f.home, "docs", "other.md")
	_, err := f.h.WriteBack(context.Background(), linkproto.WriteBackRequest{Session: filesSession, Origin: other, BoxPath: "/box/x"})
	if !errors.Is(err, linkproto.ErrRefused) {
		t.Fatalf("err = %v, want a refusal the box sees as one", err)
	}
	if n := <-notices; !strings.Contains(n, "other.md not written back") {
		t.Fatalf("notice %q", n)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("refused write-back created %s", other)
	}
}

func TestAFailedFetchIsNoRefusal(t *testing.T) {
	f := newFilesFixture(t, fakeFetcher{})
	origin := f.paste(t, "notes.md", "laptop")
	_, err := f.h.WriteBack(context.Background(), linkproto.WriteBackRequest{Session: filesSession, Origin: origin, BoxPath: "/box/gone"})
	if err == nil || errors.Is(err, linkproto.ErrRefused) {
		t.Fatalf("err = %v, want a failure the box tries again", err)
	}
}

func TestLaptopSaveSavesIntoDownloadsAndRevealsIt(t *testing.T) {
	f := newFilesFixture(t, fakeFetcher{"/box/out/report.pdf": "%PDF"})
	saved, err := f.h.LaptopSave(context.Background(), linkproto.LaptopSaveRequest{Session: filesSession, BoxPath: "/box/out/report.pdf"})
	want := filepath.Join(f.home, "Downloads", "report.pdf")
	if err != nil || saved != (linkproto.LaptopSaveResult{Path: want, SHA256: sha("%PDF"), Linked: true}) {
		t.Fatalf("saved %+v, %v", saved, err)
	}
	if !slices.Equal(f.revealed.paths, []string{want}) {
		t.Fatalf("revealed %q", f.revealed.paths)
	}
	notices := f.wrapper(t)
	f.declined = errors.New("Canceled by user.")
	_, err = f.h.LaptopSave(context.Background(), linkproto.LaptopSaveRequest{Session: filesSession, BoxPath: "/box/out/report.pdf", Dest: "/etc/report.pdf"})
	if !errors.Is(err, linkproto.ErrRefused) || strings.Count(err.Error(), "refused") != 1 {
		t.Fatalf("declined save: %v", err)
	}
	if n := <-notices; !strings.Contains(n, "report.pdf not saved") || !strings.Contains(n, "Canceled by user.") {
		t.Fatalf("notice %q", n)
	}
	if _, err := f.h.LaptopSave(context.Background(), linkproto.LaptopSaveRequest{BoxPath: "relative.pdf"}); err == nil {
		t.Fatal("saved a relative box path")
	}
}

// A saved file takes its box file's later changes as a write-back.
func TestASavedFileTakesItsBoxFilesChanges(t *testing.T) {
	box := fakeFetcher{"/box/out/report.md": "v1"}
	f := newFilesFixture(t, box)
	saved, err := f.h.LaptopSave(context.Background(), linkproto.LaptopSaveRequest{Session: filesSession, BoxPath: "/box/out/report.md"})
	if err != nil {
		t.Fatal(err)
	}
	box["/box/out/report.md"] = "v2"
	res, err := f.h.WriteBack(context.Background(), linkproto.WriteBackRequest{Session: filesSession, Origin: saved.Path, BoxPath: "/box/out/report.md"})
	if err != nil || res != (linkproto.WriteBackResult{Outcome: "written", Path: saved.Path, SHA256: sha("v2")}) {
		t.Fatalf("write-back %+v, %v", res, err)
	}
}

func TestTheBoxCannotSendNoticesToTheDaemon(t *testing.T) {
	h, _, _ := newHandler(t, &fakePasteboard{})
	resp := linkproto.DispatchTo(h)(context.Background(), linkproto.Request{Notice: &linkproto.NoticeRequest{Session: filesSession, Text: "x"}})
	if resp.Err() == nil {
		t.Fatal("the daemon took a notice from the box")
	}
}

func TestSSHFetcherStreamsTheBoxFile(t *testing.T) {
	dir := t.TempDir()
	asked := filepath.Join(dir, "asked")
	ssh := fakeSSH(t, `cat > '`+asked+`'; printf '5\nhello'`)
	var got strings.Builder
	if err := (SSHFetcher{SSH: ssh, Box: sshrun.Box{Host: "box", Install: "ra-test"}}).Fetch(context.Background(), "/box/a file", &got); err != nil {
		t.Fatal(err)
	}
	if got.String() != "hello" {
		t.Fatalf("fetched %q", got.String())
	}
	if data, _ := os.ReadFile(asked); string(data) != "11\n/box/a file" {
		t.Fatalf("asked for %q", data)
	}
}

func TestSSHFetcherFailures(t *testing.T) {
	for script, want := range map[string]string{
		`cat >/dev/null; echo "remote-agents-box: /box/x is not a regular file" >&2; exit 1`: "box: /box/x is not a regular file",
		`cat >/dev/null; printf '10\nshort'`:                                                 "unexpected EOF",
		`cat >/dev/null; printf '1\na1\nb'`:                                                  "more than the file",
		`cat >/dev/null`:                                                                     "no file",
		`echo "ssh: connect to host box: Connection refused" >&2; exit 255`:                  "Connection refused",
	} {
		var got strings.Builder
		err := (SSHFetcher{SSH: fakeSSH(t, script), Box: sshrun.Box{Host: "box", Install: "ra-test"}}).Fetch(context.Background(), "/box/x", &got)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", script, err, want)
		}
	}
}

// fakeSSH is an ssh that runs script whatever it is asked.
func fakeSSH(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestCommandRevealerRunsItsCommandOnThePath(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	r := CommandRevealer{Argv: []string{"/bin/sh", "-c", `printf %s "$0" > '` + out + `'`}}
	if err := r.Reveal(context.Background(), "/Users/me/Downloads/a b.pdf"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(out); string(data) != "/Users/me/Downloads/a b.pdf" {
		t.Fatalf("revealed %q", data)
	}
	if err := (CommandRevealer{Argv: []string{"/usr/bin/false"}}).Reveal(context.Background(), "/x"); err == nil {
		t.Fatal("a failing revealer went unreported")
	}
}
