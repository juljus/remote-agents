package laptopfiles

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const session = "claude-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

type fixture struct {
	files   Files
	home    string
	trashed []string
	trash   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{home: filepath.Join(root, "home"), trash: filepath.Join(root, "trash")}
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

	res, err := f.files.WriteBack(session, origin, content("agent"))
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
	if res, err := f.files.WriteBack(session, origin, content("agent 2")); err != nil || res.Outcome != Written {
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

	res, err := f.files.WriteBack(session, origin, content("same"))
	if err != nil || res.Outcome != Unchanged {
		t.Fatalf("result %+v, %v", res, err)
	}
	if len(f.trashed) != 0 {
		t.Fatalf("trashed %q", f.trashed)
	}
	onlyFiles(t, filepath.Join(f.home, "docs"), "notes.md")
	// The laptop's content is the base now, so the next save replaces it.
	if res, err := f.files.WriteBack(session, origin, content("agent")); err != nil || res.Outcome != Written {
		t.Fatalf("next write-back %+v, %v", res, err)
	}
}

func TestWriteBackOverALaptopChangeGoesBesideIt(t *testing.T) {
	f := newFixture(t)
	docs := filepath.Join(f.home, "docs")
	origin := filepath.Join(docs, "notes.md")
	f.pasted(t, origin, "laptop")
	write(t, origin, "edited on the laptop")

	res, err := f.files.WriteBack(session, origin, content("agent"))
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
	if _, err := f.files.WriteBack(session, origin, content("agent 2")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, copyPath); got != "agent 2" || len(f.trashed) != 0 {
		t.Fatalf("copy holds %q, trashed %q", got, f.trashed)
	}

	// A copy the user changed goes to the Trash before the next one lands.
	write(t, copyPath, "user's")
	if _, err := f.files.WriteBack(session, origin, content("agent 3")); err != nil {
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
	res, err := f.files.WriteBack(session, origin, content("agent"))
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
			_, err := f.files.WriteBack(tc.session, tc.path, content("agent"))
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

func TestWriteBackRefusesAnOriginWhoseDirIsNowASymlink(t *testing.T) {
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
	if _, err := f.files.WriteBack(session, origin, content("agent")); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	onlyFiles(t, elsewhere)
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
	if _, err := f.files.WriteBack(session, origin, fetch); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.files.Save(session, "x.txt", "", fetch); !errors.Is(err, boom) {
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
		saved, err := f.files.Save(session, "report.pdf", "", content("new"))
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
		saved, err := f.files.Save(session, "x.txt", tc.dest, content("c"))
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

	saved, err := f.files.Save(session, "whatever.md", origin, content("agent"))
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
	if res, err := f.files.WriteBack(session, origin, content("agent 2")); err != nil || res.Outcome != Written {
		t.Fatalf("write-back %+v, %v", res, err)
	}
}

func TestSaveRefusesEverythingOutsideDownloadsAndThePastedFiles(t *testing.T) {
	f := newFixture(t)
	downloads := filepath.Join(f.home, "Downloads")
	docs := filepath.Join(f.home, "docs")
	origin := filepath.Join(docs, "notes.md")
	f.pasted(t, origin, "laptop")
	if err := os.Symlink(docs, filepath.Join(downloads, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(docs, "planted.txt"), filepath.Join(downloads, "dangling.txt")); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct{ session, name, dest string }{
		"another dir":                   {session, "x.txt", filepath.Join(docs, "x.txt")},
		"home":                          {session, "x.txt", "~/x.txt"},
		"up out of Downloads":           {session, "x.txt", "../docs/x.txt"},
		"an absolute path with ..":      {session, "x.txt", downloads + "/../docs/x.txt"},
		"a symlinked dir":               {session, "x.txt", "escape/x.txt"},
		"through a symlinked dir":       {session, "x.txt", filepath.Join(downloads, "escape")},
		"another session's origin":      {"codex-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "x.txt", origin},
		"a missing dir":                 {session, "x.txt", "nope/x.txt"},
		"a name that is a path":         {session, "../x.txt", ""},
		"a name that is ..":             {session, "..", ""},
		"an empty name":                 {session, "", ""},
		"an origin beside a pasted one": {session, "x.txt", filepath.Join(docs, "notes (box).md")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.files.Save(tc.session, tc.name, tc.dest, content("agent")); !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
			}
		})
	}
	if read(t, origin) != "laptop" || len(f.trashed) != 0 {
		t.Fatal("a refused save changed a file")
	}
	onlyFiles(t, docs, "notes.md")
	onlyFiles(t, downloads, "dangling.txt", "escape")

	// A symlink in Downloads is a taken name, never a way out.
	saved, err := f.files.Save(session, "dangling.txt", "", content("agent"))
	path := saved.Path
	if err != nil || path != filepath.Join(downloads, "dangling (1).txt") {
		t.Fatalf("saved to %s, %v", path, err)
	}
	onlyFiles(t, docs, "notes.md")
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
		res, err := f.files.WriteBack(session, path, content("agent"))
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

	res, err := f.files.WriteBack(session, origin, content("agent"))
	if err != nil || res.Outcome != Written || read(t, origin) != "agent" {
		t.Fatalf("result %+v, %v; origin holds %q", res, err, read(t, origin))
	}
	if res.Leftover == "" || read(t, res.Leftover) != "laptop" || !strings.Contains(res.LeftoverErr.Error(), "no Trash") {
		t.Fatalf("old version kept at %q (%v)", res.Leftover, res.LeftoverErr)
	}
	// The record moved on, so the next save is no conflict.
	if res, err := f.files.WriteBack(session, origin, content("agent 2")); err != nil || res.Outcome != Written {
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
	res, err := f.files.WriteBack(session, origin, content("agent"))
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

func TestSaveWithoutASessionGoesOnlyToDownloads(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, "docs", "notes.md")
	f.pasted(t, origin, "laptop")
	if _, err := f.files.Save("", "x.md", origin, content("agent")); !errors.Is(err, ErrRefused) {
		t.Fatalf("saved over a pasted file without a session: %v", err)
	}
	if saved, err := f.files.Save("", "x.md", "", content("agent")); err != nil || saved.Path != filepath.Join(f.home, "Downloads", "x.md") {
		t.Fatalf("saved to %+v, %v", saved, err)
	}
}

// A trailing slash says the path is a dir, so it is never the pasted file.
func TestSaveToAPastedFileAsADirIsRefused(t *testing.T) {
	f := newFixture(t)
	origin := filepath.Join(f.home, "docs", "notes.md")
	f.pasted(t, origin, "laptop")
	if _, err := f.files.Save(session, "x.md", origin+"/", content("agent")); !errors.Is(err, ErrRefused) {
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
	if res, err := f.files.WriteBack(session, origin, content("agent")); err != nil || res.Outcome != Written {
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
	if _, err := f.files.WriteBack(session, origin, content("agent")); !errors.Is(err, ErrRefused) {
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
			_, err := f.files.WriteBack(session, origin, content(c))
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
	_, err := f.files.WriteBack(session, origin, content("agent"))
	if err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want a failure that is no refusal", err)
	}
}
