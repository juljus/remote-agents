package laptopfiles

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const session = "claude-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

type fixture struct {
	files    Files
	home     string
	trashed  []string
	trash    string
	approver *answers
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{home: filepath.Join(root, "home"), trash: filepath.Join(root, "trash"), approver: &answers{}}
	for _, dir := range []string{f.home, filepath.Join(f.home, "Downloads"), filepath.Join(f.home, "docs"), f.trash} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.files = Files{
		Records: Records{Dir: filepath.Join(root, "state", "pasted")},
		Home:    f.home,
		Trash: func(path string) error {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			f.trashed = append(f.trashed, filepath.Base(path)+"="+string(data))
			return os.Rename(path, filepath.Join(f.trash, filepath.Base(path)))
		},
		Gate: NewGate(f.approver, time.Minute),
	}
	return f
}

func sum(content string) string {
	s := sha256.Sum256([]byte(content))
	return hex.EncodeToString(s[:])
}

func content(c string) Fetch {
	return func(w io.Writer) error {
		_, err := io.WriteString(w, c)
		return err
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// pasted makes path a file pasted into the session with what it holds now.
func (f *fixture) pasted(t *testing.T, path, c string) {
	t.Helper()
	write(t, path, c)
	if err := f.files.Records.Pasted(session, path, sum(c)); err != nil {
		t.Fatal(err)
	}
}

// onlyFiles lists dir's entries, failing on leftovers such as temporary files.
func onlyFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s holds %q, want %q", dir, got, want)
	}
}

func TestWriteBackReplacesAnUnchangedOriginAndTrashesItsOldVersion(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, "docs", "notes.md")
	f.pasted(t, origin, "laptop")

	res, err := f.files.WriteBack(t.Context(), session, origin, content("agent"))
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{Outcome: Written, Path: origin, Sum: sum("agent")}) {
		t.Fatalf("result %+v", res)
	}
	if got := read(t, origin); got != "agent" {
		t.Fatalf("origin holds %q", got)
	}
	info, err := os.Stat(origin)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("origin mode %v, %v; want the old file's", info.Mode(), err)
	}
	if !slices.Equal(f.trashed, []string{"notes.md=laptop"}) {
		t.Fatalf("trashed %q", f.trashed)
	}
	onlyFiles(t, filepath.Join(f.home, "docs"), "notes.md")

	// The written version is the new base: the next save replaces it too.
	if res, err := f.files.WriteBack(t.Context(), session, origin, content("agent 2")); err != nil || res.Outcome != Written {
		t.Fatalf("second write-back %+v, %v", res, err)
	}
	if got := read(t, origin); got != "agent 2" {
		t.Fatalf("origin holds %q", got)
	}
}

func TestWriteBackOfWhatTheLaptopHasChangesNothing(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, "docs", "notes.md")
	f.pasted(t, origin, "laptop")
	write(t, origin, "same")

	res, err := f.files.WriteBack(t.Context(), session, origin, content("same"))
	if err != nil || res.Outcome != Unchanged {
		t.Fatalf("result %+v, %v", res, err)
	}
	if len(f.trashed) != 0 {
		t.Fatalf("trashed %q", f.trashed)
	}
	onlyFiles(t, filepath.Join(f.home, "docs"), "notes.md")
	// The laptop's content is the base now, so the next save replaces it.
	if res, err := f.files.WriteBack(t.Context(), session, origin, content("agent")); err != nil || res.Outcome != Written {
		t.Fatalf("next write-back %+v, %v", res, err)
	}
}

func TestWriteBackOverALaptopChangeGoesBesideIt(t *testing.T) {
	f := newFixture(t)
	docs := filepath.Join(f.home, "docs")
	origin := filepath.Join(docs, "notes.md")
	f.pasted(t, origin, "laptop")
	write(t, origin, "edited on the laptop")

	res, err := f.files.WriteBack(t.Context(), session, origin, content("agent"))
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(docs, "notes (box).md")
	if res != (Result{Outcome: Conflict, Path: copyPath, Sum: sum("agent")}) {
		t.Fatalf("result %+v", res)
	}
	if got := read(t, origin); got != "edited on the laptop" {
		t.Fatalf("origin holds %q", got)
	}
	if got := read(t, copyPath); got != "agent" {
		t.Fatalf("copy holds %q", got)
	}

	// The next save refreshes the copy it wrote, without trashing it.
	if _, err := f.files.WriteBack(t.Context(), session, origin, content("agent 2")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, copyPath); got != "agent 2" || len(f.trashed) != 0 {
		t.Fatalf("copy holds %q, trashed %q", got, f.trashed)
	}

	// A copy the user changed goes to the Trash before the next one lands.
	write(t, copyPath, "user's")
	if _, err := f.files.WriteBack(t.Context(), session, origin, content("agent 3")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, copyPath); got != "agent 3" || !slices.Equal(f.trashed, []string{"notes (box).md=user's"}) {
		t.Fatalf("copy holds %q, trashed %q", got, f.trashed)
	}
	onlyFiles(t, docs, "notes.md", "notes (box).md")
}

