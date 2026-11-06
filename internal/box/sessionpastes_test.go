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

// An agent's change the laptop lacks yet must survive a paste of the same file.
func TestPastingAFileAgainKeepsTheAgentsChangeTheLaptopLacks(t *testing.T) {
	l := testLayout(t)
	first := pasteInto(t, l, pasteSession, [2]string{"/l/a.csv", "v1"})
	if err := os.WriteFile(first[0], []byte("edited by the agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The laptop's file is as it was: the agent's change is the newer one, and goes back.
	if again := pasteInto(t, l, pasteSession, [2]string{"/l/a.csv", "v1"}); again[0] != first[0] {
		t.Fatalf("pasted again to %q", again)
	}
	if got, err := os.ReadFile(first[0]); err != nil || string(got) != "edited by the agent" {
		t.Fatalf("copy holds %q, %v", got, err)
	}
	// Both changed: the paste waits until the agent's change reached the laptop.
	in := exchange(t, []string{"/l/a.csv"}, [2]string{"/l/a.csv", "v2"})
	err := PasteFilesInto(l, pasteSession, in, io.Discard, time.Now())
	if err == nil || !strings.Contains(err.Error(), "not reached the laptop yet") {
		t.Fatalf("PasteFilesInto = %v", err)
	}
	if got, err := os.ReadFile(first[0]); err != nil || string(got) != "edited by the agent" {
		t.Fatalf("copy holds %q, %v", got, err)
	}
	copies, err := sessionCopies(l)
	if err != nil || len(copies) != 1 || copies[0].Synced != sha("v1") {
		t.Fatalf("copies %+v, %v", copies, err)
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
	// The mark made c's view stale; a second mark from it must not land.
	if err := markSynced(l, c, sha("c")); err != nil {
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

func TestABrokenRecordHoldsUpNoOtherCopy(t *testing.T) {
	l := testLayout(t)
	paths := pasteInto(t, l, pasteSession, [2]string{"/l/a.txt", "a"}, [2]string{"/l/b.txt", "b"})
	if err := os.WriteFile(filepath.Dir(paths[0])+".json", []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	copies, err := sessionCopies(l)
	if err == nil || len(copies) != 1 || copies[0].Path != paths[1] {
		t.Fatalf("copies %+v, %v", copies, err)
	}
}

// A paste of a laptop file that follows a box file since a save takes its
// place once the box file's change reached the laptop; the box file stays.
func TestAPasteOfASavedFileTakesThePlaceOfItsBoxFile(t *testing.T) {
	l := testLayout(t)
	path := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(path, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if linked, err := linkSaved(l, pasteSession, "/l/report.md", path, sha("v1")); err != nil || !linked {
		t.Fatalf("linked %v, %v", linked, err)
	}
	var out bytes.Buffer
	if err := PasteFilesInto(l, pasteSession, exchange(t, []string{"/l/report.md"}, [2]string{"/l/report.md", "v1"}), &out, time.Now()); err == nil || !strings.Contains(err.Error(), "has not reached the laptop yet") {
		t.Fatalf("paste over a pending change: %v", err)
	}
	copies, err := sessionCopies(l)
	if err != nil || len(copies) != 1 {
		t.Fatalf("copies %+v, %v", copies, err)
	}
	if err := markSynced(l, copies[0], sha("v2")); err != nil {
		t.Fatal(err)
	}
	pasted := pasteInto(t, l, pasteSession, [2]string{"/l/report.md", "v2"})
	copies, err = sessionCopies(l)
	if err != nil || len(copies) != 1 || copies[0].Linked || copies[0].Path != pasted[0] {
		t.Fatalf("copies %+v, %v; pasted %q", copies, err, pasted)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "v2" {
		t.Fatalf("the box file holds %q, %v", data, err)
	}
}

// Pruning drops an old record of a box file a laptop file follows, never the box file.
func TestPrunePastedLeavesTheBoxFileOfAnOldSave(t *testing.T) {
	l := testLayout(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if linked, err := linkSaved(l, pasteSession, "/l/report.md", path, sha("v1")); err != nil || !linked {
		t.Fatalf("linked %v, %v", linked, err)
	}
	weekAgo := time.Now().Add(-8 * 24 * time.Hour)
	record := filepath.Join(l.SessionPastesDir(), pasteSession, originKey("/l/report.md")+".json")
	for _, p := range []string{path, record} {
		if err := os.Chtimes(p, weekAgo, weekAgo); err != nil {
			t.Fatal(err)
		}
	}
	if err := prunePasted(l, time.Now()); err != nil {
		t.Fatal(err)
	}
	if copies, err := sessionCopies(l); err != nil || len(copies) != 0 {
		t.Fatalf("copies %+v, %v", copies, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "v1" {
		t.Fatalf("the box file holds %q, %v", data, err)
	}
}

// A save over a file pasted into the session, as one pasted since the save,
// leaves its pasted copy the one that goes back.
func TestASaveOverAPastedFileLeavesItsCopyGoingBack(t *testing.T) {
	l := testLayout(t)
	pasted := pasteInto(t, l, pasteSession, [2]string{"/l/notes.md", "laptop"})
	if linked, err := linkSaved(l, pasteSession, "/l/notes.md", "/box/notes.md", sha("agent")); err != nil || linked {
		t.Fatalf("linked %v, %v", linked, err)
	}
	copies, err := sessionCopies(l)
	if err != nil || len(copies) != 1 || copies[0].Linked || copies[0].Path != pasted[0] || copies[0].Synced != sha("laptop") {
		t.Fatalf("copies %+v, %v", copies, err)
	}
}
