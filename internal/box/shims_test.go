package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/linkproto"
)

type fakeLaptop struct {
	clipboard map[string][]byte
	opened    []string
	written   map[string][]byte
	hook      func(linkproto.HookRequest) (linkproto.HookResult, error)
	writeBack func(linkproto.WriteBackRequest) (linkproto.WriteBackResult, error)
	saves     []linkproto.LaptopSaveRequest
	saveErr   error
	// pasted makes every save one over a file pasted into the session, and
	// conflict one beside a file the user changed since the last save.
	pasted, conflict bool
	down             bool
}

func (f *fakeLaptop) err() error {
	if f.down {
		return fmt.Errorf("%w: dial unix: no such file", linkproto.ErrLinkDown)
	}
	return nil
}

func (f *fakeLaptop) OpenURL(_ context.Context, url string) error {
	if err := f.err(); err != nil {
		return err
	}
	f.opened = append(f.opened, url)
	return nil
}

func (f *fakeLaptop) ClipboardTargets(context.Context) ([]string, error) {
	if err := f.err(); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range []string{"image/png", "text/plain"} {
		if _, ok := f.clipboard[m]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeLaptop) ClipboardRead(_ context.Context, mime string) ([]byte, error) {
	if err := f.err(); err != nil {
		return nil, err
	}
	data, ok := f.clipboard[mime]
	if !ok {
		return nil, errors.New("laptop: clipboard has no " + mime)
	}
	return data, nil
}

func (f *fakeLaptop) ClipboardWrite(_ context.Context, mime string, data []byte) error {
	if err := f.err(); err != nil {
		return err
	}
	f.written = map[string][]byte{mime: data}
	return nil
}

func (f *fakeLaptop) Hook(_ context.Context, req linkproto.HookRequest) (linkproto.HookResult, error) {
	if err := f.err(); err != nil {
		return linkproto.HookResult{}, err
	}
	return f.hook(req)
}

func (f *fakeLaptop) WriteBack(_ context.Context, req linkproto.WriteBackRequest) (linkproto.WriteBackResult, error) {
	if err := f.err(); err != nil {
		return linkproto.WriteBackResult{}, err
	}
	return f.writeBack(req)
}

// LaptopSave saves as a laptop where nothing was pasted: a save from a
// session follows its box file.
func (f *fakeLaptop) LaptopSave(_ context.Context, req linkproto.LaptopSaveRequest) (linkproto.LaptopSaveResult, error) {
	if err := f.err(); err != nil {
		return linkproto.LaptopSaveResult{}, err
	}
	if f.saveErr != nil {
		return linkproto.LaptopSaveResult{}, f.saveErr
	}
	f.saves = append(f.saves, req)
	sum, err := fileSum(req.BoxPath)
	if err != nil {
		return linkproto.LaptopSaveResult{}, err
	}
	res := linkproto.LaptopSaveResult{Path: "/Users/me/Downloads/" + filepath.Base(req.BoxPath), SHA256: sum}
	if req.Session != "" && !f.pasted {
		res.Following = res.Path
	}
	if f.conflict {
		res.Path = strings.TrimSuffix(res.Path, filepath.Ext(res.Path)) + " (box)" + filepath.Ext(res.Path)
	}
	return res, nil
}

type run struct {
	code   int
	stdout string
	stderr string
}

func runShim(t *testing.T, name string, args []string, stdin string, laptop Laptop, env map[string]string) run {
	t.Helper()
	return runShimIn(t, testLayout(t), name, args, stdin, laptop, env)
}

func runShimIn(t *testing.T, l boxlayout.Layout, name string, args []string, stdin string, laptop Laptop, env map[string]string) run {
	t.Helper()
	var out, errOut bytes.Buffer
	io := IO{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut}
	code := RunShim(context.Background(), l, name, args, io, laptop, func(k string) string { return env[k] })
	return run{code, out.String(), errOut.String()}
}

var png = string([]byte{0x89, 'P', 'N', 'G', 0, 1})

func TestXclipClaudeImagePaste(t *testing.T) {
	l := &fakeLaptop{clipboard: map[string][]byte{"image/png": []byte(png)}}
	r := runShim(t, "xclip", []string{"-selection", "clipboard", "-t", "TARGETS", "-o"}, "", l, nil)
	if r.code != 0 || r.stdout != "TARGETS\nimage/png\n" {
		t.Fatalf("TARGETS = %+v", r)
	}
	r = runShim(t, "xclip", []string{"-selection", "clipboard", "-t", "image/png", "-o"}, "", l, nil)
	if r.code != 0 || r.stdout != png {
		t.Fatalf("image read = %+v", r)
	}
	r = runShim(t, "xclip", []string{"-selection", "clipboard", "-t", "image/bmp", "-o"}, "", l, nil)
	if r.code == 0 || !strings.Contains(r.stderr, "image/bmp") {
		t.Fatalf("bmp read should fail, got %+v", r)
	}
}

func TestXclipText(t *testing.T) {
	l := &fakeLaptop{clipboard: map[string][]byte{"text/plain": []byte("hello")}}
	r := runShim(t, "xclip", []string{"-selection", "clipboard", "-t", "TARGETS", "-o"}, "", l, nil)
	if r.stdout != "TARGETS\nUTF8_STRING\ntext/plain\n" {
		t.Fatalf("TARGETS = %q", r.stdout)
	}
	for _, args := range [][]string{
		{"-selection", "clipboard", "-o"},
		{"-sel", "clipboard", "-t", "text/plain", "-o"},
		{"-selection", "clipboard", "-t", "UTF8_STRING", "-out"},
	} {
		r := runShim(t, "xclip", args, "", l, nil)
		if r.code != 0 || r.stdout != "hello" {
			t.Fatalf("%v = %+v", args, r)
		}
	}
	r = runShim(t, "xclip", []string{"-selection", "clipboard"}, "copied text", l, nil)
	if r.code != 0 || string(l.written["text/plain"]) != "copied text" {
		t.Fatalf("copy = %+v, written %q", r, l.written)
	}
	r = runShim(t, "xclip", []string{"-selection", "clipboard", "-i", "-t", "image/png"}, png, l, nil)
	if r.code != 0 || string(l.written["image/png"]) != png {
		t.Fatalf("image copy = %+v", r)
	}
}

func TestXclipRejectsPrimaryAndUnknownOptions(t *testing.T) {
	l := &fakeLaptop{clipboard: map[string][]byte{"text/plain": []byte("x")}}
	for _, args := range [][]string{
		{"-o"}, // xclip defaults to the primary selection
		{"-selection", "primary", "-o"},
		{"-selection", "clipboard", "-loops", "1"},
		{"-selection", "clipboard", "somefile"},
	} {
		r := runShim(t, "xclip", args, "", l, nil)
		if r.code == 0 {
			t.Fatalf("%v should fail, got %+v", args, r)
		}
	}
}

func TestWlPaste(t *testing.T) {
	l := &fakeLaptop{clipboard: map[string][]byte{"image/png": []byte(png), "text/plain": []byte("hi")}}
	r := runShim(t, "wl-paste", []string{"-l"}, "", l, nil)
	if r.stdout != "image/png\ntext/plain\n" {
		t.Fatalf("list = %q", r.stdout)
	}
	r = runShim(t, "wl-paste", []string{"--type", "image/png"}, "", l, nil)
	if r.stdout != png {
		t.Fatalf("image = %q", r.stdout)
	}
	r = runShim(t, "wl-paste", nil, "", l, nil)
	if r.stdout != "hi\n" {
		t.Fatalf("text = %q", r.stdout)
	}
	r = runShim(t, "wl-paste", []string{"--no-newline"}, "", l, nil)
	if r.stdout != "hi" {
		t.Fatalf("text -n = %q", r.stdout)
	}
	if r := runShim(t, "wl-paste", []string{"--primary"}, "", l, nil); r.code == 0 {
		t.Fatal("primary should fail")
	}
}

func TestXdgOpen(t *testing.T) {
	l := &fakeLaptop{}
	r := runShim(t, "xdg-open", []string{"https://login.example.com/?a=b"}, "", l, nil)
	if r.code != 0 || len(l.opened) != 1 || l.opened[0] != "https://login.example.com/?a=b" {
		t.Fatalf("open = %+v %v", r, l.opened)
	}
	if r := runShim(t, "xdg-open", nil, "", l, nil); r.code == 0 {
		t.Fatal("xdg-open without url should fail")
	}
}

func TestLinkDownIsLoud(t *testing.T) {
	l := &fakeLaptop{down: true}
	r := runShim(t, "xdg-open", []string{"https://x"}, "", l, nil)
	if r.code == 0 || !strings.Contains(r.stderr, "laptop link is down") {
		t.Fatalf("expected loud link-down failure, got %+v", r)
	}
	r = runShim(t, "xclip", []string{"-selection", "clipboard", "-t", "TARGETS", "-o"}, "", l, nil)
	if r.code == 0 || !strings.Contains(r.stderr, "laptop link is down") {
		t.Fatalf("expected loud link-down failure, got %+v", r)
	}
}

func TestCmuxHookRelay(t *testing.T) {
	var seen linkproto.HookRequest
	l := &fakeLaptop{hook: func(req linkproto.HookRequest) (linkproto.HookResult, error) {
		seen = req
		return linkproto.HookResult{Stdout: []byte(`{"decision":"block"}`), Stderr: []byte("note"), ExitCode: 2}, nil
	}}
	env := map[string]string{"REMOTE_AGENTS_SESSION": "claude-abc"}
	r := runShim(t, "cmux", []string{"hooks", "claude", "pre-tool-use"}, `{"tool":"Bash"}`, l, env)
	if r.code != 2 || r.stdout != `{"decision":"block"}` || r.stderr != "note" {
		t.Fatalf("relay = %+v", r)
	}
	if seen.Session != "claude-abc" || strings.Join(seen.Argv, " ") != "hooks claude pre-tool-use" || string(seen.Stdin) != `{"tool":"Bash"}` {
		t.Fatalf("request = %+v", seen)
	}
}

func TestCmuxHookWithoutTerminalOrLinkIsDropped(t *testing.T) {
	env := map[string]string{"REMOTE_AGENTS_SESSION": "claude-abc"}
	noTerm := &fakeLaptop{hook: func(linkproto.HookRequest) (linkproto.HookResult, error) {
		return linkproto.HookResult{}, fmt.Errorf("%w: claude-abc", linkproto.ErrNoTerminal)
	}}
	for name, l := range map[string]*fakeLaptop{"no terminal": noTerm, "link down": {down: true}} {
		r := runShim(t, "cmux", []string{"hooks", "claude", "stop"}, "{}", l, env)
		if r.code != 0 || r.stderr == "" {
			t.Fatalf("%s: want exit 0 with a note, got %+v", name, r)
		}
	}
}

func TestCmuxHookNeedsSession(t *testing.T) {
	r := runShim(t, "cmux", []string{"hooks", "claude", "stop"}, "{}", &fakeLaptop{}, nil)
	if r.code == 0 {
		t.Fatal("expected failure without REMOTE_AGENTS_SESSION")
	}
}

func TestLaptopSaveAsksTheLaptopToFetchTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(path, []byte("%PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	l := &fakeLaptop{}
	env := map[string]string{SessionEnvVar: pasteSession}
	r := runShim(t, "laptop-save", []string{"report.pdf"}, "", l, env)
	if r.code != 0 || r.stdout != "saved to /Users/me/Downloads/report.pdf on the laptop; later changes to "+path+" go there on their own\n" {
		t.Fatalf("laptop-save = %+v", r)
	}
	r = runShim(t, "laptop-save", []string{path, "~/Downloads/x.pdf"}, "", l, nil)
	if r.code != 0 || r.stdout != "saved to /Users/me/Downloads/report.pdf on the laptop\n" {
		t.Fatalf("laptop-save to a path = %+v", r)
	}
	want := []linkproto.LaptopSaveRequest{
		{Session: pasteSession, BoxPath: path},
		{BoxPath: path, Dest: "~/Downloads/x.pdf"},
	}
	if !slices.Equal(l.saves, want) {
		t.Fatalf("saves %+v, want %+v", l.saves, want)
	}
}

// A laptop file a save from a session made follows the box file: the link
// holder's watcher finds its record.
func TestLaptopSaveLeavesTheLaptopFileFollowingTheBoxFile(t *testing.T) {
	l := testLayout(t)
	path := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runShimIn(t, l, "laptop-save", []string{path}, "", &fakeLaptop{}, map[string]string{SessionEnvVar: pasteSession})
	if r.code != 0 {
		t.Fatalf("laptop-save = %+v", r)
	}
	copies, err := sessionCopies(l)
	want := pastedCopy{Session: pasteSession, Origin: "/Users/me/Downloads/report.md", Path: path, Linked: true, Synced: sha("v1")}
	if err != nil || len(copies) != 1 || copies[0].record == "" {
		t.Fatalf("copies %+v, %v", copies, err)
	}
	if copies[0].record = ""; copies[0] != want {
		t.Fatalf("copy %+v, want %+v", copies[0], want)
	}
}

// A save beside a laptop file the user changed since says so, and follows the file.
func TestLaptopSaveBesideAChangedFileSaysSo(t *testing.T) {
	l := testLayout(t)
	path := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(path, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runShimIn(t, l, "laptop-save", []string{path}, "", &fakeLaptop{conflict: true}, map[string]string{SessionEnvVar: pasteSession})
	want := "the user changed /Users/me/Downloads/report.md on the laptop since the last save, so this one went beside it to /Users/me/Downloads/report (box).md; later changes to " + path + " follow on their own\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("laptop-save = %+v", r)
	}
	if copies, err := sessionCopies(l); err != nil || len(copies) != 1 || copies[0].Origin != "/Users/me/Downloads/report.md" {
		t.Fatalf("copies %+v, %v", copies, err)
	}
}

// A save over a file pasted into the session leaves its pasted copy the one
// that goes back, and the agent hears so.
func TestLaptopSaveOverAPastedFileSaysWhichCopyGoesBack(t *testing.T) {
	l := testLayout(t)
	path := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runShimIn(t, l, "laptop-save", []string{path}, "", &fakeLaptop{pasted: true}, map[string]string{SessionEnvVar: pasteSession})
	if r.code != 0 || !strings.Contains(r.stdout, "it is pasted into this session") {
		t.Fatalf("laptop-save = %+v", r)
	}
	if copies, err := sessionCopies(l); err != nil || len(copies) != 0 {
		t.Fatalf("copies %+v, %v", copies, err)
	}
}

func TestLaptopSaveRefusesWhatCannotGo(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(100<<20 + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, args := range [][]string{{}, {"a", "b", "c"}, {dir}, {big}, {filepath.Join(dir, "missing")}} {
		l := &fakeLaptop{}
		if r := runShim(t, "laptop-save", args, "", l, nil); r.code == 0 || r.stderr == "" || len(l.saves) != 0 {
			t.Errorf("laptop-save %q = %+v, saves %v", args, r, l.saves)
		}
	}
	l := &fakeLaptop{saveErr: errors.New("laptop: refused: /etc/x not approved: Canceled by user.; the box keeps its version")}
	file := filepath.Join(dir, "x")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := runShim(t, "laptop-save", []string{file, "/etc/x"}, "", l, nil); r.code == 0 || !strings.Contains(r.stderr, "refused") {
		t.Fatalf("refused save = %+v", r)
	}
}

func TestLaptopSaveHelpSaysWhatItDoes(t *testing.T) {
	r := runShim(t, "laptop-save", []string{"--help"}, "", &fakeLaptop{}, nil)
	if r.code != 0 || !strings.Contains(r.stdout, "~/Downloads") || !strings.Contains(r.stdout, "Touch ID") {
		t.Fatalf("--help = %+v", r)
	}
}
