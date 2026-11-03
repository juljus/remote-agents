package paste

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"remote-agents/internal/pastefile"
)

var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01")

func TestWordsFollowShellQuoting(t *testing.T) {
	for in, want := range map[string][]string{
		`/a/b.png`:                   {"/a/b.png"},
		`/a/my\ shot.png /b/c.png`:   {"/a/my shot.png", "/b/c.png"},
		"'/a/my shot.png'\n\"/b/x\"": {"/a/my shot.png", "/b/x"},
		`"/a/\"q\" \x.png"`:          {`/a/"q" \x.png`},
		`/a/\(1\)\[2\]\&\;.png`:      {"/a/(1)[2]&;.png"},
		"  /a.png  \t":               {"/a.png"},
		`'/a/it'\''s.png'`:           {"/a/it's.png"},
	} {
		ws, ok := words([]byte(in))
		var got []string
		for _, w := range ws {
			got = append(got, w.value)
			if !strings.HasPrefix(in[w.start:], strings.TrimSpace(in[w.start:w.end])) || strings.TrimSpace(in[w.start:w.end]) != in[w.start:w.end] {
				t.Errorf("%q: word span %q is not the word", in, in[w.start:w.end])
			}
		}
		if !ok || !slices.Equal(got, want) {
			t.Errorf("words(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{`'/a`, `"/a`, `/a\`} {
		if _, ok := words([]byte(bad)); ok {
			t.Errorf("words(%q) accepted", bad)
		}
	}
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func escape(path string) string { return strings.ReplaceAll(path, " ", `\ `) }

func TestFilesNamesThePathsOnTheLaptop(t *testing.T) {
	dir := t.TempDir()
	shot := writeFile(t, dir, "my shot.png", png)
	report := writeFile(t, dir, "report.PDF", []byte("%PDF-1.7"))
	paste := escape(shot) + " " + report + " /nonexistent/box.txt " + dir + " " + filepath.Join(report, "under")
	files, err := Files([]byte(paste))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, f.Path)
	}
	if !slices.Equal(got, []string{shot, report, dir}) {
		t.Fatalf("Files = %q", got)
	}
}

func TestFilesIgnoresPastesThatAreNotJustPaths(t *testing.T) {
	shot := writeFile(t, t.TempDir(), "a.png", png)
	for _, paste := range []string{"see " + shot, shot + " and more", "'" + shot, "~/a.png", ""} {
		if files, err := Files([]byte(paste)); err != nil || len(files) != 0 {
			t.Errorf("Files(%q) = %v, %v", paste, files, err)
		}
	}
}

func TestFilesFailsOnAPathItCannotLookUp(t *testing.T) {
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := writeFile(t, locked, "a.txt", []byte("x"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if _, err := Files([]byte(inside)); err == nil {
		t.Fatal("a path that could not be looked up passed")
	}
}

func TestUploadableTakesOnlyRegularFilesWithinTheLimit(t *testing.T) {
	dir := t.TempDir()
	small := writeFile(t, dir, "small.zip", []byte("PK"))
	big := writeFile(t, dir, "big.zip", nil)
	if err := os.Truncate(big, pastefile.MaxSize+1); err != nil {
		t.Fatal(err)
	}
	limit := writeFile(t, dir, "limit.zip", nil)
	if err := os.Truncate(limit, pastefile.MaxSize); err != nil {
		t.Fatal(err)
	}
	files, err := Files([]byte(strings.Join([]string{small, limit, big, dir}, " ")))
	if err != nil || len(files) != 4 {
		t.Fatalf("Files = %v, %v", files, err)
	}
	for i, want := range []string{"", "", "big.zip is over 100 MB", filepath.Base(dir) + " is a directory"} {
		err := files[i].Uploadable()
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: Uploadable = %v, want %q", files[i].Path, err, want)
		}
	}
}

func TestReplaceSwapsOnlyTheGivenWords(t *testing.T) {
	dir := t.TempDir()
	shot := writeFile(t, dir, "my shot.png", png)
	kept := writeFile(t, dir, "kept.txt", []byte("x"))
	paste := kept + "  " + escape(shot) + "\n'" + shot + "'"
	files, err := Files([]byte(paste))
	if err != nil || len(files) != 3 {
		t.Fatalf("Files = %v, %v", files, err)
	}
	got := string(Replace([]byte(paste), files[1:], []string{"/home/agent/p/1.png", "/home/agent/my p/2.png"}))
	want := kept + "  /home/agent/p/1.png\n/home/agent/my\\ p/2.png"
	if got != want {
		t.Fatalf("Replace = %q, want %q", got, want)
	}
}

func TestReplaceEscapesOnlyWhatTheShellWouldReadAsSyntax(t *testing.T) {
	kept := writeFile(t, t.TempDir(), "a.txt", []byte("x"))
	files, err := Files([]byte(kept))
	if err != nil || len(files) != 1 {
		t.Fatalf("Files = %v, %v", files, err)
	}
	got := string(Replace([]byte(kept), files, []string{"/p/Tööplaan (1) at 4.52\u202fPM.png"}))
	if want := "/p/Tööplaan\\ \\(1\\)\\ at\\ 4.52\u202fPM.png"; got != want {
		t.Fatalf("Replace = %q, want %q", got, want)
	}
}
