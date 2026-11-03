package corner

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
)

func newWriter(t *testing.T, cols, rows int) (*Writer, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	return New(&out, cols, rows, t.Logf), &out
}

// written returns what writing s sent to the terminal.
func written(t *testing.T, w *Writer, out *bytes.Buffer, s string) string {
	t.Helper()
	out.Reset()
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestAtClearsTheCornerBelowTheCursor(t *testing.T) {
	w, out := newWriter(t, 10, 4)
	if err := w.At(2, 3); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "\x1b[4;9H\x1b[0m\x1b[2X\x1b[2;3H"; got != want {
		t.Fatalf("At wrote %q, want %q", got, want)
	}
}

func TestAtOnTheLastRowLeavesTheCornerUnknown(t *testing.T) {
	w, out := newWriter(t, 10, 4)
	if err := w.At(4, 1); err != nil {
		t.Fatal(err)
	}
	w.Show()
	if out.Len() != 0 {
		t.Fatalf("At wrote %q", out)
	}
	if got := written(t, w, out, "x"); got != "x" {
		t.Fatalf("a mark over an unknown corner: %q", got)
	}
	// The shell's row scrolls away; the new last row is blank.
	if got := written(t, w, out, "\r\n"); got != "\r\n\x1b[0m\x1b[4;9H"+Mark+"\x1b[4;1H\x1b[0m" {
		t.Fatalf("after scrolling wrote %q", got)
	}
}

func TestMarkIsDrawnOnlyOnceShown(t *testing.T) {
	w, out := newWriter(t, 10, 4)
	w.At(1, 1)
	if got := written(t, w, out, "hi"); got != "hi" {
		t.Fatalf("drawn before Show: %q", got)
	}
	w.Show()
	if got, want := written(t, w, out, "!"), "!\x1b[0m\x1b[4;9H"+Mark+"\x1b[1;4H\x1b[0m"; got != want {
		t.Fatalf("wrote %q, want %q", got, want)
	}
	if got := written(t, w, out, "more"); got != "more" {
		t.Fatalf("drawn again: %q", got)
	}
}

const hide = "\x1b[0m\x1b[4;9H\x1b[2X"

func shown(t *testing.T, cols, rows, row, col int) (*Writer, *bytes.Buffer) {
	t.Helper()
	w, out := newWriter(t, cols, rows)
	w.At(row, col)
	w.Show()
	written(t, w, out, "")
	if !w.t.active().drawn {
		t.Fatal("no mark")
	}
	return w, out
}

func TestScrollAtTheBottomHidesTheMarkFirst(t *testing.T) {
	w, out := shown(t, 10, 4, 3, 1)
	written(t, w, out, "\x1b[4;1H")
	got := written(t, w, out, "a\nb")
	want := "a" + hide + "\x1b[4;2H\x1b[0m\nb\x1b[0m\x1b[4;9H" + Mark + "\x1b[4;3H\x1b[0m"
	if got != want {
		t.Fatalf("wrote %q, want %q", got, want)
	}
}

func TestMarkKeepsClearOfText(t *testing.T) {
	w, out := shown(t, 10, 4, 1, 1)
	if got := written(t, w, out, "\x1b[4;8Habc"); strings.Contains(got, Mark) || !strings.HasPrefix(got, "\x1b[4;8Ha") {
		t.Fatalf("wrote %q", got)
	}
	if w.t.active().drawn {
		t.Fatal("drawn over text")
	}
	if got := written(t, w, out, "\r\x1b[2K"); !strings.Contains(got, Mark) {
		t.Fatalf("not drawn once the row is erased: %q", got)
	}
}

func TestPendingWrapHidesTheMarkUntilTheCursorMoves(t *testing.T) {
	w, out := shown(t, 10, 4, 1, 1)
	got := written(t, w, out, "\x1b[2;1H0123456789")
	if !strings.Contains(got, "012345678"+hide+"\x1b[2;10H\x1b[0m9") || strings.HasSuffix(got, "9\x1b[0m") {
		t.Fatalf("wrote %q", got)
	}
	if got := written(t, w, out, "\r"); !strings.Contains(got, Mark) {
		t.Fatalf("not drawn after the carriage return: %q", got)
	}
}

func TestSynchronizedFrameGetsTheMarkBeforeItEnds(t *testing.T) {
	w, out := shown(t, 10, 4, 1, 1)
	got := written(t, w, out, "\x1b[?2026h\x1b[4;1H\x1b[2Kx\x1b[1;1H\x1b[?2026l")
	want := "\x1b[?2026h\x1b[4;1H\x1b[2Kx\x1b[1;1H\x1b[0m\x1b[4;9H" + Mark + "\x1b[1;1H\x1b[0m\x1b[?2026l"
	if got != want {
		t.Fatalf("wrote %q, want %q", got, want)
	}
	if got := written(t, w, out, "\x1b[?2026h\x1b[4;1H\x1b[2K"); got != "\x1b[?2026h\x1b[4;1H\x1b[2K" {
		t.Fatalf("drawn inside an open frame: %q", got)
	}
}

func TestUncertainWidthHidesTheMarkUntilTheColumnIsKnown(t *testing.T) {
	w, out := shown(t, 20, 4, 1, 1)
	got := written(t, w, out, "\u26a0\ufe0f ok")
	if !strings.HasPrefix(got, "⚠\x1b[0m\x1b[4;19H\x1b[2X\x1b[1;2H\x1b[0m️ ok") || strings.Contains(got, Mark) {
		t.Fatalf("wrote %q", got)
	}
	if got := written(t, w, out, "\r"); !strings.Contains(got, Mark) {
		t.Fatalf("not drawn after the carriage return: %q", got)
	}
}

func TestUnfinishedSequenceIsHeldBack(t *testing.T) {
	w, out := shown(t, 10, 4, 1, 1)
	if got := written(t, w, out, "x\x1b["); got != "x" {
		t.Fatalf("wrote %q", got)
	}
	if got := written(t, w, out, "4;1H"); got != "\x1b[4;1H" {
		t.Fatalf("wrote %q", got)
	}
	if got := written(t, w, out, "\x1b]0;a title"); got != "\x1b]0;a title" {
		t.Fatalf("a string is not held back: %q", got)
	}
	if got := written(t, w, out, "\x07"); got != "\x07" {
		t.Fatalf("wrote %q", got)
	}
}

func TestLoneC1ByteIsAReplacementCharacter(t *testing.T) {
	w, out := shown(t, 10, 4, 1, 1)
	got := written(t, w, out, "\x9b4;1Hz")
	// Ghostty prints U+FFFD and then the rest as text; the width is uncertain, so the mark goes.
	if !strings.HasPrefix(got, hide) || !strings.Contains(got, "\x9b4;1Hz") {
		t.Fatalf("wrote %q", got)
	}
}

func TestInvalidUTF8LosesTheCursor(t *testing.T) {
	w, out := shown(t, 10, 4, 1, 1)
	written(t, w, out, "\xe2\x1b[2Jx")
	if w.t.cur().rowKnown {
		t.Fatal("the cursor is still known")
	}
}

func TestEraseDisplayOnThePrimaryScreenHidesTheMarkFirst(t *testing.T) {
	// Ghostty may push the screen into scrollback first.
	w, out := shown(t, 10, 4, 1, 1)
	if got := written(t, w, out, "\x1b[2J"); !strings.HasPrefix(got, hide) {
		t.Fatalf("wrote %q", got)
	}
}

func TestLeavingAScreenTakesItsMark(t *testing.T) {
	w, out := shown(t, 10, 4, 1, 1)
	got := written(t, w, out, "\x1b[?1049h")
	if !strings.HasPrefix(got, hide+"\x1b[1;1H\x1b[0m\x1b[?1049h") || !strings.HasSuffix(got, Mark+"\x1b[1;1H\x1b[0m") {
		t.Fatalf("wrote %q", got)
	}
	got = written(t, w, out, "\x1b[?1049l")
	if !strings.HasPrefix(got, hide+"\x1b[1;1H\x1b[0m\x1b[?1049l") || !strings.HasSuffix(got, Mark+"\x1b[1;1H\x1b[0m") {
		t.Fatalf("wrote %q", got)
	}
}

func TestMarkLeavesAndRestoresModes(t *testing.T) {
	w, out := newWriter(t, 10, 6)
	w.At(1, 1)
	written(t, w, out, "\x1b[6;1H\x1b[48;5;7m\x1b[2K\x1b]8;;https://example.com\x1b\\\x1b[2;5r\x1b[?6h\x1b[4h\x1b[1;31m\x1b[3;2H")
	w.Show()
	got := written(t, w, out, "")
	want := "\x1b]8;;\x1b\\\x1b[4l\x1b[?6l\x1b[0;48;5;7m\x1b[6;9H" + Mark + "\x1b[?6h\x1b[3;2H\x1b[0;1;31;48;5;7m\x1b[4h\x1b]8;;https://example.com\x1b\\"
	if got != want {
		t.Fatalf("wrote %q, want %q", got, want)
	}
}

func TestRepeatAfterTheMarkRepeatsTheOutputsCharacter(t *testing.T) {
	w, out := shown(t, 10, 4, 1, 1)
	written(t, w, out, "x\x1b[4;1H\x1b[2K\x1b[1;2H")
	if got := written(t, w, out, "\x1b[3b"); got != "xxx" {
		t.Fatalf("wrote %q", got)
	}
	if got := written(t, w, out, "\x1b[2b"); got != "\x1b[2b" {
		t.Fatalf("wrote %q", got)
	}
}

func TestSingleShiftKeepsTheMarkAway(t *testing.T) {
	w, out := newWriter(t, 10, 4)
	w.At(1, 1)
	w.Show()
	if got := written(t, w, out, "\x1bN"); got != "\x1bN" {
		t.Fatalf("wrote %q", got)
	}
	if got := written(t, w, out, "a"); !strings.Contains(got, Mark) {
		t.Fatalf("wrote %q", got)
	}
}

func TestBottomLineKeepsTheMarkAndTheCursor(t *testing.T) {
	w, out := shown(t, 12, 4, 2, 3)
	written(t, w, out, "\x1b(0")
	out.Reset()
	ok, err := w.BottomLine("a long notice text")
	if err != nil || !ok {
		t.Fatalf("BottomLine = %v, %v", ok, err)
	}
	want := "\x1b(B\x0f\x1b[0m\x1b[4;1H\x1b[2Ka long not\x1b(0\x0f\x1b[2;3H\x1b[0m\x1b[0m\x1b[4;11H" + Mark + "\x1b[2;3H\x1b[0m"
	if got := out.String(); got != want {
		t.Fatalf("wrote %q, want %q", got, want)
	}
	if c := w.t.cur(); c.row != 1 || c.lo != 2 || !c.exact() || c.charset.g[0] != '0' {
		t.Fatalf("cursor %+v", *c)
	}
}

func TestBottomLineNeedsAKnownCursor(t *testing.T) {
	w, out := newWriter(t, 12, 4)
	if ok, err := w.BottomLine("notice"); ok || err != nil || out.Len() != 0 {
		t.Fatalf("BottomLine = %v, %v, wrote %q", ok, err, out)
	}
}

func TestCloseTakesTheMarkAway(t *testing.T) {
	w, out := shown(t, 10, 4, 1, 1)
	out.Reset()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != hide+"\x1b[1;1H\x1b[0m" {
		t.Fatalf("Close wrote %q", got)
	}
	if got := written(t, w, out, "\x1b[4;1H\r\n"); got != "\x1b[4;1H\r\n" {
		t.Fatalf("after Close wrote %q", got)
	}
}

func TestResizeWaitsForTheOutputToPlaceTheCursor(t *testing.T) {
	w, out := shown(t, 10, 4, 1, 1)
	w.Resize(12, 5)
	if got := written(t, w, out, "x"); got != "x" {
		t.Fatalf("wrote %q", got)
	}
	if got := written(t, w, out, "\x1b[H\x1b[J"); !strings.HasSuffix(got, "\x1b[5;11H"+Mark+"\x1b[1;1H\x1b[0m") {
		t.Fatalf("wrote %q", got)
	}
}

// edge is output for the model alone, beyond what the oracle follows:
// regions, origin mode, wide and width-uncertain characters, joiners.
func edge(rng *rand.Rand, cols, rows int) string {
	n := rng.IntN
	tokens := []func() string{
		func() string {
			return []string{"a", "word ", "中文", "\u26a0\ufe0f", "e\u0301", "\U0001f44d\U0001f3fd", "\U0001f1ea\U0001f1ea", "x\u200dy"}[n(8)]
		},
		func() string { return "\r\n" },
		func() string { return fmt.Sprintf("\x1b[%d;%dH", n(rows+2), n(cols+2)) },
		func() string { return fmt.Sprintf("\x1b[%d%c", n(4), "ABCDEFGIZadeb@PXLMSTJK"[n(22)]) },
		func() string { return fmt.Sprintf("\x1b[%d;%dr", n(rows+1), n(rows+1)) },
		func() string {
			return []string{"\x1b[?6h", "\x1b[?6l", "\x1b[?7h", "\x1b[?7l", "\x1b[4h", "\x1b[4l", "\x1b[20h", "\x1b[20l"}[n(8)]
		},
		func() string {
			return []string{"\x1b7", "\x1b8", "\x1b[s", "\x1b[u", "\x1b[?1048h", "\x1b[?1048l"}[n(6)]
		},
		func() string {
			return []string{"\x1b[?1049h", "\x1b[?1049l", "\x1b[?47h", "\x1b[?47l", "\x1b[?1047h", "\x1b[?1047l"}[n(6)]
		},
		func() string {
			return []string{"\x1bD", "\x1bE", "\x1bM", "\x1bH", "\x1b[g", "\x1b[3g", "\t", "\b"}[n(8)]
		},
		func() string { return []string{"\x1b(0", "\x1b(B", "\x0e", "\x0f", "\x1b)0"}[n(5)] },
		func() string { return []string{"\x1b[1;4:3;38;2;1;2;3m", "\x1b[0m", "\x1b[48;5;9m", "\x1b[7m"}[n(4)] },
		func() string { return []string{"\x1b]8;;https://example.com\x07", "\x1b]8;;\x07"}[n(2)] },
		func() string { return []string{"\x1b[?2026h", "\x1b[?2026l", "\x1b[!p", "\x1b[?6s", "\x1b[?6r"}[n(5)] },
		func() string { return fmt.Sprintf("\x1b[%d;%dHyy", rows, cols-n(3)) },
		func() string { return fmt.Sprintf("\x1b[%d;1H\x1b[2K", rows) },
	}
	return tokens[n(len(tokens))]()
}

// TestMarkIsInvisibleToTheModel feeds what a Writer sends into a second
// model: everything the output set, the mark aside, must come out the same.
func TestMarkIsInvisibleToTheModel(t *testing.T) {
	for seed := range uint64(500) {
		rng := rand.New(rand.NewPCG(seed, 11))
		cols, rows := 6+rng.IntN(20), 2+rng.IntN(8)
		row, col := 1+rng.IntN(rows), 1+rng.IntN(cols)
		shadow := New(io.Discard, cols, rows, t.Logf)
		shadow.At(row, col)
		w := New(shadow, cols, rows, t.Logf)
		w.At(row, col)
		w.Show()
		var sent strings.Builder
		for range 200 {
			s := edge(rng, cols, rows)
			sent.WriteString(s)
			for len(s) > 0 {
				n := min(len(s), 1+rng.IntN(8))
				if _, err := w.Write([]byte(s[:n])); err != nil {
					t.Fatal(err)
				}
				s = s[n:]
			}
			if diff := sameAsShadow(&w.t, &shadow.t); diff != "" {
				t.Fatalf("seed %d %dx%d at %d,%d: %s\nsent: %q", seed, cols, rows, row, col, diff, tail(sent.String(), 600))
			}
		}
	}
}

// sameAsShadow compares what the output set in two models, leaving out
// the corners, which only one of them saw marked.
func sameAsShadow(a, b *term) string {
	type modes struct {
		alt, origin, autowrap, insert, newline, reverseWrap, reverseWrapExt, leftRight, margins, allow132 bool
		top, bottom                                                                                       int
	}
	ma := modes{a.alt, a.origin, a.autowrap, a.insert, a.newline, a.reverseWrap, a.reverseWrapExt, a.leftRight, a.margins, a.allow132, a.top, a.bottom}
	mb := modes{b.alt, b.origin, b.autowrap, b.insert, b.newline, b.reverseWrap, b.reverseWrapExt, b.leftRight, b.margins, b.allow132, b.top, b.bottom}
	if ma != mb {
		return fmt.Sprintf("modes %+v, shadow %+v", ma, mb)
	}
	for i := range a.screens {
		sa, sb := &a.screens[i], &b.screens[i]
		if sa.cur != sb.cur {
			return fmt.Sprintf("screen %d cursor %+v, shadow %+v", i, sa.cur, sb.cur)
		}
		if sa.link != sb.link {
			return fmt.Sprintf("screen %d link %q, shadow %q", i, sa.link, sb.link)
		}
		if (sa.saved == nil) != (sb.saved == nil) || sa.saved != nil && *sa.saved != *sb.saved {
			return fmt.Sprintf("screen %d saved %+v, shadow %+v", i, sa.saved, sb.saved)
		}
	}
	return ""
}
