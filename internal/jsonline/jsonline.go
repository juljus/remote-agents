// Package jsonline frames strict JSON messages one per line.
package jsonline

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Read decodes the next line, refusing lines longer than max bytes.
func Read[T any](r *bufio.Reader, max int) (T, error) {
	var v T
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > max {
			return v, fmt.Errorf("message exceeds %d bytes", max)
		}
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return v, io.ErrUnexpectedEOF
			}
			return v, err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("decoding message: %w", err)
	}
	return v, nil
}

func Write(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding message: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("writing message: %w", err)
	}
	return nil
}
