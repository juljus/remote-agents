package box

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"remote-agents/internal/boxlayout"
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

// firstLine closes stored on the first write to it.
type firstLine struct{ stored chan struct{} }

func (f firstLine) Write(p []byte) (int, error) {
	select {
	case <-f.stored:
	default:
		close(f.stored)
	}
	return len(p), nil
}

// exchange is the laptop's side of a paste-files run: the query, then a name
// and content frame per upload.
func exchange(t *testing.T, query []string, uploads ...[2]string) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	if err := pastefile.WriteQuery(&b, query); err != nil {
		t.Fatal(err)
	}
	for _, u := range uploads {
		if err := pastefile.WriteFrame(&b, []byte(u[0])); err != nil {
			t.Fatal(err)
		}
		if err := pastefile.WriteFrame(&b, []byte(u[1])); err != nil {
			t.Fatal(err)
		}
	}
	return &b
}

// replies reads the box's answer to a query of n paths and the stored paths after it.
func replies(t *testing.T, out *bytes.Buffer, n int) ([]bool, []string) {
	t.Helper()
	r := pastefile.NewReader(out)
	onBox, err := r.Answer(n)
	if err != nil {
		t.Fatal(err)
	}
	var stored []string
	for {
		path, err := r.Next()
		if errors.Is(err, io.EOF) {
			return onBox, stored
		}
		if err != nil {
			t.Fatal(err)
		}
		stored = append(stored, string(path))
	}
}

func pastedPath(l boxlayout.Layout, content, name string) string {
	sum := sha256.Sum256([]byte(content))
	return filepath.Join(l.PastedDir(), hex.EncodeToString(sum[:]), name)
}

