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

func TestImagesNamesOnlyLaptopImageFiles(t *testing.T) {
	dir := t.TempDir()
	shot := writeFile(t, dir, "my shot.png", png)
	other := writeFile(t, dir, "b.PNG", png)
	text := writeFile(t, dir, "notes.png", []byte("plain text, named like an image"))
	noExt := writeFile(t, dir, "image", png)
	paste := escape(shot) + " " + other + " " + text + " " + noExt + " /nonexistent/box.png " + dir
	images, err := Images([]byte(paste))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, im := range images {
		got = append(got, im.Path)
	}
	if !slices.Equal(got, []string{shot, other}) {
		t.Fatalf("Images = %q", got)
	}
}

func TestImagesIgnoresPastesThatAreNotJustPaths(t *testing.T) {
	shot := writeFile(t, t.TempDir(), "a.png", png)
	for _, paste := range []string{"see " + shot, shot + " and more", "'" + shot, "~/a.png", ""} {
		if images, err := Images([]byte(paste)); err != nil || len(images) != 0 {
			t.Errorf("Images(%q) = %v, %v", paste, images, err)
		}
	}
}

func TestImagesRefusesWhatCannotBeUploaded(t *testing.T) {
	dir := t.TempDir()
	big := writeFile(t, dir, "big.png", append(png, make([]byte, pastefile.MaxSize)...))
	if _, err := Images([]byte(big)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized image: %v", err)
	}
	locked := writeFile(t, dir, "locked.png", png)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Images([]byte(locked)); err == nil {
		t.Fatal("unreadable image accepted")
	}
}

func TestReplaceSwapsOnlyTheImageWords(t *testing.T) {
	dir := t.TempDir()
	shot := writeFile(t, dir, "my shot.png", png)
	paste := "/box/kept.txt  " + escape(shot) + "\n'" + shot + "'"
	images, err := Images([]byte(paste))
	if err != nil || len(images) != 2 {
		t.Fatalf("Images = %v, %v", images, err)
	}
	got := string(Replace([]byte(paste), images, []string{"/home/agent/p/1.png", "/home/agent/my p/2.png"}))
	want := "/box/kept.txt  /home/agent/p/1.png\n/home/agent/my\\ p/2.png"
	if got != want {
		t.Fatalf("Replace = %q, want %q", got, want)
	}
}