func TestWriteBackOfAnOriginGoneFromTheLaptopGoesBesideIt(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, "docs", "notes.md")
	f.pasted(t, origin, "laptop")
	if err := os.Remove(origin); err != nil {
		t.Fatal(err)
	}
	res, err := f.files.WriteBack(t.Context(), session, origin, content("agent"))
	if err != nil || res.Outcome != Conflict {
		t.Fatalf("result %+v, %v", res, err)
	}
	onlyFiles(t, filepath.Join(f.home, "docs"), "notes (box).md")
}

func TestConflictCopyNames(t *testing.T) {
	for name, want := range map[string]string{
		"notes.md":    "notes (box).md",
		"a.tar.gz":    "a.tar (box).gz",
		"Makefile":    "Makefile (box)",
		".zshrc":      ".zshrc (box)",
		"with space.": "with space (box).",
	} {
		if got := conflictName(name); got != want {
			t.Errorf("conflictName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestWriteBackRefusesWhatWasNotPastedIntoTheSession(t *testing.T) {
	f := newFixture(t)
	docs := filepath.Join(f.home, "docs")
	origin := filepath.Join(docs, "notes.md")
	f.pasted(t, origin, "laptop")
	other := filepath.Join(docs, "other.md")
	write(t, other, "untouched")
	link := filepath.Join(docs, "link.md")
	if err := os.Symlink(origin, link); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct{ session, path string }{
		"another file":            {session, other},
		"another session":         {"codex-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", origin},
		"a symlink to the origin": {session, link},
		"a path through ..":       {session, docs + "/../docs/notes.md"},
		"a relative path":         {session, "docs/notes.md"},
		"an invalid session":      {"../x", origin},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.files.WriteBack(t.Context(), tc.session, tc.path, content("agent"))
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
			}
		})
	}
	if read(t, origin) != "laptop" || read(t, other) != "untouched" || len(f.trashed) != 0 {
		t.Fatal("a refused write-back changed a file")
	}
	onlyFiles(t, docs, "link.md", "notes.md", "other.md")
}

func TestAFailedFetchLeavesNothingBehind(t *testing.T) {
	f := newFixture(t)
	docs := filepath.Join(f.home, "docs")
	origin := filepath.Join(docs, "notes.md")
	f.pasted(t, origin, "laptop")
	boom := errors.New("link dropped")
	fetch := func(w io.Writer) error {
		_, _ = io.WriteString(w, "half")
		return boom
	}
	if _, err := f.files.WriteBack(t.Context(), session, origin, fetch); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.files.Save(t.Context(), session, "x.txt", "", fetch); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	onlyFiles(t, docs, "notes.md")
	onlyFiles(t, filepath.Join(f.home, "Downloads"))
}

func TestSaveGoesToDownloadsUnderAFreeName(t *testing.T) {
	f := newFixture(t)
	downloads := filepath.Join(f.home, "Downloads")
	write(t, filepath.Join(downloads, "report.pdf"), "older")

	for _, want := range []string{"report (1).pdf", "report (2).pdf"} {
		saved, err := f.files.Save(t.Context(), session, "report.pdf", "", content("new"))
		path := saved.Path
		if err != nil {
			t.Fatal(err)
		}
		if path != filepath.Join(downloads, want) || read(t, path) != "new" {
			t.Fatalf("saved to %s", path)
		}
	}
	if read(t, filepath.Join(downloads, "report.pdf")) != "older" || len(f.trashed) != 0 {
		t.Fatal("save overwrote or trashed a file")
	}
	onlyFiles(t, downloads, "report.pdf", "report (1).pdf", "report (2).pdf")
}