func TestPasteFilesAnswersWhichPathsTheBoxHas(t *testing.T) {
	l := testLayout(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "on box.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	query := []string{file, dir, filepath.Join(dir, "missing.pdf"), filepath.Join(file, "under a file")}
	var out bytes.Buffer
	if err := PasteFiles(l, exchange(t, query), &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	onBox, stored := replies(t, &out, len(query))
	if !slices.Equal(onBox, []bool{true, true, false, false}) || len(stored) != 0 {
		t.Fatalf("answer %v, stored %q", onBox, stored)
	}
}

func TestPasteFilesKeepsEachUploadUnderItsNameInAContentHashDir(t *testing.T) {
	l := testLayout(t)
	var out bytes.Buffer
	in := exchange(t, []string{"/laptop/report 1.pdf", "/laptop/data.csv"}, [2]string{"report 1.pdf", "%PDF-1.7"}, [2]string{"data.csv", "a,b\n1,2\n"})
	if err := PasteFiles(l, in, &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, stored := replies(t, &out, 2)
	want := []string{pastedPath(l, "%PDF-1.7", "report 1.pdf"), pastedPath(l, "a,b\n1,2\n", "data.csv")}
	if !slices.Equal(stored, want) {
		t.Fatalf("stored %q, want %q", stored, want)
	}
	if got, err := os.ReadFile(want[1]); err != nil || string(got) != "a,b\n1,2\n" {
		t.Fatalf("stored content %q, %v", got, err)
	}
}

func TestPasteFilesReusesKnownContentAndPrunesOldPastesOfBothLayouts(t *testing.T) {
	l := testLayout(t)
	var out bytes.Buffer
	if err := PasteFiles(l, exchange(t, []string{"/l/a.txt"}, [2]string{"a.txt", "same"}), &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, first := replies(t, &out, 1)
	contentDir := filepath.Dir(first[0])
	oldDir := filepath.Join(l.PastedDir(), "0ld")
	oldFlat := filepath.Join(l.PastedDir(), "0ld.png")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(oldDir, "x.txt"), oldFlat} {
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	weekAgo := time.Now().Add(-8 * 24 * time.Hour)
	for _, path := range []string{contentDir, oldDir, oldFlat} {
		if err := os.Chtimes(path, weekAgo, weekAgo); err != nil {
			t.Fatal(err)
		}
	}
	out.Reset()
	in := exchange(t, []string{"/l/a.txt", "/l/b.txt"}, [2]string{"a.txt", "same"}, [2]string{"b.txt", "same"})
	if err := PasteFiles(l, in, &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, again := replies(t, &out, 2)
	if want := []string{first[0], filepath.Join(contentDir, "b.txt")}; !slices.Equal(again, want) {
		t.Fatalf("second store gave %q, want %q", again, want)
	}
	if info, err := os.Stat(contentDir); err != nil || time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("pasting it again did not keep %s fresh: %v", contentDir, err)
	}
	for _, path := range []string{oldDir, oldFlat} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("old paste %s survived: %v", path, err)
		}
	}
}

func TestPasteFilesRefusesNamesThatAreNotOneFileName(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../escape", "a/b", "/abs"} {
		l := testLayout(t)
		err := PasteFiles(l, exchange(t, []string{"/l/x"}, [2]string{name, "data"}), io.Discard, time.Now())
		if err == nil {
			t.Errorf("name %q was stored", name)
		}
		if entries, err := os.ReadDir(l.PastedDir()); err != nil || len(entries) != 0 {
			t.Errorf("name %q left %v, %v", name, entries, err)
		}
	}
}

func TestPasteFilesRefusesARelativeQueryPath(t *testing.T) {
	if err := PasteFiles(testLayout(t), exchange(t, []string{"relative/path"}), io.Discard, time.Now()); err == nil {
		t.Fatal("a relative path was looked up on the box")
	}
}

func TestPasteFilesLeavesNothingOfATruncatedUpload(t *testing.T) {
	l := testLayout(t)
	in := exchange(t, []string{"/l/x"})
	in.WriteString("1\nx\n10\nshort")
	if err := PasteFiles(l, in, io.Discard, time.Now()); err == nil {
		t.Fatal("a truncated upload was stored")
	}
	if entries, err := os.ReadDir(l.PastedDir()); err != nil || len(entries) != 0 {
		t.Fatalf("left %v, %v", entries, err)
	}
}

func TestPastingContentAgainRestoresACopyTheAgentChanged(t *testing.T) {
	l := testLayout(t)
	var out bytes.Buffer
	if err := PasteFiles(l, exchange(t, []string{"/l/a.csv"}, [2]string{"a.csv", "original"}), &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, stored := replies(t, &out, 1)
	if err := os.WriteFile(stored[0], []byte("edited on the box"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PasteFiles(l, exchange(t, []string{"/l/a.csv"}, [2]string{"a.csv", "original"}), io.Discard, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(stored[0]); err != nil || string(got) != "original" {
		t.Fatalf("copy holds %q, %v", got, err)
	}
}

func TestPasteFilesAnswersBeforeWaitingOnThePastedLock(t *testing.T) {
	l := testLayout(t)
	if err := os.MkdirAll(filepath.Dir(l.PastedLock()), 0o700); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = withFlock(l.PastedLock(), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)
	answered := make(chan struct{})
	go func() { _ = PasteFiles(l, exchange(t, []string{"/l/x"}), firstLine{stored: answered}, time.Now()) }()
	select {
	case <-answered:
	case <-time.After(2 * time.Second):
		t.Fatal("the answer waited on the pasted lock")
	}
}

func TestAStalledPasteFilesUploadDoesNotHoldUpOthers(t *testing.T) {
	l := testLayout(t)
	stalled, feed := io.Pipe()
	defer feed.Close()
	go func() {
		// The upload starts; the rest never arrives, as when the laptop's link dies.
		_ = pastefile.WriteQuery(feed, []string{"/l/big"})
		_ = pastefile.WriteFrame(feed, []byte("big"))
		_, _ = feed.Write([]byte("100\npart"))
	}()
	answered := make(chan struct{})
	go func() { _ = PasteFiles(l, stalled, firstLine{stored: answered}, time.Now()) }()
	<-answered
	done := make(chan error, 1)
	go func() {
		done <- PasteFiles(l, exchange(t, []string{"/l/a"}, [2]string{"a", "data"}), io.Discard, time.Now())
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled upload held up another store")
	}
}
