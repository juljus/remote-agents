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
)

const (
	pasteSession = "claude-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	otherSession = "codex-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

// pasteInto runs a paste-files exchange for session, uploading each origin
// with its content, and returns where they landed.
func pasteInto(t *testing.T, l boxlayout.Layout, session string, uploads ...[2]string) []string {
	t.Helper()
	var query []string
	for _, u := range uploads {
		query = append(query, u[0])
	}
	var out bytes.Buffer
	if err := PasteFilesInto(l, session, exchange(t, query, uploads...), &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, stored := replies(t, &out, len(uploads))
	return stored
}

func TestPasteFilesGivesEachSessionItsOwnCopy(t *testing.T) {
	l := testLayout(t)
	origin := "/Users/me/docs/notes 1.md"
	a := pasteInto(t, l, pasteSession, [2]string{origin, "laptop"})
	b := pasteInto(t, l, otherSession, [2]string{origin, "laptop"})
	if len(a) != 1 || len(b) != 1 || a[0] == b[0] {
		t.Fatalf("sessions got %q and %q", a, b)
	}
	for _, path := range []string{a[0], b[0]} {
		if filepath.Base(path) != "notes 1.md" || !strings.HasPrefix(path, l.SessionPastesDir()+"/") {
			t.Fatalf("copy at %s", path)
		}
	}
	if err := os.WriteFile(a[0], []byte("edited by the agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{b[0], pastedPath(l, "laptop", "notes 1.md")} {
		if got, err := os.ReadFile(path); err != nil || string(got) != "laptop" {
			t.Fatalf("%s holds %q, %v after an edit of the other session's copy", path, got, err)
		}
	}

	copies, err := sessionCopies(l)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(copies, func(x, y pastedCopy) int { return strings.Compare(x.Session, y.Session) })
	want := []pastedCopy{
		{Session: pasteSession, Origin: origin, Path: a[0], Synced: sha("laptop")},
		{Session: otherSession, Origin: origin, Path: b[0], Synced: sha("laptop")},
	}
	if !slices.EqualFunc(copies, want, func(x, y pastedCopy) bool {
		return x.Session == y.Session && x.Origin == y.Origin && x.Path == y.Path && x.Synced == y.Synced
	}) {
		t.Fatalf("copies %+v, want %+v", copies, want)
	}
}

func TestPastingAFileAgainIntoTheSessionReplacesItsCopy(t *testing.T) {
	l := testLayout(t)
	first := pasteInto(t, l, pasteSession, [2]string{"/l/a.csv", "v1"})
	if err := os.WriteFile(first[0], []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	again := pasteInto(t, l, pasteSession, [2]string{"/l/a.csv", "v2"}, [2]string{"/l/other/a.csv", "other"})
	if again[0] != first[0] || again[1] == first[0] {
		t.Fatalf("pasted again to %q, first to %q", again, first)
	}
	if got, err := os.ReadFile(first[0]); err != nil || string(got) != "v2" {
		t.Fatalf("copy holds %q, %v", got, err)
	}
	copies, err := sessionCopies(l)
	if err != nil || len(copies) != 2 {
		t.Fatalf("copies %+v, %v", copies, err)
	}
	for _, c := range copies {
		if c.Path == first[0] && c.Synced != sha("v2") {
			t.Fatalf("synced %s, want v2's", c.Synced)
		}
	}
}

func TestPasteFilesIntoASessionRefusesWhatIsNotALaptopPath(t *testing.T) {
	for _, origin := range []string{"", "a.txt", "/l/../x", "/l/", "/"} {
		l := testLayout(t)
		in := exchange(t, []string{"/l/q"}, [2]string{origin, "data"})
		if err := PasteFilesInto(l, pasteSession, in, io.Discard, time.Now()); err == nil {
			t.Errorf("origin %q was stored", origin)
		}
		if _, err := os.Stat(l.SessionPastesDir()); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("origin %q left a copy: %v", origin, err)
		}
	}
	if err := PasteFilesInto(testLayout(t), "../x", exchange(t, []string{"/l/q"}, [2]string{"/l/q", "d"}), io.Discard, time.Now()); err == nil {
		t.Error("an invalid session name was used")
	}
}

func TestPrunePastedKeepsOldCopiesTheLaptopLacks(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/synced.txt", "s"}, [2]string{"/l/edited.txt", "e"}, [2]string{"/l/fresh.txt", "f"})
	if err := os.WriteFile(paths[1], []byte("not on the laptop yet"), 0o600); err != nil {
		t.Fatal(err)
	}
	weekAgo := time.Now().Add(-8 * 24 * time.Hour)
	for _, path := range paths[:2] {
		for _, p := range []string{path, filepath.Dir(path) + ".json"} {
			if err := os.Chtimes(p, weekAgo, weekAgo); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := prunePasted(l, time.Now()); err != nil {
		t.Fatal(err)
	}
	for i, wantKept := range []bool{false, true, true} {
		_, err := os.Stat(paths[i])
		if kept := err == nil; kept != wantKept {
			t.Errorf("%s kept %v, want %v (%v)", paths[i], kept, wantKept, err)
		}
	}
	copies, err := sessionCopies(l)
	if err != nil || len(copies) != 2 {
		t.Fatalf("copies %+v, %v", copies, err)
	}
}

func TestPrunePastedRemovesASessionsDirOnceItsLastCopyGoes(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/a.txt", "a"})
	weekAgo := time.Now().Add(-8 * 24 * time.Hour)
	for _, p := range []string{paths[0], filepath.Dir(paths[0]) + ".json"} {
		if err := os.Chtimes(p, weekAgo, weekAgo); err != nil {
			t.Fatal(err)
		}
	}
	if err := prunePasted(l, time.Now()); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(l.SessionPastesDir()); err != nil || len(entries) != 0 {
		t.Fatalf("left %v, %v", entries, err)
	}
}

func TestMarkSyncedOnlyMovesFromWhatTheSenderSaw(t *testing.T) {
	l := testLayout(t)
	pasteInto(t, l, pasteSession, [2]string{"/l/a.txt", "a"})
	copies, err := sessionCopies(l)
	if err != nil {
		t.Fatal(err)
	}
	c := copies[0]
	if err := markSynced(l, c, sha("b")); err != nil {
		t.Fatal(err)
	}
	// A paste in between makes the sender's view stale; its mark must not land.
	stale := c
	stale.Synced = sha("a")
	if err := markSynced(l, stale, sha("c")); err != nil {
		t.Fatal(err)
	}
	if copies, err = sessionCopies(l); err != nil || copies[0].Synced != sha("b") {
		t.Fatalf("copies %+v, %v", copies, err)
	}
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
