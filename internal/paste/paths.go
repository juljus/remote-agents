package paste

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"remote-agents/internal/pastefile"
)

// A word is one shell word of a paste: its bytes there and what it means.
type word struct {
	start, end int
	value      string
}

// words splits content the way a shell would: whitespace separates words, a
// backslash escapes the byte after it, and quotes group, which covers how
// terminals paste dropped and copied files. ok is false for an unterminated
// quote or a trailing backslash.
func words(content []byte) (_ []word, ok bool) {
	var out []word
	var cur strings.Builder
	start := -1
	for i := 0; i < len(content); i++ {
		c := content[i]
		if start < 0 {
			if isSpace(c) {
				continue
			}
			start = i
		}
		switch {
		case isSpace(c):
			out = append(out, word{start: start, end: i, value: cur.String()})
			cur.Reset()
			start = -1
		case c == '\\':
			if i++; i == len(content) {
				return nil, false
			}
			cur.WriteByte(content[i])
		case c == '\'':
			end := strings.IndexByte(string(content[i+1:]), '\'')
			if end < 0 {
				return nil, false
			}
			cur.Write(content[i+1 : i+1+end])
			i += end + 1
		case c == '"':
			if i = doubleQuoted(content, i+1, &cur); i < 0 {
				return nil, false
			}
		default:
			cur.WriteByte(c)
		}
	}
	if start >= 0 {
		out = append(out, word{start: start, end: len(content), value: cur.String()})
	}
	return out, true
}

// doubleQuoted reads a double-quoted string starting at i into cur and returns
// the index of its closing quote, or -1 when it has none. Within it a
// backslash escapes only $, `, ", \ and newline.
func doubleQuoted(content []byte, i int, cur *strings.Builder) int {
	for ; i < len(content); i++ {
		c := content[i]
		switch {
		case c == '"':
			return i
		case c == '\\' && i+1 < len(content) && strings.IndexByte("$`\"\\\n", content[i+1]) >= 0:
			i++
			cur.WriteByte(content[i])
		default:
			cur.WriteByte(c)
		}
	}
	return -1
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// An Image is a word of a paste that names a laptop image file.
type Image struct {
	Path string
	word word
}

// Images returns the words of content that name laptop image files, when
// content is nothing but absolute paths, as terminals paste files. A path
// with no image name, or that is no regular file holding an image on the
// laptop, is left out: it may name a file on the box. An image that cannot
// be uploaded is an error.
func Images(content []byte) ([]Image, error) {
	ws, ok := words(content)
	if !ok {
		return nil, nil
	}
	for _, w := range ws {
		if !strings.HasPrefix(w.value, "/") {
			return nil, nil
		}
	}
	var out []Image
	for _, w := range ws {
		is, err := isImage(w.value)
		if err != nil {
			return nil, err
		}
		if is {
			out = append(out, Image{Path: w.value, word: w})
		}
	}
	return out, nil
}

func isImage(path string) (bool, error) {
	if !pastefile.HasImageName(path) {
		return false, nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, pastefile.SniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	if _, ok := pastefile.ImageExt(head[:n]); !ok {
		return false, nil
	}
	if info.Size() > pastefile.MaxSize {
		return false, fmt.Errorf("%s is too large to upload (%d MB, the limit is %d MB)", path, info.Size()>>20, pastefile.MaxSize>>20)
	}
	return true, nil
}

// Replace puts boxPaths[i] in place of images[i] in content, escaped for the
// shell; everything else stays as it was.
func Replace(content []byte, images []Image, boxPaths []string) []byte {
	var out []byte
	at := 0
	for i, im := range images {
		out = append(out, content[at:im.word.start]...)
		out = append(out, shellEscape(boxPaths[i])...)
		at = im.word.end
	}
	return append(out, content[at:]...)
}

func shellEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isPlain(c) {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isPlain(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("/._-+=:,@%", c) >= 0
}
