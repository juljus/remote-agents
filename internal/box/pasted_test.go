package box

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"remote-agents/internal/pastefile"
)

func frames(t *testing.T, files ...[]byte) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	for _, f := range files {
		if err := pastefile.WriteFrame(&b, f); err != nil {
			t.Fatal(err)
		}
	}
	return &b
}

func TestStorePastedKeepsEachImageByContentHash(t *testing.T) {
	l := testLayout(t)
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	var out bytes.Buffer
	if err := StorePasted(l, frames(t, onePixelPNG, gif), &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("output %q", out.String())
	}
	for i, tc := range []struct {
		data []byte
		ext  string
	}{{onePixelPNG, "png"}, {gif, "gif"}} {
		sum := sha256.Sum256(tc.data)
		want := filepath.Join(l.PastedDir(), hex.EncodeToString(sum[:])+"."+tc.ext)
		if lines[i] != want {
			t.Fatalf("path %d = %q, want %q", i, lines[i], want)
		}
		if got, err := os.ReadFile(want); err != nil || !bytes.Equal(got, tc.data) {
			t.Fatalf("stored %d: %v", i, err)
		}
	}
}

func TestStorePastedRefreshesAKnownImageAndPrunesOldOnes(t *testing.T) {
	l := testLayout(t)
	var out bytes.Buffer
	if err := StorePasted(l, frames(t, onePixelPNG), &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	stored := strings.TrimSpace(out.String())
	old := filepath.Join(l.PastedDir(), "old.png")
	if err := os.WriteFile(old, onePixelPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	weekAgo := time.Now().Add(-8 * 24 * time.Hour)
	for _, path := range []string{stored, old} {
		if err := os.Chtimes(path, weekAgo, weekAgo); err != nil {
			t.Fatal(err)
		}
	}
	out.Reset()
	if err := StorePasted(l, frames(t, onePixelPNG), &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != stored {
		t.Fatalf("second store gave %q, want %q", out.String(), stored)
	}
	if info, err := os.Stat(stored); err != nil || time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("pasting it again did not keep %s fresh: %v", stored, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old paste survived: %v", err)
	}
}

func TestStorePastedRefusesWhatIsNotAnImage(t *testing.T) {
	l := testLayout(t)
	var out bytes.Buffer
	err := StorePasted(l, frames(t, []byte("#!/bin/sh\nrm -rf ~\n")), &out, time.Now())
	if err == nil || !strings.Contains(err.Error(), "not an image") {
		t.Fatalf("StorePasted = %v", err)
	}
	if entries, _ := os.ReadDir(l.PastedDir()); len(entries) != 0 || out.Len() != 0 {
		t.Fatalf("stored %v, printed %q", entries, out.String())
	}
}

var onePixelPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0xda, 0x63, 0xfc, 0xcf, 0xc0, 0x50,
	0x0f, 0x00, 0x04, 0x85, 0x01, 0x80, 0x84, 0xa9, 0x8c, 0x21, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
	0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func TestAStalledUploadDoesNotHoldUpOthers(t *testing.T) {
	l := testLayout(t)
	stalled, feed := io.Pipe()
	defer feed.Close()
	go func() {
		// The first frame arrives; the rest never does, as when the laptop's link dies.
		_ = pastefile.WriteFrame(feed, onePixelPNG)
	}()
	stored := make(chan struct{})
	go func() { _ = StorePasted(l, stalled, firstLine{stored: stored}, time.Now()) }()
	<-stored
	done := make(chan error, 1)
	go func() { done <- StorePasted(l, frames(t, onePixelPNG), io.Discard, time.Now()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled upload held up another store")
	}
}

// firstLine closes stored once the first path is written to it.
type firstLine struct{ stored chan struct{} }

func (f firstLine) Write(p []byte) (int, error) {
	select {
	case <-f.stored:
	default:
		close(f.stored)
	}
	return len(p), nil
}
