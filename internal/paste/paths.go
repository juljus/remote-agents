package paste

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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

// A File is a word of a paste that names a path on the laptop.
type File struct {
	Path string
	info fs.FileInfo
	word word
}

// Files returns the words of content that name paths on the laptop, when
// content is nothing but absolute paths, as terminals paste files. A path
// that is not on the laptop is left out: it may name one on the box. A path
// that cannot be looked up is an error.
func Files(content []byte) ([]File, error) {
	ws, ok := words(content)
	if !ok {
		return nil, nil
	}
	for _, w := range ws {
		if !strings.HasPrefix(w.value, "/") {
			return nil, nil
		}
	}
	var out []File
	for _, w := range ws {
		info, err := os.Stat(w.value)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, File{Path: w.value, info: info, word: w})
	}
	return out, nil
}

// Uploadable says why f cannot go to the box, or nil when it can. It names
// f by its base name, short enough for a notice.
func (f File) Uploadable() error {
	name := filepath.Base(f.Path)
	switch {
	case f.info.IsDir():
		return fmt.Errorf("%s is a directory", name)
	case !f.info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", name)
	case f.info.Size() > pastefile.MaxSize:
		return fmt.Errorf("%s is over %d MB", name, pastefile.MaxSize>>20)
	}
	return nil
}

// Replace puts boxPaths[i] in place of files[i] in content, escaped for the
// shell; everything else stays as it was. files are in paste order.
func Replace(content []byte, files []File, boxPaths []string) []byte {
	var out []byte
	at := 0
	for i, f := range files {
		out = append(out, content[at:f.word.start]...)
		out = append(out, shellEscape(boxPaths[i])...)
		at = f.word.end
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