func TestSaveToAPathInDownloads(t *testing.T) {
	f := newFixture(t)
	downloads := filepath.Join(f.home, "Downloads")
	if err := os.Mkdir(filepath.Join(downloads, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ dest, want string }{
		{"renamed.txt", filepath.Join(downloads, "renamed.txt")},
		{"sub", filepath.Join(downloads, "sub", "x.txt")},
		{downloads + "/sub/", filepath.Join(downloads, "sub", "x (1).txt")},
		{"~/Downloads/sub/y.txt", filepath.Join(downloads, "sub", "y.txt")},
		{filepath.Join(downloads, "z.txt"), filepath.Join(downloads, "z.txt")},
		{"~/Downloads", filepath.Join(downloads, "x.txt")},
	} {
		saved, err := f.files.Save(t.Context(), session, "x.txt", tc.dest, content("c"))
		path := saved.Path
		if err != nil {
			t.Fatalf("Save to %q: %v", tc.dest, err)
		}
		if path != tc.want {
			t.Errorf("Save to %q went to %s, want %s", tc.dest, path, tc.want)
		}
	}
}

func TestSaveOverAPastedOriginTrashesTheOldVersion(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, "docs", "notes.md")
	f.pasted(t, origin, "laptop")
	write(t, origin, "edited on the laptop")

	saved, err := f.files.Save(t.Context(), session, "whatever.md", origin, content("agent"))
	path := saved.Path
	if err != nil {
		t.Fatal(err)
	}
	if path != origin || read(t, origin) != "agent" {
		t.Fatalf("saved to %s, origin holds %q", path, read(t, origin))
	}
	if !slices.Equal(f.trashed, []string{"notes.md=edited on the laptop"}) {
		t.Fatalf("trashed %q", f.trashed)
	}
	// It is the new base, so a write-back of the next save replaces it.
	if res, err := f.files.WriteBack(t.Context(), session, origin, content("agent 2")); err != nil || res.Outcome != Written {
		t.Fatalf("write-back %+v, %v", res, err)
	}
}

func TestRecordsKeepEachSessionsPastes(t *testing.T) {
	f := newFixture(t)
	a := filepath.Join(f.home, "docs", "a.md")
	f.pasted(t, a, "a")
	b := filepath.Join(f.home, "docs", "b.md")
	f.pasted(t, b, "b")
	// Pasting again makes what the laptop has then the base.
	f.pasted(t, a, "a2")

	for path, c := range map[string]string{a: "a2", b: "b"} {
		res, err := f.files.WriteBack(t.Context(), session, path, content("agent"))
		if err != nil || res.Outcome != Written {
			t.Fatalf("%s: %+v, %v; want the base to be %q", path, res, err, c)
		}
	}
	if err := f.files.Records.Pasted(session, "docs/a.md", sum("a")); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("recorded a relative origin: %v", err)
	}
}

// Without a Trash, as on some network volumes, the write-back still lands and
// says where the old version stays.
func TestAWriteBackWhoseOldVersionCannotGoToTheTrashSaysWhereItStays(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, "docs", "notes.md")
	f.pasted(t, origin, "laptop")
	f.files.Trash = func(string) error { return errors.New("the volume has no Trash") }

	res, err := f.files.WriteBack(t.Context(), session, origin, content("agent"))
	if err != nil || res.Outcome != Written || read(t, origin) != "agent" {
		t.Fatalf("result %+v, %v; origin holds %q", res, err, read(t, origin))
	}
	if res.Leftover == "" || read(t, res.Leftover) != "laptop" || !strings.Contains(res.LeftoverErr.Error(), "no Trash") {
		t.Fatalf("old version kept at %q (%v)", res.Leftover, res.LeftoverErr)
	}
	// The record moved on, so the next save is no conflict.
	if res, err := f.files.WriteBack(t.Context(), session, origin, content("agent 2")); err != nil || res.Outcome != Written {
		t.Fatalf("next write-back %+v, %v", res, err)
	}
}

func TestAConflictCopyThatIsASymlinkGoesToTheTrashNotItsTarget(t *testing.T) {
	f := newFixture(t)
	docs := filepath.Join(f.home, "docs")
	origin := filepath.Join(docs, "notes.md")
	f.pasted(t, origin, "laptop")
	write(t, origin, "edited on the laptop")
	target := filepath.Join(f.home, "target.txt")
	write(t, target, "untouched")
	if err := os.Symlink(target, filepath.Join(docs, "notes (box).md")); err != nil {
		t.Fatal(err)
	}
	res, err := f.files.WriteBack(t.Context(), session, origin, content("agent"))
	if err != nil || res.Outcome != Conflict || read(t, res.Path) != "agent" {
		t.Fatalf("result %+v, %v", res, err)
	}
	if read(t, target) != "untouched" {
		t.Fatal("the write went through the symlink")
	}
	if info, err := os.Lstat(res.Path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("copy is %v, %v", info.Mode(), err)
	}
}

