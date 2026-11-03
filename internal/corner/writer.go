// Package corner keeps a mark in the bottom-right corner of a terminal while
// passing output on to it. It follows what the output does to the cursor and
// to the last two cells of the last row, and draws the mark only where it can
// go to the corner and come back exactly, without the save-cursor slot the
// output may be using. The mark goes before any output that would scroll or
// shift it, so it never leaves copies, and it only covers blank cells.
//
// The terminal is taken to start in its default modes on its primary
// screen, as a shell prompt leaves it.
package corner

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
)

// Mark is the cloud emoji, which terminals draw two cells wide.
const Mark = "\u2601\ufe0f"

// MarkCells is how many cells at the end of the last row the mark takes.
const MarkCells = 2

// holdLimit bounds an unfinished escape sequence held back between writes.
const holdLimit = 4096

// maxParams is the most parameters Ghostty takes; it drops longer sequences.
const maxParams = 24

// Writer passes output to a terminal and keeps the mark in its corner.
type Writer struct {
	mu   sync.Mutex
	out  io.Writer
	logf func(format string, args ...any)

	parser *ansi.Parser
	t      term
	// pending is the sequence being parsed. Until it is complete it is held
	// back, so the mark can still be taken away before it.
	pending []byte
	// sealed reports that part of pending was written already.
	sealed bool
	// stringEnded reports that the last byte ended an OSC, DCS or similar string.
	stringEnded bool
	// buf collects the bytes of one write.
	buf []byte

	shown, closed bool
	// marked reports that the last character the terminal printed is the
	// mark or a notice's, not the output's, which matters to REP.
	marked     bool
	savedModes map[int]bool
	reported   map[string]bool
}

// New writes to out, a terminal of the given size.
func New(out io.Writer, cols, rows int, logf func(format string, args ...any)) *Writer {
	w := &Writer{out: out, logf: logf, t: newTerm(cols, rows), savedModes: map[int]bool{}, reported: map[string]bool{}}
	w.parser = ansi.NewParser()
	w.parser.SetHandler(ansi.Handler{
		Print:     w.onPrint,
		Execute:   w.onExecute,
		HandleCsi: w.onCsi,
		HandleEsc: w.onEsc,
		HandleOsc: w.onOsc,
		HandleDcs: func(ansi.Cmd, ansi.Params, []byte) { w.stringEnded = true },
		HandleApc: func([]byte) { w.stringEnded = true },
		HandlePm:  func([]byte) { w.stringEnded = true },
		HandleSos: func([]byte) { w.stringEnded = true },
	})
	return w
}

// At tells where the terminal's cursor is, counting from 1, as it reported
// before any output. The corner lies below the cursor, so nothing there is
// the shell's yet; it is cleared, to be known blank.
func (w *Writer) At(row, col int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	c := w.t.cur()
	c.row, c.rowKnown = min(max(row, 1), w.t.rows)-1, true
	c.setCol(min(max(col, 1), w.t.cols) - 1)
	if c.row == w.t.rows-1 || w.t.cols < 3 {
		return nil
	}
	return w.send([]byte(cup(w.t.rows, w.t.cols-1) + "\x1b[0m\x1b[2X" + cup(row, col)))
}

// Show starts keeping the mark in the corner, from the next write on.
func (w *Writer) Show() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.shown = true
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.out.Write(p)
	}
	if err := w.send(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// BottomLine writes text, printable ASCII, over the bottom row, clear of the
// mark, and puts the cursor back. It reports false, writing nothing, when
// the cursor is not known well enough to come back.
func (w *Writer) BottomLine(text string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || len(w.pending) > 0 || !w.t.drawable() {
		return false, nil
	}
	text = text[:min(len(text), max(w.t.cols-MarkCells, 0))]
	r := w.t.restorer()
	last, certain, has := w.t.last, w.t.lastCertain, w.t.hasLast
	err := w.send([]byte(r.away() + r.asciiAway() + "\x1b[0m" + cup(w.t.rows, 1) + "\x1b[2K" + r.noWrap(text) + r.asciiBack() + r.back()))
	w.t.last, w.t.lastCertain, w.t.hasLast = last, certain, has
	w.marked = w.marked || text != ""
	return true, err
}

// Resize follows the terminal to a new size. The terminal has already
// reflowed what it shows, a drawn mark too, to where the model cannot follow,
// so the mark comes back only once the output has placed the cursor and
// erased the new corner.
func (w *Writer) Resize(cols, rows int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cols == w.t.cols && rows == w.t.rows {
		return
	}
	w.t.resize(cols, rows)
}

// Close takes the mark away for good; later writes pass straight through.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if len(w.pending) > 0 {
		// An agent that ends mid-sequence must not swallow what is written after it.
		w.report("unfinished", "dropping an unfinished escape sequence at the end: %q", w.pending)
		w.pending = w.pending[:0]
	}
	scr := w.t.active()
	if !scr.drawn {
		return nil
	}
	scr.drawn = false
	_, err := io.WriteString(w.out, w.t.restorer().hide())
	return err
}

