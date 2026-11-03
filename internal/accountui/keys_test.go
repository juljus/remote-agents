package accountui

import (
	"io"
	"testing"
	"time"
)

// bursts is input that arrives in bursts: a byte within a burst comes at
// once, the first byte of the next burst only to a reader that waits forever.
type bursts struct {
	chunks [][]byte
}

func (s *bursts) next(timeout time.Duration) (byte, bool, error) {
	for len(s.chunks) > 0 && len(s.chunks[0]) == 0 {
		s.chunks = s.chunks[1:]
		if timeout >= 0 {
			// The rest arrives later than a key's next byte may.
			return 0, false, nil
		}
	}
	if len(s.chunks) == 0 {
		return 0, false, io.EOF
	}
	b := s.chunks[0][0]
	s.chunks[0] = s.chunks[0][1:]
	return b, true, nil
}

func TestReadKeyDecodesTheKeysThePickerUses(t *testing.T) {
	src := &bursts{chunks: [][]byte{
		[]byte("\x1b[A"), []byte("\x1bOB"), []byte("\x1b"), []byte("\r"), []byte("j"),
		[]byte("\x7f"), []byte("\x03"), []byte("\x1b[1;5C"), []byte("ü"), []byte("\x01"),
	}}
	want := []Key{
		{Code: KeyUp}, {Code: KeyDown}, {Code: KeyEsc}, {Code: KeyEnter}, {Code: KeyRune, Rune: 'j'},
		{Code: KeyBackspace}, {Code: KeyCtrlC}, {Code: KeyOther}, {Code: KeyRune, Rune: 'ü'}, {Code: KeyOther},
	}
	for i, w := range want {
		got, err := readKey(src)
		if err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
		if got != w {
			t.Fatalf("key %d = %+v, want %+v", i, got, w)
		}
	}
	if _, err := readKey(src); err != io.EOF {
		t.Fatalf("after the input: %v, want EOF", err)
	}
}

func TestReadKeyTellsALoneEscFromAnEscapeThatFollowsIt(t *testing.T) {
	src := &bursts{chunks: [][]byte{[]byte("\x1b"), []byte("[A")}}
	got, err := readKey(src)
	if err != nil || got.Code != KeyEsc {
		t.Fatalf("lone ESC = %+v, %v; want Esc", got, err)
	}
	// What came later is read on its own, not as the arrow it would have been.
	got, err = readKey(src)
	if err != nil || got != (Key{Code: KeyRune, Rune: '['}) {
		t.Fatalf("after ESC = %+v, %v; want [", got, err)
	}
}