// A trailing slash says the path is a dir, so it is never the pasted file.
func TestSaveToAPastedFileAsADirIsRefused(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, "docs", "notes.md")
	f.pasted(t, origin, "laptop")
	if _, err := f.files.Save(t.Context(), session, "x.md", origin+"/", content("agent")); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v", err)
	}
	if read(t, origin) != "laptop" {
		t.Fatal("the pasted file changed")
	}
}

func TestALongNameCanBeWrittenBack(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, "docs", strings.Repeat("n", 240)+".md")
	f.pasted(t, origin, "laptop")
	if res, err := f.files.WriteBack(t.Context(), session, origin, content("agent")); err != nil || res.Outcome != Written {
		t.Fatalf("result %+v, %v", res, err)
	}
}

func TestRecordsOfSessionsUntouchedForTwoWeeksGo(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, "docs", "notes.md")
	f.pasted(t, origin, "laptop")
	old := time.Now().Add(-15 * 24 * time.Hour)
	if err := os.Chtimes(f.files.Records.path(session), old, old); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.home, "docs", "other.md")
	write(t, other, "o")
	if err := f.files.Records.Pasted("codex-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", other, sum("o")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.files.WriteBack(t.Context(), session, origin, content("agent")); !errors.Is(err, ErrRefused) {
		t.Fatalf("a pruned session's paste was written back: %v", err)
	}
}

func TestConcurrentWriteBacksOfOneFileLeaveOneOfThem(t *testing.T) {
	f := newFixture(t)
	docs := filepath.Join(f.home, "docs")
	origin := filepath.Join(docs, "notes.md")
	f.pasted(t, origin, "laptop")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, c := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.files.WriteBack(t.Context(), session, origin, content(c))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// The second to commit saw the first's write as the base, so it replaced it too.
	if got := read(t, origin); got != "one" && got != "two" {
		t.Fatalf("origin holds %q", got)
	}
	onlyFiles(t, docs, "notes.md")
}

// A dir that is gone may come back, as an ejected volume does; that is no refusal.
func TestAWriteBackIntoAMissingDirFailsWithoutRefusing(t *testing.T) {
	f := newFixture(t)
	docs := filepath.Join(f.home, "docs")
	origin := filepath.Join(docs, "notes.md")
	f.pasted(t, origin, "laptop")
	if err := os.RemoveAll(docs); err != nil {
		t.Fatal(err)
	}
	_, err := f.files.WriteBack(t.Context(), session, origin, content("agent"))
	if err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want a failure that is no refusal", err)
	}
}

// APFS ignores case, and the box picks the spelling of a path it plants.
func TestAPathTypedInAnotherCaseIsRecordedAsTheDiskSpellsIt(t *testing.T) {
	f := newFixture(t)
	library := filepath.Join(f.home, "Library", "LaunchAgents")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(library, "com.x.plist")
	write(t, plist, "laptop")
	typed := filepath.Join(f.home, "LIBRARY", "launchagents", "COM.X.PLIST")
	if _, err := os.Stat(typed); err != nil {
		t.Skipf("the temp dir's volume is case-sensitive: %v", err)
	}
	origin, err := Origin(typed)
	if err != nil {
		t.Fatal(err)
	}
	if origin != plist {
		t.Fatalf("Origin(%s) = %s, want %s", typed, origin, plist)
	}
	if err := f.files.Records.Pasted(session, origin, sum("laptop")); err != nil {
		t.Fatal(err)
	}
	declined := errors.New("Canceled by user.")
	f.approver.next = []error{declined, declined}
	if _, err := f.files.WriteBack(t.Context(), session, origin, content("agent")); !errors.Is(err, ErrRefused) {
		t.Fatalf("write-back under ~/Library: %v", err)
	}
	// Recorded as typed, as an older wrapper would, the check still holds.
	if err := f.files.Records.Pasted(session, filepath.Join(f.home, "library", "LaunchAgents", "com.x.plist"), sum("laptop")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.files.WriteBack(t.Context(), session, filepath.Join(f.home, "library", "LaunchAgents", "com.x.plist"), content("agent")); !errors.Is(err, ErrRefused) {
		t.Fatalf("write-back under ~/library: %v", err)
	}
	if read(t, plist) != "laptop" {
		t.Fatal("the plist changed")
	}
	for _, reason := range f.approver.asked() {
		if !strings.Contains(reason, "under ~/Library") {
			t.Fatalf("asked %q, which does not say it is under ~/Library", reason)
		}
	}
}

func TestAnOrdinaryWriteBackAsksNobody(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, "docs", "notes.md")
	f.pasted(t, origin, "laptop")
	if res, err := f.files.WriteBack(t.Context(), session, origin, content("agent")); err != nil || res.Outcome != Written {
		t.Fatalf("result %+v, %v", res, err)
	}
	write(t, origin, "edited on the laptop")
	if res, err := f.files.WriteBack(t.Context(), session, origin, content("agent 2")); err != nil || res.Outcome != Conflict {
		t.Fatalf("result %+v, %v", res, err)
	}
	if got := f.approver.asked(); len(got) != 0 {
		t.Fatalf("asked %q", got)
	}
}