// send passes p through the model and on to the terminal, the mark kept where it can be.
func (w *Writer) send(p []byte) error {
	w.buf = w.buf[:0]
	for _, b := range p {
		w.feed(b)
	}
	switch st := w.parser.State(); {
	case st == parser.GroundState:
		if !w.t.sync {
			w.draw()
		}
	case isString(st):
		w.flushPending()
	case len(w.pending) > holdLimit:
		w.report("long", "an escape sequence longer than %d bytes went out unfinished", holdLimit)
		w.flushPending()
	}
	if len(w.buf) == 0 {
		return nil
	}
	_, err := w.out.Write(w.buf)
	return err
}

func isString(st parser.State) bool {
	switch st {
	case parser.OscStringState, parser.DcsStringState, parser.SosStringState, parser.PmStringState, parser.ApcStringState:
		return true
	}
	return false
}

func (w *Writer) flushPending() {
	w.buf = append(w.buf, w.pending...)
	w.pending = w.pending[:0]
	w.sealed = true
}

func (w *Writer) feed(b byte) {
	from := w.parser.State()
	w.pending = append(w.pending, b)
	w.stringEnded = false
	w.parser.Advance(b)
	st := w.parser.State()
	if from == parser.GroundState && b >= 0x80 && st != parser.GroundState && st != parser.Utf8State {
		// Ghostty decodes UTF-8 first: a lone C1 byte prints a replacement character.
		w.parser.Reset()
		w.c1()
		st = parser.GroundState
	}
	switch {
	case st == parser.GroundState:
		w.buf = append(w.buf, w.pending...)
		w.pending, w.sealed = w.pending[:0], false
	case w.stringEnded && st == parser.EscapeState:
		// The ESC that ended the string may start the next sequence.
		w.buf = append(w.buf, w.pending[:len(w.pending)-1]...)
		w.pending, w.sealed = append(w.pending[:0], 0x1b), false
	}
}

// insert puts s before the sequence being parsed.
func (w *Writer) insert(s string) {
	if w.sealed {
		w.report("sealed", "the mark could not be taken away before a sequence already partly sent")
		return
	}
	w.buf = append(w.buf, s...)
}

// draw puts the mark in the corner when it is blank and the cursor can come back.
func (w *Writer) draw() {
	scr := w.t.active()
	if !w.shown || scr.drawn || !w.t.drawable() {
		return
	}
	if c := scr.corner; c[0].state != cellBlank || c[1] != c[0] {
		return
	}
	w.insert(w.t.restorer().show())
	scr.drawn = !w.sealed
	w.marked = w.marked || scr.drawn
}

// before and after wrap one operation on the model: when it would move the
// mark, or leave the cursor where it cannot be found again, the mark goes
// before the operation's bytes; when the operation overwrites it, it is gone.
type before struct {
	scr   *screen
	drawn bool
	r     restorer
}

func (w *Writer) before() before {
	scr := w.t.active()
	if !scr.drawn {
		return before{scr: scr}
	}
	return before{scr: scr, drawn: true, r: w.t.restorer()}
}

func (w *Writer) after(b before, fx effect) {
	if !b.drawn {
		return
	}
	switch {
	case fx == moved || w.t.active() != b.scr || !w.t.drawable():
		w.insert(b.r.hide())
		b.scr.drawn = false
	case fx == erased:
		b.scr.drawn = false
	}
}

