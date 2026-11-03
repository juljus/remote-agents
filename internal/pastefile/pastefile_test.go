package pastefile

import (
	"bytes"
	"errors"
	"io"
	"slices"
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
		"too large":  "104857601\n",
		"truncated":  "10\nabc",
		"no content": "10\n",
		"not a size": "ten\n",
		"no newline": "12",
	} {
		if _, err := NewReader(bytes.NewBufferString(stream)).Next(); err == nil || errors.Is(err, io.EOF) {
			t.Errorf("%s: Next = %v, want an error", name, err)
		}
		if err := NewReader(bytes.NewBufferString(stream)).CopyNext(io.Discard); err == nil || errors.Is(err, io.EOF) {
			t.Errorf("%s: CopyNext = %v, want an error", name, err)
		}
	}
}

func TestStreamedFramesRoundTrip(t *testing.T) {
	var stream bytes.Buffer
	if err := CopyFrame(&stream, bytes.NewReader([]byte("content")), 7); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&stream, []byte("next")); err != nil {
		t.Fatal(err)
	}
	r := NewReader(&stream)
	var got bytes.Buffer
	if err := r.CopyNext(&got); err != nil || got.String() != "content" {
		t.Fatalf("CopyNext = %q, %v", got.String(), err)
	}
	if next, err := r.Next(); err != nil || string(next) != "next" {
		t.Fatalf("Next = %q, %v", next, err)
	}
	if err := r.CopyNext(&got); err != io.EOF {
		t.Fatalf("after the last frame: %v, want EOF", err)
	}
}

func TestCopyFrameRefusesWhatDoesNotMatchItsSize(t *testing.T) {
	if err := CopyFrame(io.Discard, bytes.NewReader([]byte("short")), 10); err == nil {
		t.Fatal("a source shorter than its size was framed")
	}
	if err := CopyFrame(io.Discard, bytes.NewReader(nil), MaxSize+1); err == nil {
		t.Fatal("an oversized file was framed")
	}
}

func TestQueryAndAnswerRoundTrip(t *testing.T) {
	var stream bytes.Buffer
	paths := []string{"/a/my file.pdf", "/b/c\nd"}
	if err := WriteQuery(&stream, paths); err != nil {
		t.Fatal(err)
	}
	if err := WriteAnswer(&stream, []bool{true, false}); err != nil {
		t.Fatal(err)
	}
	r := NewReader(&stream)
	if got, err := r.Query(); err != nil || !slices.Equal(got, paths) {
		t.Fatalf("Query = %q, %v", got, err)
	}
	if got, err := r.Answer(2); err != nil || !slices.Equal(got, []bool{true, false}) {
		t.Fatalf("Answer = %v, %v", got, err)
	}
}

func TestAnswerRefusesTheWrongCount(t *testing.T) {
	var stream bytes.Buffer
	if err := WriteAnswer(&stream, []bool{true}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReader(&stream).Answer(2); err == nil {
		t.Fatal("an answer for one path passed for two")
	}
	if _, err := NewReader(bytes.NewBufferString("2\n1x")).Answer(2); err == nil {
		t.Fatal("an answer of other bytes passed")
	}
}
