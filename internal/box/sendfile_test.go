package box

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"remote-agents/internal/pastefile"
)

func sendFile(t *testing.T, path string) (string, error) {
	t.Helper()
	var in, out bytes.Buffer
	if err := pastefile.WriteFrame(&in, []byte(path)); err != nil {
		t.Fatal(err)
	}
	if err := SendFile(&in, &out); err != nil {
		return "", err
	}
	r := pastefile.NewReader(&out)
	data, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("more after the file: %v", err)
	}
	return string(data), nil
}

func TestSendFileSendsTheFileItIsAskedFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a file.txt")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := sendFile(t, path); err != nil || got != "content" {
		t.Fatalf("sent %q, %v", got, err)
	}
}

func TestSendFileRefusesWhatIsNotARegularFileUnderTheCap(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(pastefile.MaxSize + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for path, want := range map[string]string{
		dir:                        "not a regular file",
		big:                        "over 100 MB",
		filepath.Join(dir, "gone"): "no such file",
		"relative":                 "not absolute",
	} {
		if _, err := sendFile(t, path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("SendFile(%s) = %v, want %q", path, err, want)
		}
	}
}