// lost is output the model does not follow: the cursor and the corner are no longer known.
func (w *Writer) lost(what string) {
	b := w.before()
	w.t.lose()
	w.after(b, w.t.lostCorner())
	w.report(what, "%s leaves the cursor unknown", what)
}

func (w *Writer) c1() {
	b := w.before()
	w.after(b, w.t.print(utf8.RuneError))
}

func (w *Writer) onPrint(r rune) {
	if r == utf8.RuneError && !utf8.Valid(w.pending) {
		// Ghostty prints a replacement character and reads the bytes after the bad one again.
		w.lost(fmt.Sprintf("invalid UTF-8 %q", w.pending))
		return
	}
	w.marked = false
	b := w.before()
	w.after(b, w.t.print(r))
}

func (w *Writer) onExecute(c byte) {
	b := w.before()
	fx := untouched
	switch c {
	case 0x08:
		w.t.cursorLeft(1)
	case 0x09:
		w.t.tab(1)
	case 0x0a, 0x0b, 0x0c:
		fx = w.t.linefeed()
	case 0x0d:
		w.t.carriageReturn()
	case 0x0e:
		w.t.cur().charset.gl = 1
	case 0x0f:
		w.t.cur().charset.gl = 0
	default:
		if c >= 0x80 {
			fx = w.t.print(utf8.RuneError)
		}
	}
	w.after(b, fx)
}

func (w *Writer) onEsc(cmd ansi.Cmd) {
	b := w.before()
	fx := untouched
	t := &w.t
	switch inter, final := cmd.Intermediate(), cmd.Final(); inter {
	case 0:
		switch final {
		case '7':
			t.saveCursor()
		case '8':
			t.restoreCursor()
		case 'D':
			fx = t.index()
		case 'E':
			fx = t.index()
			t.carriageReturn()
		case 'M':
			fx = t.reverseIndex()
		case 'H':
			t.setTab(true)
		case 'c':
			t.fullReset()
			w.savedModes, w.marked = map[int]bool{}, false
			fx = erased
		case 'n':
			t.cur().charset.gl = 2
		case 'o':
			t.cur().charset.gl = 3
		case '6', '9':
			fx = w.sideIndex(final == '9')
		case 'N', 'O':
			t.shifted = true
		case '=', '>', '\\', 'V', 'W', 'Z', '~', '}', '|':
		default:
			w.ignored(fmt.Sprintf("ESC %c", final))
		}
	case '#':
		if final != '8' {
			w.ignored(fmt.Sprintf("ESC # %c", final))
			break
		}
		fx = t.alignment()
	case '(', ')', '*', '+', '-', '.', '/':
		if final == 'B' || final == 'A' || final == '0' {
			slot := map[byte]int{'(': 0, ')': 1, '*': 2, '+': 3, '-': 1, '.': 2, '/': 3}[inter]
			t.cur().charset.g[slot] = final
		}
	case ' ', '%':
	default:
		w.ignored(fmt.Sprintf("ESC %c %c", inter, final))
	}
	w.after(b, fx)
}

// ignored is a sequence Ghostty ignores, and so does the model.
func (w *Writer) ignored(what string) {
	w.report(what, "ignoring unknown %s", what)
}

// sideIndex is DECFI (forward) or DECBI: a step sideways, or at the edge a
// shift of the region's columns, which the model does not follow.
func (w *Writer) sideIndex(forward bool) effect {
	c := w.t.cur()
	switch {
	case forward && c.exact() && c.lo < w.t.cols-1:
		w.t.cursorRight(1)
		return untouched
	case !forward && c.exact() && c.lo > 0:
		w.t.cursorLeft(1)
		return untouched
	}
	w.t.lose()
	return w.t.lostCorner()
}

func (w *Writer) onOsc(cmd int, data []byte) {
	w.stringEnded = true
	if cmd != 8 {
		return
	}
	// data is "8;params;uri"; an empty uri closes the link.
	rest := data[min(2, len(data)):]
	for i, c := range rest {
		if c == ';' {
			if i+1 == len(rest) {
				w.t.active().link = ""
			} else {
				w.t.active().link = "\x1b]" + string(data) + "\x1b\\"
			}
			return
		}
	}
}

