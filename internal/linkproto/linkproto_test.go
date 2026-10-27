package linkproto

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeHandler struct {
	opened    []string
	clipboard map[string][]byte
	hookSeen  chan HookRequest
	hookBlock bool
	hookDone  chan error
}

func (f *fakeHandler) OpenURL(_ context.Context, url string) error {
	if strings.HasPrefix(url, "file:") {
		return errors.New("refusing non-http url")
	}
	f.opened = append(f.opened, url)
	return nil
}

func (f *fakeHandler) ClipboardTargets(context.Context) ([]string, error) {
	var out []string
	for k := range f.clipboard {
		out = append(out, k)
	}
	return out, nil
}

func (f *fakeHandler) ClipboardRead(_ context.Context, mime string) ([]byte, error) {
	data, ok := f.clipboard[mime]
	if !ok {
		return nil, errors.New("target not available")
	}
	return data, nil
}

func (f *fakeHandler) ClipboardWrite(_ context.Context, mime string, data []byte) error {
	f.clipboard = map[string][]byte{mime: data}
	return nil
}

func (f *fakeHandler) Hook(ctx context.Context, req HookRequest) (HookResult, error) {
	f.hookSeen <- req
	if f.hookBlock {
		<-ctx.Done()
		f.hookDone <- ctx.Err()
		return HookResult{}, ctx.Err()
	}
	return HookResult{Stdout: append([]byte("out:"), req.Stdin...), Stderr: []byte("err"), ExitCode: 2}, nil
}

func TestDialFailureIsLinkDown(t *testing.T) {
	c := Client{Socket: "/nonexistent/remote-agents.sock", DialTimeout: time.Second}
	err := c.OpenURL(context.Background(), "https://example.com")
	if !errors.Is(err, ErrLinkDown) {
		t.Fatalf("err = %v, want ErrLinkDown", err)
	}
}

func TestRequestValidation(t *testing.T) {
	bad := []Request{
		{},
		{OpenURL: &OpenURLRequest{URL: "x"}, ClipboardTargets: &struct{}{}},
	}
	for i, r := range bad {
		if err := r.Validate(); err == nil {
			t.Fatalf("request %d: expected validation error", i)
		}
	}
}
