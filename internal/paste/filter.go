// Package paste finds laptop image paths in what the user pastes into a
// session, so they can be replaced by copies on the box.
package paste

import (
	"bytes"
	"io"
	"time"
)

// Terminals wrap a paste in these markers once the agent enables bracketed paste.
var (
	startMarker = []byte("\x1b[200~")
	endMarker   = []byte("\x1b[201~")
)

// maxPaste bounds the paste Filter hands to Rewrite; a longer one is no list of paths.
const maxPaste = 64 << 10

// A terminal writes a marker and a paste in one go, so a pause means none is coming.
const (
	markerWait = 25 * time.Millisecond
	pasteWait  = 500 * time.Millisecond
)

type state int

const (
	typing   state = iota
	pasting        // within a paste, collecting it for Rewrite
	streamed       // within a paste too long for Rewrite, passing it on
)

// Filter passes keyboard input on to W unchanged, except that the content of
// each bracketed paste of at most maxPaste bytes goes through Rewrite.
// Whatever Write holds back, the caller passes on with Flush once no input
// came for HoldFor.
type Filter struct {
	W       io.Writer
	Rewrite func(content []byte) []byte

	state state
	// held is a partial start marker while typing, and the paste so far while pasting.
	held []byte
	// tail is the end of what was streamed, where an end marker may have begun.
	tail []byte
}

func (f *Filter) Write(p []byte) (int, error) {
	if err := f.feed(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (f *Filter) feed(p []byte) error {
	for len(p) > 0 {
		var err error
		switch f.state {
		case typing:
			p, err = f.typing(p)
		case pasting:
			p, err = f.pasting(p)
		case streamed:
			p, err = f.streaming(p)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (f *Filter) typing(p []byte) ([]byte, error) {
	buf := append(f.held, p...)
	f.held = nil
	if i := bytes.Index(buf, startMarker); i >= 0 {
		if _, err := f.W.Write(buf[:i]); err != nil {
			return nil, err
		}
		f.state = pasting
		return buf[i+len(startMarker):], nil
	}
	keep := partialMarker(buf, startMarker)
	f.held = append([]byte(nil), buf[len(buf)-keep:]...)
	_, err := f.W.Write(buf[:len(buf)-keep])
	return nil, err
}

func (f *Filter) pasting(p []byte) ([]byte, error) {
	f.held = append(f.held, p...)
	if i := bytes.Index(f.held, endMarker); i >= 0 && i <= maxPaste {
		content, rest := f.held[:i], f.held[i+len(endMarker):]
		f.held, f.state = nil, typing
		if err := f.emit(startMarker, f.Rewrite(content), endMarker); err != nil {
			return nil, err
		}
		return rest, nil
	}
	if len(f.held) <= maxPaste+len(endMarker) {
		return nil, nil
	}
	held := f.held
	f.held, f.state = nil, streamed
	if err := f.emit(startMarker); err != nil {
		return nil, err
	}
	return held, nil
}

func (f *Filter) streaming(p []byte) ([]byte, error) {
	buf := append(f.tail, p...)
	if i := bytes.Index(buf, endMarker); i >= 0 {
		end := i + len(endMarker) - len(f.tail)
		f.tail, f.state = nil, typing
		if _, err := f.W.Write(p[:end]); err != nil {
			return nil, err
		}
		return p[end:], nil
	}
	f.tail = append([]byte(nil), buf[len(buf)-min(len(buf), len(endMarker)-1):]...)
	_, err := f.W.Write(p)
	return nil, err
}

func (f *Filter) emit(parts ...[]byte) error {
	for _, part := range parts {
		if _, err := f.W.Write(part); err != nil {
			return err
		}
	}
	return nil
}

// partialMarker is the length of the longest proper prefix of marker that buf ends with.
func partialMarker(buf, marker []byte) int {
	for n := min(len(buf), len(marker)-1); n > 0; n-- {
		if bytes.HasSuffix(buf, marker[:n]) {
			return n
		}
	}
	return 0
}

// HoldFor is how long to wait for more input before Flush, or 0 when nothing is held.
func (f *Filter) HoldFor() time.Duration {
	switch {
	case f.state == pasting:
		return pasteWait
	case len(f.held) > 0:
		return markerWait
	}
	return 0
}

// Flush passes on what Write held back, as it came: a marker that was not one,
// or a paste that never ended.
func (f *Filter) Flush() error {
	held := f.held
	f.held = nil
	if f.state == pasting {
		f.state = typing
		return f.emit(startMarker, held)
	}
	_, err := f.W.Write(held)
	return err
}
