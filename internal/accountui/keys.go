// Package accountui is the laptop's account picker: a table of the box's
// accounts that switches, adds, renames and removes them, drawn in place in
// the terminal.
package accountui

import (
	"time"
)

// KeyCode is a key the picker tells apart.
type KeyCode int

const (
	KeyRune KeyCode = iota
	KeyUp
	KeyDown
	KeyEnter
	KeyEsc
	KeyBackspace
	KeyCtrlC
	// KeyOther is any key the picker ignores, such as a cursor key sideways.
	KeyOther
)

type Key struct {
	Code KeyCode
	// Rune is the character of a KeyRune.
	Rune rune
}

// byteSource yields the terminal's input a byte at a time; ok is false when
// no byte came within timeout, which a negative timeout never ends.
type byteSource interface {
	next(timeout time.Duration) (b byte, ok bool, err error)
}

// escapeWait is how long after an ESC a key's next byte may come: a lone
// ESC is the Esc key, ESC [ A the up arrow.
const escapeWait = 30 * time.Millisecond

// readKey reads one key.
func readKey(src byteSource) (Key, error) {
	b, _, err := src.next(-1)
	if err != nil {
		return Key{}, err
	}
	switch {
	case b == 0x1b:
		return readEscape(src)
	case b == '\r' || b == '\n':
		return Key{Code: KeyEnter}, nil
	case b == 0x7f || b == 0x08:
		return Key{Code: KeyBackspace}, nil
	case b == 0x03:
		return Key{Code: KeyCtrlC}, nil
	case b < 0x20:
		return Key{Code: KeyOther}, nil
	case b < 0x80:
		return Key{Code: KeyRune, Rune: rune(b)}, nil
	}
	return readUTF8(src, b)
}

// readEscape reads what follows an ESC: a CSI or SS3 sequence, or nothing.
func readEscape(src byteSource) (Key, error) {
	b, ok, err := src.next(escapeWait)
	if err != nil || !ok {
		return Key{Code: KeyEsc}, err
	}
	if b != '[' && b != 'O' {
		return Key{Code: KeyOther}, nil
	}
	// Parameters and intermediates run up to the final byte, 0x40-0x7e.
	for {
		b, ok, err = src.next(escapeWait)
		if err != nil || !ok {
			return Key{Code: KeyOther}, err
		}
		if b >= 0x40 && b <= 0x7e {
			break
		}
	}
	switch b {
	case 'A':
		return Key{Code: KeyUp}, nil
	case 'B':
		return Key{Code: KeyDown}, nil
	}
	return Key{Code: KeyOther}, nil
}

// readUTF8 reads the rest of a character whose first byte is first.
func readUTF8(src byteSource, first byte) (Key, error) {
	var n int
	switch {
	case first&0xe0 == 0xc0:
		n = 1
	case first&0xf0 == 0xe0:
		n = 2
	case first&0xf8 == 0xf0:
		n = 3
	default:
		return Key{Code: KeyOther}, nil
	}
	buf := []byte{first}
	for range n {
		b, ok, err := src.next(escapeWait)
		if err != nil || !ok {
			return Key{Code: KeyOther}, err
		}
		buf = append(buf, b)
	}
	r := []rune(string(buf))
	if len(r) != 1 {
		return Key{Code: KeyOther}, nil
	}
	return Key{Code: KeyRune, Rune: r[0]}, nil
}
