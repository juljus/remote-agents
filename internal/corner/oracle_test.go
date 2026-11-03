package corner

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// pair runs the same output into two emulators: plain gets it as it is, and
// marked gets it through a Writer. Apart from a mark in the corner over
// cells blank in plain, the two must always show the same thing.
type pair struct {
	t             testing.TB
	plain, marked *vt.Emulator
	w             *Writer
	cols, rows    int
	sent          strings.Builder
}

func newPair(t *testing.T, cols, rows, row, col int) *pair { return newPairT(t, cols, rows, row, col) }

func newPairT(t testing.TB, cols, rows, row, col int) *pair {
	t.Helper()
	p := &pair{t: t, plain: vt.NewEmulator(cols, rows), marked: vt.NewEmulator(cols, rows), cols: cols, rows: rows}
	// No output here asks the emulators anything, which they would answer to no one.
	for _, e := range []*vt.Emulator{p.plain, p.marked} {
		e.WriteString(cup(row, col))
	}
	p.w = New(p.marked, cols, rows, t.Logf)
	if err := p.w.At(row, col); err != nil {
		t.Fatal(err)
	}
	p.w.Show()
	return p
}

// send writes s to both in chunks cut at random.
func (p *pair) send(rng *rand.Rand, s string) {
	p.t.Helper()
	s = p.resolve(s)
	p.sent.WriteString(s)
	for len(s) > 0 {
		n := min(len(s), 1+rng.IntN(24))
		if _, err := p.plain.WriteString(s[:n]); err != nil {
			p.t.Fatal(err)
		}
		if _, err := p.w.Write([]byte(s[:n])); err != nil {
			p.t.Fatal(err)
		}
		s = s[n:]
	}
}

// resolve puts the cursor's position in place of a leading here.
func (p *pair) resolve(s string) string {
	rest, ok := strings.CutPrefix(s, here)
	if !ok {
		return s
	}
	at := p.plain.CursorPosition()
	return cup(at.Y+1, at.X+1) + rest
}

func (p *pair) sendWhole(s string) {
	s = p.resolve(s)
	p.sent.WriteString(s)
	p.plain.WriteString(s)
	p.w.Write([]byte(s))
}

func (p *pair) markShown() bool {
	return strings.HasPrefix(p.marked.CellAt(p.cols-2, p.rows-1).Content, "☁")
}

func isBlank(c *uv.Cell) bool {
	return c == nil || (c.Content == " " || c.Content == "") && c.Style.Attrs == 0 && c.Style.Underline == 0 && c.Link.URL == ""
}

func (p *pair) fail(format string, args ...any) {
	p.t.Helper()
	p.t.Fatalf("%s\nsent: %q\nplain:\n%s\nmarked:\n%s", fmt.Sprintf(format, args...), tail(p.sent.String(), 2000), p.plain.String(), p.marked.String())
}

func tail(s string, n int) string { return s[max(len(s)-n, 0):] }

// check compares the two screens, their cursors and their scrollback.
func (p *pair) check() {
	p.t.Helper()
	if a, b := p.plain.CursorPosition(), p.marked.CursorPosition(); a != b {
		p.fail("cursor at %v, marked at %v", a, b)
	}
	if a, b := p.plain.IsAltScreen(), p.marked.IsAltScreen(); a != b {
		p.fail("alt screen %v, marked %v", a, b)
	}
	shown := p.markShown()
	for y := range p.rows {
		for x := range p.cols {
			a, b := p.plain.CellAt(x, y), p.marked.CellAt(x, y)
			if shown && y == p.rows-1 && x >= p.cols-2 {
				if !isBlank(a) {
					p.fail("mark over %q at %d,%d", a.Content, x, y)
				}
				continue
			}
			if b != nil && strings.Contains(b.Content, "☁") {
				p.fail("a mark outside the corner at %d,%d", x, y)
			}
			if !a.Equal(b) {
				p.fail("cell %d,%d is %+v, marked %+v", x, y, a, b)
			}
		}
	}
	if a, b := p.plain.ScrollbackLen(), p.marked.ScrollbackLen(); a != b {
		p.fail("scrollback has %d lines, marked %d", a, b)
	}
	for y := range p.plain.ScrollbackLen() {
		for x := range p.cols {
			a, b := p.plain.ScrollbackCellAt(x, y), p.marked.ScrollbackCellAt(x, y)
			if b != nil && strings.Contains(b.Content, "☁") {
				p.fail("a mark in scrollback line %d", y)
			}
			if (a == nil) != (b == nil) || a != nil && !a.Equal(b) {
				p.fail("scrollback cell %d,%d is %+v, marked %+v", x, y, a, b)
			}
		}
	}
}

// probe prints the same character straight into both, so the cells show
// whether the pen, modes and charset came back.
func (p *pair) probe() {
	p.t.Helper()
	p.plain.WriteString("Z")
	p.marked.WriteString("Z")
	p.check()
}

