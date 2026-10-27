package jsonfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type doc struct {
	A int `json:"a"`
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	var d doc
	if found, err := Read(path, &d); found || err != nil {
		t.Fatalf("missing file: %v, %v", found, err)
	}
	if err := Write(path, doc{A: 3}); err != nil {
		t.Fatal(err)
	}
	if found, err := Read(path, &d); !found || err != nil || d.A != 3 {
		t.Fatalf("read back %+v, %v, %v", d, found, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}
}

func TestReadRefusesUnknownFieldsAndJunk(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"extra.json": `{"a":1,"b":2}`, "junk.json": `{`} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		var d doc
		if _, err := Read(path, &d); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