// csiParams is the parameters as Ghostty reads them: a missing one is 0,
// except a missing last one, which is dropped. It reports false for a
// sequence Ghostty drops: too many parameters, or colons outside SGR.
func csiParams(params ansi.Params, final byte) (ansi.Params, bool) {
	n := len(params)
	if n > 0 && params[n-1].Param(-1) == -1 {
		n--
	}
	params = params[:n]
	if n > maxParams {
		return nil, false
	}
	if final != 'm' {
		for _, p := range params {
			if p.HasMore() {
				return nil, false
			}
		}
	}
	return params, true
}

// one is the single parameter of a sequence that takes at most one.
func one(params ansi.Params, def int) (int, bool) {
	switch len(params) {
	case 0:
		return def, true
	case 1:
		return params[0].Param(0), true
	}
	return 0, false
}

func (w *Writer) onCsi(cmd ansi.Cmd, raw ansi.Params) {
	prefix, inter, final := cmd.Prefix(), cmd.Intermediate(), cmd.Final()
	params, ok := csiParams(raw, final)
	if !ok {
		return
	}
	if prefix == '?' && final == 'l' && inter == 0 && hasParam(params, 2026) && w.t.sync {
		// The agent's frame ends here: the mark goes in with it.
		w.draw()
	}
	if prefix == 0 && inter == 0 && final == 'b' && w.marked {
		if n, single := one(params, 1); single {
			w.repeatOutput(max(n, 1))
			return
		}
	}
	b := w.before()
	fx, known := w.csi(prefix, inter, final, params)
	w.after(b, fx)
	if !known {
		w.ignored(fmt.Sprintf("CSI %c%c%c", nonzero(prefix), nonzero(inter), final))
	}
}

// repeatOutput is REP after the terminal last printed the mark or a
// notice, which it would repeat: the output's own character goes out n
// times instead, or nothing when the output has printed none.
func (w *Writer) repeatOutput(n int) {
	if !w.t.hasLast {
		w.pending = w.pending[:0]
		return
	}
	last := w.t.last
	b := w.before()
	fx := untouched
	for range n {
		fx = worst(fx, w.t.print(last))
	}
	w.after(b, fx)
	w.pending = append(w.pending[:0], strings.Repeat(string(last), n)...)
	w.marked = false
}

func nonzero(c byte) rune {
	if c == 0 {
		return ' '
	}
	return rune(c)
}

func hasParam(params ansi.Params, v int) bool {
	for _, p := range params {
		if p.Param(0) == v {
			return true
		}
	}
	return false
}