func TestOracleClaudeLikeSession(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	p := newPair(t, 40, 12, 3, 1)
	p.send(rng, "\x1b7\x1b[r\x1b8\x1b[?2004h\x1b[?1004h")
	frame := func(lines []string, prev int) string {
		var b strings.Builder
		b.WriteString("\x1b[?2026h")
		for range prev {
			b.WriteString("\x1b[2K\x1b[1A")
		}
		b.WriteString("\x1b[2K\x1b[G")
		for i, l := range lines {
			if i > 0 {
				b.WriteString("\r\r\n")
			}
			b.WriteString(l)
		}
		b.WriteString("\x1b[?2026l")
		return b.String()
	}
	prompt := []string{"\x1b[38;2;255;193;7m" + strings.Repeat("─", 40) + "\x1b[39m", "\x1b[1m❯\x1b[22m \x1b[2mTry something\x1b[22m", "\x1b[48;5;236m  ? for shortcuts \x1b[49m"}
	p.send(rng, frame(prompt, 0))
	p.check()
	if !p.markShown() {
		t.Fatalf("no mark after the first frame; marked:\n%s", p.marked.String())
	}
	prev := len(prompt) - 1
	for i := range 30 {
		// Each answer line goes above the prompt and pushes the screen up.
		line := fmt.Sprintf("\x1b[2G\x1b]8;id=a;https://example.com/%d\x1b\\link %d\x1b]8;;\x1b\\ some text \x1b[31mred\x1b[39m", i, i)
		lines := append([]string{line}, prompt...)
		p.send(rng, frame(lines, prev))
		prev = len(prompt) - 1
		p.check()
	}
	if !p.markShown() {
		t.Fatalf("no mark at the end; marked:\n%s", p.marked.String())
	}
	if p.plain.ScrollbackLen() == 0 {
		t.Fatal("the session never scrolled")
	}
	p.probe()
}

