// Package pastefile is what the laptop and the box agree on about files
// pasted as laptop paths. Both directions of an exchange are a sequence of
// frames, each a decimal size, a newline and that many bytes.
//
// In paste-files the laptop sends a query of the pasted paths and the box
// answers which of them it has; the laptop then sends a name frame and a
// content frame for each file to store, and once the laptop's side ends the
// box sends a frame with the path of each stored file. While it receives, the
// box sends an empty frame for each ProgressEvery bytes, so the laptop can
// tell a slow link from a stalled one. paste-store, which
// laptops built before paste-files use, takes image content frames and
// answers a line per stored image.
package pastefile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// MaxSize bounds one pasted file.
const MaxSize = 100 << 20

// ProgressEvery is how many received bytes the box reports with each empty frame.
const ProgressEvery = 256 << 10

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

// WriteFrame writes one file to the stream.
func WriteFrame(w io.Writer, data []byte) error {
	if err := writeSize(w, int64(len(data))); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// CopyFrame writes the size bytes r holds to the stream.
func CopyFrame(w io.Writer, r io.Reader, size int64) error {
	if err := writeSize(w, size); err != nil {
		return err
	}
	n, err := io.CopyN(w, r, size)
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("the file ended at %d of its %d bytes", n, size)
	}
	return err
}

func writeSize(w io.Writer, size int64) error {
	if size > MaxSize {
		return fmt.Errorf("file of %d bytes exceeds %d", size, MaxSize)
	}
	_, err := fmt.Fprintf(w, "%d\n", size)
	return err
}

// queryJoin separates the paths of a query; no path holds it.
const queryJoin = "\x00"

// WriteQuery asks the box which of paths it has.
func WriteQuery(w io.Writer, paths []string) error {
	return WriteFrame(w, []byte(strings.Join(paths, queryJoin)))
}

// WriteAnswer tells the laptop, path by path in query order, whether the box has it.
func WriteAnswer(w io.Writer, onBox []bool) error {
	answer := make([]byte, len(onBox))
	for i, has := range onBox {
		answer[i] = '0'
		if has {
			answer[i] = '1'
		}
	}
	return WriteFrame(w, answer)
}

type Reader struct{ r *bufio.Reader }

func NewReader(r io.Reader) *Reader { return &Reader{r: bufio.NewReader(r)} }

// Next returns the next file, or io.EOF after the last one.
func (r *Reader) Next() ([]byte, error) {
	size, err := r.size()
	if err != nil {
		return nil, err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r.r, data); err != nil {
		return nil, fmt.Errorf("reading %d-byte frame: %w", size, unexpectedEOF(err))
	}
	return data, nil
}

// CopyNext copies the next file to w, or returns io.EOF after the last one.
func (r *Reader) CopyNext(w io.Writer) error {
	size, err := r.size()
	if err != nil {
		return err
	}
	if n, err := io.CopyN(w, r.r, size); err != nil {
		return fmt.Errorf("copying %d-byte frame after %d: %w", size, n, unexpectedEOF(err))
	}
	return nil
}

// unexpectedEOF keeps a stream that ends within a frame from reading as one that ended after it.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (r *Reader) size() (int64, error) {
	line, err := r.r.ReadString('\n')
	if err == io.EOF && line == "" {
		return 0, io.EOF
	}
	if err == io.EOF {
		return 0, fmt.Errorf("frame size %q ends without a newline", line)
	}
	if err != nil {
		return 0, fmt.Errorf("reading frame size: %w", err)
	}
	size, err := strconv.ParseInt(strings.TrimSuffix(line, "\n"), 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("invalid frame size %q", strings.TrimSuffix(line, "\n"))
	}
	if size > MaxSize {
		return 0, fmt.Errorf("file of %d bytes exceeds %d", size, MaxSize)
	}
	return size, nil
}

// Query reads the paths the laptop asks about.
func (r *Reader) Query() ([]string, error) {
	query, err := r.Next()
	if err != nil {
		return nil, fmt.Errorf("reading the query: %w", err)
	}
	return strings.Split(string(query), queryJoin), nil
}

// Answer reads whether the box has each of the n paths queried.
func (r *Reader) Answer(n int) ([]bool, error) {
	answer, err := r.Next()
	if err != nil {
		return nil, fmt.Errorf("reading the answer: %w", err)
	}
	if len(answer) != n {
		return nil, fmt.Errorf("the box answered %q for %d paths", answer, n)
	}
	onBox := make([]bool, n)
	for i, c := range answer {
		switch c {
		case '1':
			onBox[i] = true
		case '0':
		default:
			return nil, fmt.Errorf("the box answered %q", answer)
		}
	}
	return onBox, nil
}