// csi applies one CSI sequence; it reports false for one Ghostty ignores.
func (w *Writer) csi(prefix, inter, final byte, params ansi.Params) (effect, bool) {
	t := &w.t
	switch {
	case prefix == '>' || prefix == '=' || prefix == '<':
		// Keyboard protocols and queries.
		return untouched, true
	case prefix == '?':
		return w.csiPrivate(inter, final, params)
	case inter != 0:
		switch {
		case inter == '!' && final == 'p':
			if len(params) == 0 {
				t.softReset()
			}
		case inter == ' ' && final == 'q', inter == '"' && final == 'q', inter == '$' && final == 'p':
		case inter == '\'' && (final == '}' || final == '~'), inter == ' ' && (final == '@' || final == 'A'):
			// Columns inserted, deleted or shifted: the cursor stays, the corner's cells move.
			return t.lostCorner(), true
		default:
			return untouched, false
		}
		return untouched, true
	}
	n, single := one(params, 1)
	switch final {
	case 'A', 'k', 'B', 'C', 'D', 'j', 'E', 'F', 'G', '`', 'I', 'L', 'M', 'P', 'S', 'T', 'X', 'Z', 'a', 'b', 'd', 'e', '@':
		if !single {
			return untouched, true
		}
	}
	switch final {
	case 'A', 'k':
		t.cursorUp(n)
	case 'B':
		t.cursorDown(n)
	case 'C':
		t.cursorRight(n)
	case 'D', 'j':
		t.cursorLeft(n)
	case 'E':
		t.cursorDown(n)
		t.carriageReturn()
	case 'F':
		t.cursorUp(n)
		t.carriageReturn()
	case 'G', '`':
		t.setCursorCol(n)
	case 'H', 'f':
		switch len(params) {
		case 0:
			t.setCursorPos(1, 1)
		case 1:
			t.setCursorPos(params[0].Param(0), 1)
		case 2:
			t.setCursorPos(params[0].Param(0), params[1].Param(0))
		}
	case 'I':
		t.tab(n)
	case 'J':
		mode, single := one(params, 0)
		if single {
			return t.eraseDisplay(mode), true
		}
	case 'K':
		if mode, single := one(params, 0); single && mode < 3 {
			return t.eraseLine(mode), true
		}
	case 'L':
		return t.lines(n, true), true
	case 'M':
		return t.lines(n, false), true
	case 'P':
		return t.deleteChars(n), true
	case 'S':
		return t.scrollRegion(n, true), true
	case 'T':
		return t.scrollRegion(n, false), true
	case 'W':
		switch mode, _ := one(params, 0); {
		case len(params) > 1:
		case mode == 0:
			t.setTab(true)
		case mode == 2:
			t.setTab(false)
		case mode == 5:
			t.clearTabs()
		}
	case 'X':
		return t.eraseChars(n), true
	case 'Z':
		t.tabBack(n)
	case 'a':
		t.cursorColRelative(n)
	case 'b':
		return t.repeat(n), true
	case 'd':
		t.setCursorRow(n)
	case 'e':
		t.cursorRowRelative(n)
	case 'g':
		switch mode, single := one(params, 0); {
		case !single:
		case mode == 0:
			t.setTab(false)
		case mode == 3:
			t.clearTabs()
		}
	case 'h', 'l':
		for _, p := range params {
			switch p.Param(0) {
			case 4:
				t.insert = final == 'h'
			case 20:
				t.newline = final == 'h'
			}
		}
	case 'm':
		t.cur().pen.apply(params)
	case 'r':
		switch len(params) {
		case 0:
			t.setTopBottom(0, 0)
		case 1:
			t.setTopBottom(params[0].Param(0), 0)
		case 2:
			t.setTopBottom(params[0].Param(0), params[1].Param(0))
		}
	case 's':
		switch {
		case len(params) == 0 && !t.leftRight:
			t.saveCursor()
		case len(params) == 0:
			t.leftRightMargins(0, 0)
		case len(params) == 1:
			t.leftRightMargins(params[0].Param(0), 0)
		case len(params) == 2:
			t.leftRightMargins(params[0].Param(0), params[1].Param(0))
		}
	case 'u':
		t.restoreCursor()
	case '@':
		return t.insertBlanks(n), true
	case 'c', 'n', 'q', 't', 'i', 'x', 'y':
		// Reports, LEDs and window operations.
	default:
		return untouched, false
	}
	return untouched, true
}

func (w *Writer) csiPrivate(inter, final byte, params ansi.Params) (effect, bool) {
	t := &w.t
	if inter != 0 {
		return untouched, inter == '$' && final == 'p'
	}
	switch final {
	case 'h', 'l':
		fx := untouched
		for _, p := range params {
			fx = worst(fx, w.decMode(p.Param(0), final == 'h'))
		}
		return fx, true
	case 'J':
		if mode, single := one(params, 0); single {
			return t.eraseDisplay(mode), true
		}
	case 'K':
		if mode, single := one(params, 0); single && mode < 3 {
			return t.eraseLine(mode), true
		}
	case 'W':
		if len(params) == 1 && params[0].Param(0) == 5 {
			t.resetTabs()
		}
	case 's':
		for _, p := range params {
			if on, ok := w.decModeValue(p.Param(0)); ok {
				w.savedModes[p.Param(0)] = on
			}
		}
	case 'r':
		fx := untouched
		for _, p := range params {
			mode := p.Param(0)
			on, ok := w.savedModes[mode]
			if _, followed := w.decModeValue(mode); followed {
				// Ghostty restores a mode never saved to off.
				fx = worst(fx, w.decMode(mode, ok && on))
			}
		}
		return fx, true
	}
	// Queries, graphics and keyboard settings.
	return untouched, true
}