func TestOracleCodexLikeAltScreen(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	p := newPair(t, 30, 10, 10, 1)
	p.send(rng, "\x1b[?2026h\x1b[?1049h\x1b[1;1H\x1b[J\x1b[?2026l")
	p.check()
	for i := range 10 {
		var b strings.Builder
		b.WriteString("\x1b[?2026h")
		for row := 1; row <= 10; row++ {
			fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[0m\x1b[49m\x1b[K", row, 2+(i+row)%20)
			fmt.Fprintf(&b, "\x1b[%d;1H\x1b[1mrow %d frame %d\x1b[22m", row, row, i)
		}
		b.WriteString("\x1b[10;3H\x1b[?25h\x1b[?2026l")
		p.send(rng, b.String())
		p.check()
		if !p.markShown() {
			t.Fatalf("frame %d: no mark; marked:\n%s", i, p.marked.String())
		}
	}
	p.send(rng, "\x1b[?1049l")
	p.check()
	p.probe()
}

func TestOracleScrollingOutputLeavesNoCopies(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	p := newPair(t, 20, 6, 1, 1)
	for i := range 40 {
		p.send(rng, fmt.Sprintf("line %d\r\n", i))
		p.check()
	}
	// Lines that fill the row, and wrap at the bottom.
	for i := range 10 {
		p.send(rng, strings.Repeat(string(rune('a'+i)), 20+i*3))
		p.check()
	}
	p.send(rng, "\r\n")
	p.check()
	if !p.markShown() {
		t.Fatalf("no mark at the end; marked:\n%s", p.marked.String())
	}
	p.probe()
}

// vocabulary makes output the oracle and Ghostty agree on. The oracle does
// not wrap wide characters at the right edge, joins a combining mark only
// within one write, gets origin mode and moving or scrolling around a
// region wrong,
// does NEL without the carriage return, erases the whole row for ED 1, homes
// the cursor for 1049 and to the region's top for DECSTBM, keeps the scroll
// region and hyperlink per screen, sets no pending wrap with autowrap off,
// keeps a pending wrap through erasing, inserting, indexing and DECRC,
// moves the cursor and fills scrollback on DL, erases ED 2 and 1049 without
// the pen's background, and restores the hyperlink with the cursor; so none
// of that is in here. Neither are zero counts, which terminals read
// differently, or restoring a cursor never saved. A token that starts with
// here gets the cursor moved to where it is first, which ends a pending wrap.
const here = "\x00"

type vocabulary struct {
	rng        *rand.Rand
	cols, rows int
	alt, saved bool
	// region reports a scroll region smaller than the screen.
	region bool
}

func (v *vocabulary) next() string {
	n := func(max int) int { return v.rng.IntN(max) }
	cols, rows := v.cols, v.rows
	switch n(40) {
	case 0, 1, 2, 3, 4, 5:
		words := []string{"a", "word", "─│╭╮", "é", "αβγ", "longer-text-run", "\t"}
		return words[n(len(words))]
	case 6, 7:
		return "\r\n"
	case 8:
		return "\n"
	case 9:
		return "\r"
	case 10:
		return "\b"
	case 11:
		return fmt.Sprintf("\x1b[%d;%dH", n(rows+3), n(cols+3))
	case 12:
		if v.region {
			return fmt.Sprintf("\x1b[%d%c", 1+n(4), "CDGd`ae"[n(7)])
		}
		return fmt.Sprintf("\x1b[%d%c", 1+n(4), "ABCDEFGd`ae"[n(11)])
	case 13:
		return here + fmt.Sprintf("\x1b[%dK", n(3))
	case 14:
		return here + []string{"\x1b[J", "\x1b[49m\x1b[2J"}[n(2)]
	case 15:
		return here + fmt.Sprintf("\x1b[%dX", 1+n(4))
	case 16:
		return here + fmt.Sprintf("\x1b[%d@", 1+n(3))
	case 17:
		return here + fmt.Sprintf("\x1b[%dP", 1+n(3))
	case 18, 19:
		if v.region {
			return "a"
		}
		return here + fmt.Sprintf("\x1b[%dL", 1+n(3))
	case 20:
		if v.region {
			return "a"
		}
		return fmt.Sprintf("\x1b[%dS", 1+n(3))
	case 21:
		if v.region {
			return "a"
		}
		return fmt.Sprintf("\x1b[%dT", 1+n(3))
	case 22:
		if v.region {
			return "a"
		}
		return here + []string{"\x1bM", "\x1bD"}[n(2)]
	case 23, 24:
		sgr := []string{"0", "1", "3", "4", "7", "31", "42", "38;5;99", "48;2;10;20;30", "39", "49", "22", "24", "27"}
		return "\x1b[" + sgr[n(len(sgr))] + "m"
	case 25:
		// The oracle panics on a region past the screen, which Ghostty clamps.
		top := 1 + n(rows)
		bottom := min(top+n(rows), rows)
		if top < bottom {
			v.region = top > 1 || bottom < rows
		}
		return fmt.Sprintf("\x1b[%d;%dr\x1b[H", top, bottom)
	case 26:
		v.region = false
		return "\x1b[r\x1b[H"
	case 27:
		return fmt.Sprintf("\x1b[%d;%dH%s", rows-n(2), cols-n(3), strings.Repeat("y", n(4)))
	case 28:
		return []string{"\x1b[4h", "\x1b[4l"}[n(2)]
	case 29:
		return "\x1b[?7l" + strings.Repeat("z", n(cols+2)) + "\x1b[?7h\r"
	case 30:
		if v.saved && n(2) == 0 {
			return here + "\x1b]8;;\x1b\\\x1b8"
		}
		v.saved = true
		return here + "\x1b]8;;\x1b\\\x1b7"
	case 31:
		v.alt, v.region = !v.alt, false
		if v.alt {
			v.saved = false
			return "\x1b]8;;\x1b\\\x1b[49m\x1b[r\x1b[?1049h\x1b[H"
		}
		return "\x1b]8;;\x1b\\\x1b[49m\x1b[r\x1b[?1049l"
	case 32:
		return []string{"\x1b[?2026h", "\x1b[?2026l"}[n(2)]
	case 33:
		return []string{"\x1b]8;;https://example.com\x1b\\", "\x1b]8;;\x1b\\", "\x1b]0;title\x07"}[n(3)]
	case 34:
		return fmt.Sprintf("x\x1b[%db", 1+n(4))
	case 35:
		return fmt.Sprintf("\x1b[%d;%dH%s", rows, cols-n(4), strings.Repeat("y", n(3)))
	case 36:
		return fmt.Sprintf("\x1b[%dI", 1+n(3))
	}
	return fmt.Sprintf("\x1b[%d;1H\x1b[2K", rows)
}

// step is one token of random output and where its chunks are cut.
type step struct {
	tok  string
	cuts []int
}

func randomSteps(seed uint64) (cols, rows, row, col int, steps []step) {
	rng := rand.New(rand.NewPCG(seed, 7))
	cols, rows = 8+rng.IntN(20), 3+rng.IntN(8)
	row, col = 1+rng.IntN(rows), 1+rng.IntN(cols)
	v := &vocabulary{rng: rng, cols: cols, rows: rows}
	for range 150 {
		tok := v.next()
		var cuts []int
		for left := len(tok) + len(here)*16; left > 0; {
			n := min(left, 1+rng.IntN(24))
			cuts = append(cuts, n)
			left -= n
		}
		steps = append(steps, step{tok, cuts})
	}
	return cols, rows, row, col, steps
}

// run sends each step, cut as it says, and checks after each.
func (p *pair) run(steps []step) {
	p.t.Helper()
	for _, st := range steps {
		s := p.resolve(st.tok)
		p.sent.WriteString(s)
		for _, n := range st.cuts {
			n = min(n, len(s))
			p.plain.WriteString(s[:n])
			if _, err := p.w.Write([]byte(s[:n])); err != nil {
				p.t.Fatal(err)
			}
			s = s[n:]
		}
		p.check()
	}
	p.sendWhole("\x1b[?2026l")
	p.probe()
}

func TestOracleRandomOutput(t *testing.T) {
	for seed := range uint64(300) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			cols, rows, row, col, steps := randomSteps(seed)
			newPair(t, cols, rows, row, col).run(steps)
		})
	}
}
