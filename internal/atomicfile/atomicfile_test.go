package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteSetsTheModeAndLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("read %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", info.Mode(), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("dir holds %v, %v", entries, err)
	}
}

func TestWriteIntoAMissingDirLeavesNothing(t *testing.T) {
	if err := Write(filepath.Join(t.TempDir(), "no", "f"), []byte("x"), 0o600); err == nil {
		t.Fatal("wrote into a missing dir")
	}
}