// decModeValue is the current value of a DEC mode the model follows.
func (w *Writer) decModeValue(mode int) (bool, bool) {
	t := &w.t
	switch mode {
	case 6:
		return t.origin, true
	case 7:
		return t.autowrap, true
	case 40:
		return t.allow132, true
	case 45:
		return t.reverseWrap, true
	case 1045:
		return t.reverseWrapExt, true
	case 47, 1047, 1049:
		return t.alt, true
	case 69:
		return t.leftRight, true
	case 2026:
		return t.sync, true
	}
	return false, false
}

func (w *Writer) decMode(mode int, on bool) effect {
	t := &w.t
	switch mode {
	case 3:
		if t.allow132 {
			// Ghostty changes the width and clears the screen.
			t.lose()
			return t.lostCorner()
		}
	case 6:
		t.origin = on
		t.setCursorPos(1, 1)
	case 7:
		t.autowrap = on
	case 40:
		t.allow132 = on
	case 45:
		t.reverseWrap = on
	case 1045:
		t.reverseWrapExt = on
	case 47, 1047, 1049:
		return t.altScreen(mode, on)
	case 1048:
		if on {
			t.saveCursor()
		} else {
			t.restoreCursor()
		}
	case 69:
		t.leftRight = on
		if !on {
			t.margins = false
		}
	case 2026:
		t.sync = on
	}
	return untouched
}

func (w *Writer) report(key, format string, args ...any) {
	if w.reported[key] || len(w.reported) >= 32 {
		return
	}
	w.reported[key] = true
	w.logf("corner: "+format, args...)
}

func cup(row, col int) string { return "\x1b[" + strconv.Itoa(row) + ";" + strconv.Itoa(col) + "H" }

// restorer holds what it takes to leave the cursor and come back to it.
type restorer struct {
	cols, rows     int
	row, col, top  int
	pen            pen
	charset        charset
	origin, insert, autowrap bool
	link                     string
	// bg is the background of the blank corner cells.
	bg string
}

func (t *term) restorer() restorer {
	scr := t.active()
	c := scr.cur
	return restorer{
		cols: t.cols, rows: t.rows, row: c.row, col: c.lo, top: t.top,
		pen: c.pen, charset: c.charset, origin: t.origin, insert: t.insert, autowrap: t.autowrap,
		link: scr.link, bg: scr.corner[0].bg,
	}
}

// away leaves the modes that would change what is drawn at the corner or how the cursor gets there.
func (r restorer) away() string {
	s := ""
	if r.link != "" {
		s += "\x1b]8;;\x1b\\"
	}
	if r.insert {
		s += "\x1b[4l"
	}
	if r.origin {
		s += "\x1b[?6l"
	}
	return s
}

func (r restorer) back() string {
	s := cup(r.row+1, r.col+1)
	if r.origin {
		s = "\x1b[?6h" + cup(r.row-r.top+1, r.col+1)
	}
	s += r.pen.sgr()
	if r.insert {
		s += "\x1b[4h"
	}
	return s + r.link
}

// asciiAway and asciiBack make GL print ASCII as itself, and undo that.
func (r restorer) asciiAway() string {
	if r.charset.ascii() {
		return ""
	}
	return "\x1b(B\x0f"
}

func (r restorer) asciiBack() string {
	if r.charset.ascii() {
		return ""
	}
	s := ""
	if g0 := r.charset.g[0]; g0 != 0 {
		s += "\x1b(" + string(g0)
	}
	return s + [...]string{"\x0f", "\x0e", "\x1bn", "\x1bo"}[r.charset.gl]
}

// noWrap prints body with autowrap off, so a terminal that shrank before
// the resize reaches the model drops what no longer fits instead of
// wrapping and scrolling.
func (r restorer) noWrap(body string) string {
	if !r.autowrap {
		return body
	}
	return "\x1b[?7l" + body + "\x1b[?7h"
}

func (r restorer) corner(body string) string {
	return r.away() + blankSGR(r.bg) + cup(r.rows, r.cols-1) + body + r.back()
}

func (r restorer) show() string { return r.corner(r.noWrap(Mark)) }

// hide erases the mark the way the corner was blank before it.
func (r restorer) hide() string { return r.corner("\x1b[2X") }
