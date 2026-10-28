// Package pastefile is what the laptop and the box agree on about image files
// pasted as laptop paths: which files qualify, and how the laptop sends them.
// The stream is a sequence of frames, each a decimal size, a newline and that
// many bytes.
package pastefile

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// MaxSize bounds one pasted file.
const MaxSize = 20 << 20

// SniffLen is how much of a file ImageExt needs.
const SniffLen = 512

var extByType = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// ImageExt names the extension of an image going by its first bytes.
func ImageExt(head []byte) (string, bool) {
	ext, ok := extByType[http.DetectContentType(head)]
	return ext, ok
}

// HasImageName reports whether name has an image extension. Claude attaches
// a pasted path only when it has one.
func HasImageName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// WriteFrame writes one file to the stream.
func WriteFrame(w io.Writer, data []byte) error {
	if len(data) > MaxSize {
		return fmt.Errorf("file of %d bytes exceeds %d", len(data), MaxSize)
	}
	if _, err := fmt.Fprintf(w, "%d\n", len(data)); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

type Reader struct{ r *bufio.Reader }

func NewReader(r io.Reader) *Reader { return &Reader{r: bufio.NewReader(r)} }

// Next returns the next file, or io.EOF after the last one.
func (r *Reader) Next() ([]byte, error) {
	line, err := r.r.ReadString('\n')
	if err == io.EOF && line == "" {
		return nil, io.EOF
	}
	if err == io.EOF {
		return nil, fmt.Errorf("frame size %q ends without a newline", line)
	}
	if err != nil {
		return nil, fmt.Errorf("reading frame size: %w", err)
	}
	size, err := strconv.Atoi(strings.TrimSuffix(line, "\n"))
	if err != nil || size < 0 {
		return nil, fmt.Errorf("invalid frame size %q", strings.TrimSuffix(line, "\n"))
	}
	if size > MaxSize {
		return nil, fmt.Errorf("file of %d bytes exceeds %d", size, MaxSize)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r.r, data); err != nil {
		return nil, fmt.Errorf("reading %d-byte frame: %w", size, err)
	}
	return data, nil
}
