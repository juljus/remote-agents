package corner

import "testing"

func TestWidth(t *testing.T) {
	for _, tc := range []struct {
		r       rune
		width   int
		certain bool
	}{
		{'a', 1, true},
		{0xad, 1, true},   // soft hyphen: Ghostty prints every Latin-1 character in one cell
		{0x301, 0, true},  // combining acute accent
		{'─', 1, true},    // box drawing
		{'❯', 1, true},    // dingbat
		{'⏺', 1, true},    // misc technical
		{'✅', 2, true},    // emoji presentation by default
		{'中', 2, true},    // CJK
		{0xe0b0, 1, true}, // private use, as in Nerd Font glyphs
		{0x85, 0, false},  // a C1 control sent as UTF-8
		{0xfe0f, 0, false},
		{0x200d, 0, false},
		{0x200b, 0, false},
		{0x1f600, 0, false},
		{0x1f3fd, 0, false},
		{0x1f1ea, 0, false},
		{0x915, 0, false}, // Devanagari, which joins consonants into one cluster
	} {
		w, certain := width(tc.r)
		if certain != tc.certain || certain && w != tc.width {
			t.Errorf("width(%U) = %d, %v; want %d, %v", tc.r, w, certain, tc.width, tc.certain)
		}
	}
}
