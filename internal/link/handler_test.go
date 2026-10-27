package link

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"remote-agents/internal/control"
	"remote-agents/internal/hookexec"
	"remote-agents/internal/linkproto"
)

type fakePasteboard struct {
	text    string
	png     []byte
	written map[string][]byte
}

func (f *fakePasteboard) Types() ([]string, error) {
	var out []string
	if f.png != nil {
		out = append(out, MIMEPNG)
	}
	if f.text != "" {
		out = append(out, MIMEText)
	}
	return out, nil
}
func (f *fakePasteboard) PNG() ([]byte, error)  { return f.png, nil }
func (f *fakePasteboard) Text() (string, error) { return f.text, nil }
func (f *fakePasteboard) SetText(s string) error {
	f.written = map[string][]byte{MIMEText: []byte(s)}
	return nil
}
func (f *fakePasteboard) SetPNG(b []byte) error {
	f.written = map[string][]byte{MIMEPNG: b}
	return nil
}

type recordOpener struct{ urls []string }

func (r *recordOpener) Open(_ context.Context, url string) error {
	r.urls = append(r.urls, url)
	return nil
}

type recordForwarder struct {
	ports []uint16
	busy  map[uint16]bool
}

func (r *recordForwarder) Forward(_ context.Context, port uint16) error {
	if r.busy[port] {
		return fmt.Errorf("laptop port %d is in use", port)
	}
	r.ports = append(r.ports, port)
	return nil
}

func newHandler(t *testing.T, pb *fakePasteboard) (*Handler, *recordOpener, *control.Registry) {
	t.Helper()
	h, op, _, reg := newHandlerForwarding(t, pb)
	return h, op, reg
}

func newHandlerForwarding(t *testing.T, pb *fakePasteboard) (*Handler, *recordOpener, *recordForwarder, *control.Registry) {
	t.Helper()
	reg := control.NewRegistry()
	op := &recordOpener{}
	fw := &recordForwarder{busy: map[uint16]bool{8631: true}}
	resolve := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "login.microsoftonline.com":
			return []netip.Addr{netip.MustParseAddr("20.190.160.1")}, nil
		case "grafana.internal":
			return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
		case "localtest.me":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		case "claude.ai", "dex.example", "idp.example", "docs.example":
			return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
		}
		return nil, errors.New("no such host")
	}
	return &Handler{Pasteboard: pb, Opener: op, Forwarder: fw, Registry: reg, Resolve: resolve}, op, fw, reg
}

func TestOpenURLPolicy(t *testing.T) {
	ctx := context.Background()
	for raw, ports := range map[string][]uint16{
		"https://login.microsoftonline.com/x":                  nil,
		"https://grafana.internal/d/1":                         nil,
		"http://192.168.1.1/":                                  nil,
		"http://100.64.0.27:3000/":                             nil,
		"http://localhost:8000":                                {8000},
		"http://127.0.0.1:8250/":                               {8250},
		"http://127.1:8000/":                                   {8000},
		"http://2130706433:8000/":                              {8000},
		"https://[::1]:9180/":                                  {9180},
		"http://app.localhost:3000/":                           {3000},
		"http://localtest.me:3000/":                            {3000},
		"https://docs.example/?next=http%3A%2F%2Flocalhost%2F": nil,
		"https://dex.example/auth?client_id=bao&redirect_uri=http%3A%2F%2Flocalhost%3A8250%2Foidc%2Fcallback":          {8250},
		"https://claude.ai/oauth/authorize?code=true&redirect_uri=http%3A%2F%2Flocalhost%3A54545%2Fcallback":           {54545},
		"https://idp.example/a?redirect_uri=http%3A%2F%2F127.0.0.1%3A18000%2F&next=http%3A%2F%2F%5B%3A%3A1%5D%3A18001": {18000, 18001},
		"http://localhost:8000/?redirect_uri=http%3A%2F%2Flocalhost%3A8000%2F":                                         {8000},
		"https://idp.example/a?redirect_uri=https%3A%2F%2Fapp.example%2Fcb&q=100%":                                     nil,
	} {
		h, op, fw, _ := newHandlerForwarding(t, &fakePasteboard{})
		if err := h.OpenURL(ctx, raw); err != nil {
			t.Fatalf("OpenURL(%q): %v", raw, err)
		}
		if len(op.urls) != 1 {
			t.Fatalf("OpenURL(%q) opened %v", raw, op.urls)
		}
		slices.Sort(fw.ports)
		if !slices.Equal(fw.ports, ports) {
			t.Fatalf("OpenURL(%q) forwarded %v, want %v", raw, fw.ports, ports)
		}
	}
	for raw, why := range map[string]string{
		"file:///etc/passwd":          "only http(s)",
		"javascript:alert(1)":         "only http(s)",
		"x-apple.systempreferences:":  "only http(s)",
		"https://":                    "only http(s)",
		"-a Terminal":                 "only http(s)",
		"http://localhost:8631/admin": "laptop port 8631 is in use",
		"https://idp.example/a?redirect_uri=http%3A%2F%2Flocalhost%3A8631%2F": "laptop port 8631 is in use",
		"http://127.0.0.2:8000/":        "127.0.0.2",
		"http://localhost/":             "port 80",
		"https://127.0.0.1/":            "port 443",
		"http://localtest.me:631/":      "port 631",
		"http://0/":                     "0.0.0.0",
		"http://0.0.0.1:8000/":          "0.0.0.1",
		"http://[::]:8000/":             "::",
		"http://169.254.169.254/latest": "link-local",
		"http://[ff02::1]/":             "multicast",
		"http://unresolvable.example/":  "no such host",
	} {
		h, op, fw, _ := newHandlerForwarding(t, &fakePasteboard{})
		err := h.OpenURL(ctx, raw)
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Fatalf("OpenURL(%q) = %v, want refusal mentioning %q", raw, err, why)
		}
		if len(op.urls) != 0 {
			t.Fatalf("refused OpenURL(%q) still opened %v", raw, op.urls)
		}
		if len(fw.ports) != 0 {
			t.Fatalf("refused OpenURL(%q) still forwarded %v", raw, fw.ports)
		}
	}
}

