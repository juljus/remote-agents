package corner

import "github.com/clipperhouse/displaywidth"

// certainRanges are the characters whose width terminals agree on and that
// never join a neighbour into one wider cell: Latin, Greek and Cyrillic with
// their combining marks, the symbol and box-drawing blocks, CJK, Hangul
// syllables and the private use area. Everything else, emoji and their
// modifiers, joiners and variation selectors included, is width uncertain.
var certainRanges = [][2]rune{
	{0x20, 0x7e},
	{0xa0, 0x58f},
	{0x2010, 0x2027},
	{0x2030, 0x205f},
	{0x2070, 0x20cf},
	{0x2100, 0x2bff},
	{0x2e80, 0x303f},
	{0x3041, 0x33ff},
	{0x3400, 0x4dbf},
	{0x4e00, 0x9fff},
	{0xa000, 0xa4cf},
	{0xac00, 0xd7a3},
	{0xe000, 0xf8ff},
	{0xf900, 0xfaff},
	{0xfe30, 0xfe4f},
	{0xff00, 0xff9d},
	{0xffa0, 0xffef},
	{0x20000, 0x3fffd},
}

// width is how many cells r takes as Ghostty prints it, when terminals agree on it.
func width(r rune) (int, bool) {
	if r <= 0xff {
		return 1, r >= 0x20 && r < 0x7f || r >= 0xa0
	}
	for _, rg := range certainRanges {
		if r >= rg[0] && r <= rg[1] {
			return displaywidth.Rune(r), true
		}
	}
	return 0, false
}
