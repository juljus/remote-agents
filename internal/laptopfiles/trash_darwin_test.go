package laptopfiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrashMovesAFileToTheTrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-agents-trash-test.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	trashed, err := Trash(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(trashed) })
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s is still there: %v", path, err)
	}
	if data, err := os.ReadFile(trashed); err != nil || string(data) != "x" {
		t.Fatalf("trashed %s holds %q, %v", trashed, data, err)
	}
	if !strings.Contains(trashed, "/.Trash") {
		t.Fatalf("trashed to %s", trashed)
	}
}

func TestTrashOfAMissingFileFails(t *testing.T) {
	if _, err := Trash(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("trashed a missing file")
	}
}
