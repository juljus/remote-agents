package link

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandOpenerPassesTheURLAlone(t *testing.T) {
	dir := t.TempDir()
	got := filepath.Join(dir, "got")
	opener := filepath.Join(dir, "opener")
	script := "#!/bin/sh\nprintf '%s|' \"$@\" > " + got + "\n[ \"$1\" = fail ] && { echo no browser >&2; exit 1; }\nexit 0\n"
	if err := os.WriteFile(opener, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	o := CommandOpener{Path: opener}
	if err := o.Open(context.Background(), "https://example.com/a b"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "https://example.com/a b|" {
		t.Fatalf("opener argv = %q", data)
	}
	if err := o.Open(context.Background(), "fail"); err == nil || !strings.Contains(err.Error(), "no browser") {
		t.Fatalf("failing opener: %v", err)
	}
}