// A box that puts ~/.zshrc on the clipboard for the user to paste gets to
// write it only once the user approves; then for the rest of the session.
func TestAHiddenFileIsWrittenBackOnceTheUserApproves(t *testing.T) {
	f := newFixture(t)
	hiddenDir := filepath.Join(f.home, ".config", "app")
	if err := os.MkdirAll(hiddenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{filepath.Join(f.home, ".zshrc"), filepath.Join(hiddenDir, "config.toml")} {
		t.Run(filepath.Base(origin), func(t *testing.T) {
			f.pasted(t, origin, "laptop")
			before := len(f.approver.asked())
			f.approver.next = []error{errors.New("Canceled by user.")}
			if _, err := f.files.WriteBack(t.Context(), session, origin, content("agent")); !errors.Is(err, ErrRefused) {
				t.Fatalf("declined write-back: %v", err)
			}
			if read(t, origin) != "laptop" {
				t.Fatal("a declined write-back changed the file")
			}
			for _, c := range []string{"agent", "agent 2"} {
				if res, err := f.files.WriteBack(t.Context(), session, origin, content(c)); err != nil || res.Outcome != Written || read(t, origin) != c {
					t.Fatalf("approved write-back %+v, %v", res, err)
				}
			}
			asked := f.approver.asked()[before:]
			if len(asked) != 2 {
				t.Fatalf("asked %q, want the declined prompt and one more", asked)
			}
			want := "write back the edits of box session " + session + " to " + strconv.Quote(origin) + " (hidden): replace it now, the old version to the Trash"
			if asked[1] != want {
				t.Fatalf("asked %q, want %q", asked[1], want)
			}
		})
	}
}

// A script, a launch agent or a file outside home may run as code once
// written, so writing back over one waits for the user.
func TestRiskyWriteBacksAskTheUserFirst(t *testing.T) {
	f := newFixture(t)
	library := filepath.Join(f.home, "Library", "LaunchAgents")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(f.home), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(f.home, "docs", "run.sh")
	for name, tc := range map[string]struct{ origin, risk string }{
		"under ~/Library":    {filepath.Join(library, "agent.plist"), "under ~/Library"},
		"outside home":       {filepath.Join(outside, "notes.md"), "outside your home"},
		"an executable file": {script, "executable"},
	} {
		t.Run(name, func(t *testing.T) {
			f.pasted(t, tc.origin, "laptop")
			if tc.origin == script {
				if err := os.Chmod(script, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			f.approver.next = []error{errors.New("Canceled by user.")}
			if _, err := f.files.WriteBack(t.Context(), session, tc.origin, content("agent")); !errors.Is(err, ErrRefused) {
				t.Fatalf("declined write-back: %v", err)
			}
			if read(t, tc.origin) != "laptop" {
				t.Fatal("it changed")
			}
			onlyFiles(t, filepath.Dir(tc.origin), filepath.Base(tc.origin))
			if res, err := f.files.WriteBack(t.Context(), session, tc.origin, content("agent")); err != nil || res.Outcome != Written {
				t.Fatalf("approved write-back %+v, %v", res, err)
			}
			asked := f.approver.asked()
			if last := asked[len(asked)-1]; !strings.Contains(last, tc.risk) || !strings.Contains(last, tc.origin) {
				t.Fatalf("asked %q, want it to name %s and say %q", last, tc.origin, tc.risk)
			}
		})
	}
	if info, err := os.Stat(script); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("script mode %v, %v; want it kept", info.Mode(), err)
	}
}

func TestAnApprovedWriteBackHoldsForThatSessionOnly(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, ".zshrc")
	f.pasted(t, origin, "laptop")
	if err := f.files.Records.Pasted(otherSession, origin, sum("laptop")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.files.WriteBack(t.Context(), session, origin, content("agent")); err != nil {
		t.Fatal(err)
	}
	f.approver.next = []error{errors.New("Canceled by user.")}
	if _, err := f.files.WriteBack(t.Context(), otherSession, origin, content("other")); !errors.Is(err, ErrRefused) {
		t.Fatalf("another session wrote with the first one's approval: %v", err)
	}
	if read(t, origin) != "agent" || len(f.approver.asked()) != 2 {
		t.Fatalf("origin holds %q, asked %q", read(t, origin), f.approver.asked())
	}
}

// What the user approved is where the write lands: a dir swapped for a
// symlink between the prompt and the write leads elsewhere, so it is refused.
func TestAWriteBackWhoseTargetMovedAfterTheApprovalIsRefused(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.home, ".config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(dir, "app.toml")
	f.pasted(t, origin, "laptop")
	elsewhere := filepath.Join(f.home, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(elsewhere, "app.toml"), "laptop")
	swap := func(w io.Writer) error {
		if err := os.Rename(dir, filepath.Join(f.home, "moved")); err != nil {
			return err
		}
		if err := os.Symlink(elsewhere, dir); err != nil {
			return err
		}
		_, err := io.WriteString(w, "agent")
		return err
	}
	if _, err := f.files.WriteBack(t.Context(), session, origin, swap); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if read(t, filepath.Join(elsewhere, "app.toml")) != "laptop" || read(t, filepath.Join(f.home, "moved", "app.toml")) != "laptop" {
		t.Fatal("the write landed")
	}
	onlyFiles(t, elsewhere, "app.toml")
}

// A pasted file's dir that now leads elsewhere is written only once the user
// approves the place it leads to.
func TestAWriteBackThroughADirNowASymlinkAsksWhereItLands(t *testing.T) {
	f := newFixture(t)
	docs := filepath.Join(f.home, "docs")
	origin := filepath.Join(docs, "notes.md")
	f.pasted(t, origin, "laptop")
	if err := os.Rename(docs, filepath.Join(f.home, "moved")); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(f.home, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, docs); err != nil {
		t.Fatal(err)
	}
	f.approver.next = []error{errors.New("Canceled by user.")}
	if _, err := f.files.WriteBack(t.Context(), session, origin, content("agent")); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	onlyFiles(t, elsewhere)
	res, err := f.files.WriteBack(t.Context(), session, origin, content("agent"))
	if err != nil || res.Outcome != Conflict || res.Path != filepath.Join(elsewhere, "notes (box).md") {
		t.Fatalf("result %+v, %v", res, err)
	}
	asked := f.approver.asked()
	if want := "write back the edits of box session " + session + " to " + strconv.Quote(filepath.Join(elsewhere, "notes.md")) + " (" + strconv.Quote(docs) + " now leads to " + strconv.Quote(elsewhere) + "): create " + strconv.Quote(filepath.Join(elsewhere, "notes (box).md")) + " beside it now, as it changed on the laptop"; asked[1] != want {
		t.Fatalf("asked %q, want %q", asked[1], want)
	}
}

// An origin swapped for a symlink changed on the laptop: the box's version
// goes beside it, never through it.
func TestAWriteBackOverAnOriginNowASymlinkGoesBesideIt(t *testing.T) {
	f := newFixture(t)
	docs := filepath.Join(f.home, "docs")
	origin := filepath.Join(docs, "notes.md")
	f.pasted(t, origin, "laptop")
	target := filepath.Join(f.home, "target.txt")
	write(t, target, "untouched")
	if err := os.Remove(origin); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, origin); err != nil {
		t.Fatal(err)
	}
	res, err := f.files.WriteBack(t.Context(), session, origin, content("agent"))
	if err != nil || res.Outcome != Conflict || read(t, res.Path) != "agent" {
		t.Fatalf("result %+v, %v", res, err)
	}
	if read(t, target) != "untouched" {
		t.Fatal("the write went through the symlink")
	}
	if asked := f.approver.asked(); len(asked) != 1 || !strings.Contains(asked[0], "to "+strconv.Quote(origin)+" (a symlink): create") {
		t.Fatalf("asked %q", asked)
	}
}

// Every save asks first, ~/Downloads included, once per destination and session.
func TestEverySaveAsksOncePerDestinationAndSession(t *testing.T) {
	f := newFixture(t)
	downloads := filepath.Join(f.home, "Downloads")
	for range 2 {
		if _, err := f.files.Save(t.Context(), session, "x.txt", "", content("c")); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(f.home, "docs", "out.txt")
	// Creating it is approved once, replacing it once more: by then it may hold the user's work.
	for _, c := range []string{"one", "two", "three"} {
		if _, err := f.files.Save(t.Context(), session, "x.txt", dest, content(c)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.files.Save(t.Context(), otherSession, "x.txt", dest, content("four")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"create " + strconv.Quote(filepath.Join(downloads, "x.txt")) + " with a file from box session " + session,
		"create " + strconv.Quote(filepath.Join(downloads, "x (1).txt")) + " with a file from box session " + session,
		"create " + strconv.Quote(dest) + " with a file from box session " + session,
		"replace " + strconv.Quote(dest) + " with a file from box session " + session + " (the old version goes to the Trash)",
		"replace " + strconv.Quote(dest) + " with a file from box session " + otherSession + " (the old version goes to the Trash)",
	}
	if got := f.approver.asked(); !slices.Equal(got, want) {
		t.Fatalf("asked\n%q\nwant\n%q", got, want)
	}
	if read(t, dest) != "four" || !slices.Equal(f.trashed, []string{"out.txt=one", "out.txt=two", "out.txt=three"}) {
		t.Fatalf("%s holds %q, trashed %q", dest, read(t, dest), f.trashed)
	}
}

func TestADeclinedSaveWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.approver.next = []error{errors.New("Canceled by user.")}
	if _, err := f.files.Save(t.Context(), session, "x.txt", "", content("c")); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	onlyFiles(t, filepath.Join(f.home, "Downloads"))
}

// Once approved, a save may go anywhere on the laptop; the prompt names the
// place it lands, with symlinks in its dir resolved.
func TestASaveGoesWhereverTheUserApproves(t *testing.T) {
	f := newFixture(t)
	downloads := filepath.Join(f.home, "Downloads")
	docs := filepath.Join(f.home, "docs")
	if err := os.Symlink(docs, filepath.Join(downloads, "escape")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(f.home), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ dest, want, risk string }{
		{filepath.Join(docs, "a.txt"), filepath.Join(docs, "a.txt"), ""},
		{"~/b.txt", filepath.Join(f.home, "b.txt"), ""},
		{"../docs/c.txt", filepath.Join(docs, "c.txt"), ""},
		{"escape/d.txt", filepath.Join(docs, "d.txt"), ""},
		{filepath.Join(downloads, "escape"), filepath.Join(docs, "x.txt"), ""},
		{filepath.Join(outside, "e.txt"), filepath.Join(outside, "e.txt"), " (outside your home)"},
		{"~/.zshenv", filepath.Join(f.home, ".zshenv"), " (hidden)"},
	} {
		saved, err := f.files.Save(t.Context(), session, "x.txt", tc.dest, content("c"))
		if err != nil || saved.Path != tc.want || read(t, tc.want) != "c" {
			t.Fatalf("Save to %q: %+v, %v; want %s", tc.dest, saved, err, tc.want)
		}
		asked := f.approver.asked()
		if want := "create " + strconv.Quote(tc.want) + " with a file from box session " + session + tc.risk; asked[len(asked)-1] != want {
			t.Fatalf("Save to %q asked %q, want %q", tc.dest, asked[len(asked)-1], want)
		}
	}
}

func TestSaveRefusesWhatNamesNoFileItCanWrite(t *testing.T) {
	f := newFixture(t)
	docs := filepath.Join(f.home, "docs")
	if err := os.Mkdir(filepath.Join(docs, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ name, dest string }{
		"a missing dir":         {"x.txt", "nope/x.txt"},
		"a file as a dir":       {"x.txt", filepath.Join(docs, "missing.txt") + "/"},
		"a name that is a path": {"../x.txt", ""},
		"a name that is ..":     {"..", ""},
		"an empty name":         {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.files.Save(t.Context(), session, tc.name, tc.dest, content("agent")); !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
			}
		})
	}
	if got := f.approver.asked(); len(got) != 0 {
		t.Fatalf("asked %q", got)
	}
	onlyFiles(t, docs, "sub")
}

// A symlink is a taken name in a dir, and a file of its own when named: a
// save replaces the link, never its target.
func TestASaveNeverWritesThroughASymlink(t *testing.T) {
	f := newFixture(t)
	downloads := filepath.Join(f.home, "Downloads")
	target := filepath.Join(f.home, "docs", "target.txt")
	write(t, target, "untouched")
	link := filepath.Join(downloads, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if saved, err := f.files.Save(t.Context(), session, "link.txt", "", content("agent")); err != nil || saved.Path != filepath.Join(downloads, "link (1).txt") {
		t.Fatalf("saved to %+v, %v", saved, err)
	}
	saved, err := f.files.Save(t.Context(), session, "x", link, content("agent"))
	if err != nil || saved.Path != link || read(t, link) != "agent" {
		t.Fatalf("saved to %+v, %v", saved, err)
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("%s is %v, %v", link, info.Mode(), err)
	}
	if read(t, target) != "untouched" {
		t.Fatal("the save went through the symlink")
	}
	if asked := f.approver.asked(); !strings.HasPrefix(asked[1], "replace "+strconv.Quote(link)) || !strings.Contains(asked[1], "a symlink") {
		t.Fatalf("asked %q", asked)
	}
}

func TestASaveWithoutASessionAsksForTheBox(t *testing.T) {
	f := newFixture(t)
	if _, err := f.files.Save(t.Context(), "", "x.md", "", content("agent")); err != nil {
		t.Fatal(err)
	}
	if asked := f.approver.asked(); len(asked) != 1 || asked[0] != "create "+strconv.Quote(filepath.Join(f.home, "Downloads", "x.md"))+" with a file from the box" {
		t.Fatalf("asked %q", asked)
	}
}

// The box picks the names in a prompt: quoted, they cannot pass for more of
// its text.
func TestAPromptQuotesWhatTheBoxNamed(t *testing.T) {
	f := newFixture(t)
	dest := filepath.Join(f.home, "Downloads", "a.txt) with nothing risky\n\nOK to")
	if _, err := f.files.Save(t.Context(), session, "x", dest, content("c")); err != nil {
		t.Fatal(err)
	}
	asked := f.approver.asked()
	if want := "create " + strconv.Quote(dest) + " with a file from box session " + session; len(asked) != 1 || asked[0] != want {
		t.Fatalf("asked %q, want %q", asked, want)
	}
	if _, err := f.files.Save(t.Context(), "claude-x (approved)", "x", "", content("c")); !errors.Is(err, ErrRefused) {
		t.Fatalf("saved for an invalid session: %v", err)
	}
	if len(f.approver.asked()) != 1 {
		t.Fatalf("asked for an invalid session: %q", f.approver.asked())
	}
}

// An approval holds on the terms its prompt named: a file that became
// executable since asks again.
func TestAWriteBackWhoseRisksChangedAsksAgain(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, ".notes")
	f.pasted(t, origin, "laptop")
	if _, err := f.files.WriteBack(t.Context(), session, origin, content("agent")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(origin, 0o700); err != nil {
		t.Fatal(err)
	}
	f.approver.next = []error{errors.New("Canceled by user.")}
	if _, err := f.files.WriteBack(t.Context(), session, origin, content("agent 2")); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want the new risk asked about and declined", err)
	}
	if asked := f.approver.asked(); len(asked) != 2 || !strings.Contains(asked[1], "(hidden, executable)") {
		t.Fatalf("asked %q", asked)
	}
}

// A save approved to create a file that someone else creates meanwhile is
// refused rather than replacing it unasked.
func TestASaveWhosePlaceChangedSinceTheApprovalIsRefused(t *testing.T) {
	f := newFixture(t)
	dest := filepath.Join(f.home, "docs", "new.txt")
	sneak := func(w io.Writer) error {
		write(t, dest, "the user's")
		_, err := io.WriteString(w, "agent")
		return err
	}
	if _, err := f.files.Save(t.Context(), session, "x", dest, sneak); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if read(t, dest) != "the user's" || len(f.trashed) != 0 {
		t.Fatalf("%s holds %q, trashed %q", dest, read(t, dest), f.trashed)
	}
}
