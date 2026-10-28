package pastefile

import (
	"bytes"
	"io"
	"testing"
)

var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func TestImageExtGoesByContent(t *testing.T) {
	for name, tc := range map[string]struct {
		head []byte
		ext  string
		ok   bool
	}{
		"png":  {png, "png", true},
		"jpeg": {[]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), "jpg", true},
		"gif":  {[]byte("GIF89a\x01\x00\x01\x00"), "gif", true},
		"webp": {[]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), "webp", true},
		"text": {[]byte("not an image"), "", false},
		"svg":  {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "", false},
	} {
		ext, ok := ImageExt(tc.head)
		if ext != tc.ext || ok != tc.ok {
			t.Errorf("%s: ImageExt = %q, %v; want %q, %v", name, ext, ok, tc.ext, tc.ok)
		}
	}
}

func TestHasImageName(t *testing.T) {
	for name, want := range map[string]bool{
		"a.png": true, "a.PNG": true, "a.jpg": true, "a.jpeg": true, "a.gif": true, "a.webp": true,
		"a.svg": false, "a": false, "png": false, "a.png.txt": false,
	} {
		if got := HasImageName(name); got != want {
			t.Errorf("HasImageName(%q) = %v", name, got)
		}
	}
}

func TestFramesRoundTrip(t *testing.T) {
	var stream bytes.Buffer
	files := [][]byte{png, {}, []byte("second\nwith newline")}
	for _, f := range files {
		if err := WriteFrame(&stream, f); err != nil {
			t.Fatal(err)
		}
	}
	r := NewReader(&stream)
	for i, want := range files {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d = %q, want %q", i, got, want)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("after the last frame: %v, want EOF", err)
	}
}

func TestReaderRefusesOversizedAndTruncatedFrames(t *testing.T) {
	for name, stream := range map[string]string{
		"too large":  "20971521\n",
		"truncated":  "10\nabc",
		"not a size": "ten\n",
		"no newline": "12",
	} {
		if _, err := NewReader(bytes.NewBufferString(stream)).Next(); err == nil || err == io.EOF {
			t.Errorf("%s: Next = %v, want an error", name, err)
		}
	}
}
