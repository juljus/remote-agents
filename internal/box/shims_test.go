package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"remote-agents/internal/linkproto"
)

type fakeLaptop struct {
	clipboard map[string][]byte
	opened    []string
	written   map[string][]byte
	hook      func(linkproto.HookRequest) (linkproto.HookResult, error)
	down      bool
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

type run struct {
	code   int
	stdout string
	stderr string
}

func runShim(t *testing.T, name string, args []string, stdin string, laptop Laptop, env map[string]string) run {
	t.Helper()
	var out, errOut bytes.Buffer
	io := IO{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut}
	code := RunShim(context.Background(), name, args, io, laptop, func(k string) string { return env[k] })
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