func TestBrowserIPv4(t *testing.T) {
	cases := map[string]string{
		"127.1": "127.0.0.1", "2130706433": "127.0.0.1", "0x7f000001": "127.0.0.1", "0177.0.0.1": "127.0.0.1",
		"10.1": "10.0.0.1", "192.168.1.1": "192.168.1.1", "192.168.1.1.": "192.168.1.1",
	}
	for in, want := range cases {
		got, ok := browserIPv4(in)
		if !ok || got.String() != want {
			t.Fatalf("browserIPv4(%q) = %v %v, want %s", in, got, ok, want)
		}
	}
	for _, in := range []string{"example.com", "1.2.3.4.5", "256.1.1.1", "1.2.3.256", "4294967296", ""} {
		if _, ok := browserIPv4(in); ok {
			t.Fatalf("browserIPv4(%q) should not parse", in)
		}
	}
}

func TestClipboard(t *testing.T) {
	pb := &fakePasteboard{text: "hello", png: []byte{0x89, 'P'}}
	h, _, _ := newHandler(t, pb)
	ctx := context.Background()
	types, err := h.ClipboardTargets(ctx)
	if err != nil || strings.Join(types, ",") != "image/png,text/plain" {
		t.Fatalf("targets = %v, %v", types, err)
	}
	if data, err := h.ClipboardRead(ctx, MIMEText); err != nil || string(data) != "hello" {
		t.Fatalf("text = %q, %v", data, err)
	}
	if data, err := h.ClipboardRead(ctx, MIMEPNG); err != nil || len(data) != 2 {
		t.Fatalf("png = %v, %v", data, err)
	}
	if _, err := h.ClipboardRead(ctx, "image/bmp"); err == nil {
		t.Fatal("bmp should be unavailable")
	}
	empty, _, _ := newHandler(t, &fakePasteboard{})
	if _, err := empty.ClipboardRead(ctx, MIMEPNG); err == nil {
		t.Fatal("reading a missing image should fail")
	}
	if err := h.ClipboardWrite(ctx, MIMEText, []byte("copied")); err != nil || string(pb.written[MIMEText]) != "copied" {
		t.Fatalf("write = %v %v", pb.written, err)
	}
	if err := h.ClipboardWrite(ctx, "image/gif", []byte("x")); err == nil {
		t.Fatal("writing gif should fail")
	}
}

func fakeCmux(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cmux")
	script := "#!/bin/sh\necho \"args=$* surface=$CMUX_SURFACE_ID\"\ncat\nexit 3\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHookIsRunByTheRegisteredWrapper(t *testing.T) {
	h, _, reg := newHandler(t, &fakePasteboard{})
	env := map[string]string{"CMUX_SURFACE_ID": "surface-1", hookexec.BinEnvVar: fakeCmux(t)}
	serveAndRegister(t, reg, "claude-x", hookexec.Executor{
		Env:     func([]string) map[string]string { return env },
		Allowed: [][]string{{"hooks", "claude", "stop"}},
	})
	res, err := h.Hook(context.Background(), linkproto.HookRequest{
		Session: "claude-x", Argv: []string{"hooks", "claude", "stop"}, Stdin: []byte(`{"session_id":"x"}`),
	})
	if err != nil {
		t.Fatalf("Hook: %v", err)
	}
	want := "args=hooks claude stop surface=surface-1\n{\"session_id\":\"x\"}"
	if string(res.Stdout) != want || res.ExitCode != 3 {
		t.Fatalf("result = %q / %d", res.Stdout, res.ExitCode)
	}
}

func TestHookUnknownSession(t *testing.T) {
	h, _, _ := newHandler(t, &fakePasteboard{})
	_, err := h.Hook(context.Background(), linkproto.HookRequest{Session: "nope", Argv: []string{"hooks", "claude", "stop"}})
	if !errors.Is(err, linkproto.ErrNoTerminal) {
		t.Fatalf("err = %v, want ErrNoTerminal", err)
	}
}
