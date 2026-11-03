package claudeauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Laid out as JavaScript's JSON.stringify(v, null, 2) lays it out.
const indented = `{
  "numStartups": 12,
  "tips": {
    "a<b": 1.50,
    "empty": {},
    "list": []
  },
  "oauthAccount": {
    "accountUuid": "old"
  },
  "text": "é & <x>"
}`

func TestObjectRewritesOnlyTheKeySet(t *testing.T) {
	o, err := parseObject([]byte(indented))
	if err != nil {
		t.Fatal(err)
	}
	same, err := o.marshal("  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(same) != indented {
		t.Fatalf("round trip changed the file:\n%s", same)
	}
	changed, err := o.with("oauthAccount", json.RawMessage(`{"accountUuid":"new"}`)).marshal("  ")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "numStartups": 12,
  "tips": {
    "a<b": 1.50,
    "empty": {},
    "list": []
  },
  "oauthAccount": {
    "accountUuid": "new"
  },
  "text": "é & <x>"
}`
	if string(changed) != want {
		t.Fatalf("got:\n%s", changed)
	}
	compact, err := o.with("added", json.RawMessage(`true`)).marshal("")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"numStartups":12,"tips":{"a<b":1.50,"empty":{},"list":[]},"oauthAccount":{"accountUuid":"old"},"text":"é & <x>","added":true}`; string(compact) != want {
		t.Fatalf("compact:\n%s", compact)
	}
}

func TestParseObjectRefusesWhatIsNotOneObject(t *testing.T) {
	for _, bad := range []string{`[]`, `{"a":1}{}`, `{"a":1,"a":2}`, `{"a":}`, ``} {
		if _, err := parseObject([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestWriteFileAtomicSetsTheModeAndLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new"), 0o600); err != nil {
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
