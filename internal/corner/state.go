package corner

// The model follows Ghostty's handling of each sequence (src/terminal in
// ghostty-org/ghostty), as far as it moves the cursor or changes the last
// two cells of the last row. Where the output leaves something unknown, the
// model says so rather than guess.

// cellState is what the output last left in one corner cell.
type cellState uint8

const (
	cellUnknown cellState = iota
	cellBlank
	cellFilled
)

type cell struct {
	state cellState
	// bg is the background a blank cell was erased with, as SGR parameters.
	bg string
}

func blank(bg string) cell { return cell{state: cellBlank, bg: bg} }

// effect is what an operation did to the corner of the active screen.
type effect uint8

const (
	untouched effect = iota
	// erased: the corner cells were overwritten, so a mark there is gone.
	erased
	// moved: the corner's content moved or was joined to, so a mark there must go first.
	moved
)

func worst(a, b effect) effect { return max(a, b) }

// cursor is where the next character goes, as far as the output tells.
type cursor struct {
	row      int
	rowKnown bool
	// The column lies in [lo, hi].
	lo, hi int
	// wrap is Ghostty's pending wrap; it means something only while the column is known.
	wrap    bool
	pen     pen
	charset charset
}

func (c *cursor) exact() bool { return c.rowKnown && c.lo == c.hi }

func (c *cursor) setCol(x int) { c.lo, c.hi, c.wrap = x, x, false }

// charset is the character sets designated and shifted in, as Ghostty keeps them.
type charset struct {
	// g holds each set's final byte; zero is ASCII.
	g  [4]byte
	gl int
}

// ascii reports whether GL prints ASCII as itself.
func (c charset) ascii() bool { return c.g[c.gl] == 0 || c.g[c.gl] == 'B' }

type savedCursor struct {
	cur    cursor
	origin bool
}

type screen struct {
	cur   cursor
	saved *savedCursor
	// link is the OSC 8 sequence of the open hyperlink, "" when none is.
	link   string
	corner [2]cell
	// drawn reports that the mark is over corner.
	drawn bool
}

type term struct {
	cols, rows int
	screens    [2]screen
	alt        bool
	// The scroll region's first and last rows.
	top, bottom int

	origin, autowrap, insert, newline bool
	reverseWrap, reverseWrapExt       bool
	// leftRight is DECLRMM; margins reports left and right margins set, which the model does not follow.
	leftRight, margins bool
	allow132           bool
	sync               bool

	tabs []bool
	// tabsUnknown: a tab stop changed at a column the model did not know.
	tabsUnknown bool

	// last is the character REP repeats; lastCertain reports whether its width was certain.
	last        rune
	lastCertain bool
	hasLast     bool
	// afterUncertain: the last character printed may still be joined by the next.
	afterUncertain bool
	// shifted: a single shift waits for the next character, which may be the mark.
	shifted bool
}

func newTerm(cols, rows int) term {
	t := term{cols: cols, rows: rows, autowrap: true}
	t.resetRegion()
	t.resetTabs()
	for i := range t.screens {
		t.screens[i].cur = cursor{hi: cols - 1}
	}
	return t
}

func (t *term) active() *screen {
	if t.alt {
		return &t.screens[1]
	}
	return &t.screens[0]
}

func (t *term) cur() *cursor { return &t.active().cur }

func (t *term) resetRegion() { t.top, t.bottom = 0, t.rows-1 }

func (t *term) resetTabs() {
	t.tabs = make([]bool, t.cols)
	for x := 8; x < t.cols; x += 8 {
		t.tabs[x] = true
	}
	t.tabsUnknown = false
}

// lose forgets where the cursor is.
func (t *term) lose() {
	c := t.cur()
	c.rowKnown, c.lo, c.hi, c.wrap = false, 0, t.cols-1, false
}

// lostCorner forgets the corner after an operation the model does not follow there.
func (t *term) lostCorner() effect {
	t.active().corner = [2]cell{}
	return moved
}

// drawable reports whether the cursor is known well enough to leave it and come back.
func (t *term) drawable() bool {
	c := t.cur()
	return t.cols >= 3 && c.exact() && !c.wrap && !t.margins && !t.shifted &&
		!(t.origin && (c.row < t.top || c.row > t.bottom))
}

// cells records that the output set cells [from, to] of the cursor's row to
// after. A wide character the range cuts in half loses its other half.
func (t *term) cells(from, to int, after cell) effect {
	c := t.cur()
	first := t.cols - 2
	if to < first-1 || c.rowKnown && c.row != t.rows-1 {
		return untouched
	}
	if !c.rowKnown {
		return t.lostCorner()
	}
	scr := t.active()
	fx := untouched
	for i := range scr.corner {
		x := first + i
		switch {
		case x >= from && x <= to:
			scr.corner[i] = after
			fx = erased
		case (x == from-1 || x == to+1) && scr.corner[i].state == cellFilled:
			scr.corner[i] = cell{}
			fx = erased
		}
	}
	return fx
}

// scrollUp is content moving up in the scroll region, with new rows of the
// pen's background at its bottom.
func (t *term) scrollUp() effect {
	if t.bottom != t.rows-1 {
		return untouched
	}
	bg := t.cur().pen.bg
	t.active().corner = [2]cell{blank(bg), blank(bg)}
	return moved
}

// scrollDown is content moving down in the scroll region: the last row's
// content leaves, and what takes its place the model does not know.
func (t *term) scrollDown() effect {
	if t.bottom != t.rows-1 {
		return untouched
	}
	t.active().corner = [2]cell{}
	return erased
}

// shiftRow is characters inserted or deleted on the cursor's row.
func (t *term) shiftRow() effect {
	c := t.cur()
	if c.rowKnown && c.row != t.rows-1 {
		return untouched
	}
	return t.lostCorner()
}

func (t *term) print(r rune) effect {
	t.shifted = false
	w, certain := width(r)
	if r >= 0x80 && t.afterUncertain {
		certain = false
	}
	if !certain {
		// Ghostty repeats only a character it gave a cell of its own.
		t.last, t.lastCertain, t.hasLast = r, false, true
		t.afterUncertain = true
		return t.printUncertain()
	}
	t.afterUncertain = false
	if w == 0 {
		return t.printJoined()
	}
	t.last, t.lastCertain, t.hasLast = r, true, true
	return t.printCell(w)
}

// printJoined is a zero-width character, which Ghostty adds to the cell left
// of the cursor, or under it at a pending wrap, and drops after a blank one.
func (t *term) printJoined() effect {
	c := t.cur()
	if !c.exact() {
		return untouched
	}
	x := c.lo - 1
	if c.wrap && t.autowrap {
		x = c.lo
	}
	if c.row == t.rows-1 && x >= t.cols-2 {
		// Joined to the mark it would change it.
		return moved
	}
	return untouched
}

// printUncertain is a character terminals may print in up to two cells, or
// join to the one left of the cursor, widening or narrowing it by one.
func (t *term) printUncertain() effect {
	c := t.cur()
	fx := untouched
	onLast := !c.rowKnown || c.row == t.rows-1
	if onLast && (t.insert || c.hi+1 >= t.cols-3) {
		fx = t.lostCorner()
	}
	if c.hi+2 >= t.cols {
		// It may reach the last column or wrap, and wrapping may scroll.
		if (!c.rowKnown || c.row == t.bottom) && t.bottom == t.rows-1 {
			fx = t.lostCorner()
		}
		t.lose()
		return fx
	}
	c.lo, c.hi, c.wrap = max(c.lo-1, 0), c.hi+2, false
	return fx
}

// printCell prints a character of width w at the cursor.
func (t *term) printCell(w int) effect {
	c := t.cur()
	if c.lo != c.hi {
		return t.printSomewhere(w)
	}
	fx := untouched
	if c.wrap {
		if t.autowrap {
			fx = t.printWrap()
		} else {
			c.wrap = false
		}
	}
	x := c.lo
	if t.insert && x+w < t.cols {
		fx = worst(fx, t.shiftRow())
	}
	if w == 2 && x == t.cols-1 {
		if !t.autowrap {
			return fx
		}
		// Ghostty leaves a spacer in the last column and prints on the next line.
		fx = worst(fx, t.cells(x, x, cell{state: cellFilled}))
		fx = worst(fx, t.printWrap())
		x = c.lo
	}
	fx = worst(fx, t.cells(x, x+w-1, cell{state: cellFilled}))
	if end := x + w - 1; end == t.cols-1 {
		c.setCol(end)
		c.wrap = true
	} else {
		c.setCol(x + w)
	}
	return fx
}

// printSomewhere prints a character of width w at a column the model only
// bounds: it lands in [lo, hi+w-1].
func (t *term) printSomewhere(w int) effect {
	c := t.cur()
	fx := untouched
	onLast := !c.rowKnown || c.row == t.rows-1
	if onLast && (t.insert || c.hi+w-1 >= t.cols-3) {
		fx = t.lostCorner()
	}
	if c.hi+w >= t.cols {
		// It may reach the last column or wrap, and wrapping may scroll.
		if (!c.rowKnown || c.row == t.bottom) && t.bottom == t.rows-1 {
			fx = t.lostCorner()
		}
		t.lose()
		return fx
	}
	c.lo, c.hi = c.lo+w, c.hi+w
	return fx
}

func (t *term) printWrap() effect {
	fx := t.index()
	t.cur().setCol(0)
	return fx
}

// index moves the cursor down a row, scrolling at the bottom of the scroll region.
func (t *term) index() effect {
	c := t.cur()
	c.wrap = false
	t.afterUncertain = false
	switch {
	case !c.rowKnown:
		if t.bottom == t.rows-1 {
			return t.lostCorner()
		}
		return untouched
	case c.row < t.top || c.row > t.bottom:
		if c.row < t.rows-1 {
			c.row++
		}
		return untouched
	case c.row == t.bottom:
		return t.scrollUp()
	}
	c.row++
	return untouched
}

func (t *term) linefeed() effect {
	fx := t.index()
	if t.newline {
		t.carriageReturn()
	}
	return fx
}

func (t *term) carriageReturn() {
	t.cur().setCol(0)
	t.afterUncertain = false
}

func (t *term) reverseIndex() effect {
	c := t.cur()
	switch {
	case !c.rowKnown:
		// It may scroll down, and the cursor stays wherever it was.
		c.wrap = false
		return t.scrollDown()
	case c.row != t.top:
		t.cursorUp(1)
		return untouched
	}
	return t.scrollDown()
}

func (t *term) cursorUp(n int) {
	c := t.cur()
	c.wrap = false
	t.afterUncertain = false
	if !c.rowKnown {
		return
	}
	limit := 0
	if c.row >= t.top {
		limit = t.top
	}
	c.row = max(c.row-max(n, 1), limit)
}

func (t *term) cursorDown(n int) {
	c := t.cur()
	c.wrap = false
	t.afterUncertain = false
	if !c.rowKnown {
		return
	}
	limit := t.rows - 1
	if c.row <= t.bottom {
		limit = t.bottom
	}
	c.row = min(c.row+max(n, 1), limit)
}

func (t *term) cursorRight(n int) {
	c := t.cur()
	n = max(n, 1)
	c.lo, c.hi, c.wrap = min(c.lo+n, t.cols-1), min(c.hi+n, t.cols-1), false
	t.afterUncertain = false
}

func (t *term) cursorLeft(n int) {
	c := t.cur()
	n = max(n, 1)
	t.afterUncertain = false
	if t.autowrap && (t.reverseWrap || t.reverseWrapExt) {
		// Ghostty wraps back to the previous row; the model follows that only within the row.
		if c.lo != c.hi || c.wrap || c.lo < n {
			t.lose()
			return
		}
	}
	c.lo, c.hi, c.wrap = max(c.lo-n, 0), max(c.hi-n, 0), false
}

// setCursorPos is CUP: row and col count from 1, relative to the scroll region in origin mode.
func (t *term) setCursorPos(row, col int) {
	c := t.cur()
	top, rowMax := 0, t.rows
	if t.origin {
		top, rowMax = t.top, t.bottom+1
	}
	c.row, c.rowKnown = min(rowMax, max(row, 1)+top)-1, true
	c.setCol(min(t.cols, max(col, 1)) - 1)
	t.afterUncertain = false
}

// rowForSet is the cursor's row as setCursorPos counts it.
func (t *term) rowForSet() int {
	if t.origin {
		return max(t.cur().row-t.top, 0) + 1
	}
	return t.cur().row + 1
}

// setCursorCol is CHA and HPA: the row stays, though origin mode may pull it into the region.
func (t *term) setCursorCol(col int) {
	c := t.cur()
	if c.rowKnown {
		t.setCursorPos(t.rowForSet(), col)
		return
	}
	c.setCol(min(t.cols, max(col, 1)) - 1)
	t.afterUncertain = false
}

// setCursorRow is VPA: the column stays.
func (t *term) setCursorRow(row int) {
	c := t.cur()
	lo, hi := c.lo, c.hi
	t.setCursorPos(row, lo+1)
	c.lo, c.hi = lo, hi
}

func (t *term) cursorRowRelative(n int) {
	c := t.cur()
	if !c.rowKnown {
		c.wrap = false
		t.afterUncertain = false
		return
	}
	t.setCursorRow(t.rowForSet() + n)
}

func (t *term) cursorColRelative(n int) {
	c := t.cur()
	if !c.exact() {
		c.lo, c.hi, c.wrap = min(c.lo+n, t.cols-1), min(c.hi+n, t.cols-1), false
		t.afterUncertain = false
		return
	}
	t.setCursorCol(c.lo + 1 + n)
}

func (t *term) nextTab(x int) int {
	for x < t.cols-1 {
		x++
		if t.tabs[x] {
			break
		}
	}
	return x
}

func (t *term) prevTab(x int) int {
	for x > 0 {
		x--
		if t.tabs[x] {
			break
		}
	}
	return x
}

// tab is HT and CHT. Ghostty keeps a pending wrap through it.
func (t *term) tab(n int) {
	c := t.cur()
	t.afterUncertain = false
	if t.tabsUnknown {
		c.lo, c.hi = min(c.lo+1, t.cols-1), t.cols-1
		return
	}
	for range n {
		c.lo, c.hi = t.nextTab(c.lo), t.nextTab(c.hi)
	}
}

func (t *term) tabBack(n int) {
	c := t.cur()
	t.afterUncertain = false
	if t.tabsUnknown {
		c.lo, c.hi = 0, max(c.hi-1, 0)
		return
	}
	for range n {
		c.lo, c.hi = t.prevTab(c.lo), t.prevTab(c.hi)
	}
}

func (t *term) setTab(on bool) {
	c := t.cur()
	if c.lo != c.hi {
		t.tabsUnknown = true
		return
	}
	t.tabs[c.lo] = on
}

func (t *term) clearTabs() {
	t.tabs = make([]bool, t.cols)
	t.tabsUnknown = false
}

// erase blanks cells [from, to] of the cursor's row, a range that starts at
// a column the model only bounds: cells [maybeFrom, maybeTo] may or may not
// have been erased.
func (t *term) erase(from, to, maybeFrom, maybeTo int) effect {
	fx := untouched
	if maybeFrom <= maybeTo {
		fx = t.cells(maybeFrom, maybeTo, cell{})
	}
	if from <= to {
		fx = worst(fx, t.cells(from, to, blank(t.cur().pen.bg)))
	}
	return fx
}

// eraseLine is EL: 0 right of the cursor, 1 left of it, 2 the whole row.
func (t *term) eraseLine(mode int) effect {
	c := t.cur()
	c.wrap = false
	switch mode {
	case 0:
		return t.erase(c.hi, t.cols-1, c.lo, c.hi-1)
	case 1:
		return t.erase(0, c.lo, c.lo+1, c.hi)
	}
	return t.erase(0, t.cols-1, 0, -1)
}

// eraseDisplay is ED and DECSED.
func (t *term) eraseDisplay(mode int) effect {
	c := t.cur()
	bg := c.pen.bg
	switch mode {
	case 0:
		c.wrap = false
		if c.rowKnown && c.row < t.rows-1 {
			t.active().corner = [2]cell{blank(bg), blank(bg)}
			return erased
		}
		return t.eraseLine(0)
	case 1:
		c.wrap = false
		if c.rowKnown && c.row == t.rows-1 {
			return t.eraseLine(1)
		}
		if !c.rowKnown {
			return t.lostCorner()
		}
		return untouched
	case 2, 22:
		// On the primary screen Ghostty may first push the screen into scrollback.
		c.wrap = false
		t.active().corner = [2]cell{blank(bg), blank(bg)}
		if t.alt && mode == 2 {
			return erased
		}
		return moved
	}
	return untouched
}

func (t *term) eraseChars(n int) effect {
	c := t.cur()
	c.wrap = false
	n = max(n, 1)
	if c.lo == c.hi {
		return t.erase(c.lo, c.lo+n-1, 0, -1)
	}
	return t.erase(c.hi, c.lo+n-1, c.lo, c.hi+n-1)
}

func (t *term) insertBlanks(n int) effect {
	t.cur().wrap = false
	if n == 0 {
		return untouched
	}
	return t.shiftRow()
}

func (t *term) deleteChars(n int) effect {
	if n == 0 {
		return untouched
	}
	t.cur().wrap = false
	return t.shiftRow()
}

// lines is IL (insert) or DL: inside the scroll region the rows from the
// cursor's shift, and the cursor goes to the first column.
func (t *term) lines(n int, insert bool) effect {
	c := t.cur()
	if n == 0 || c.rowKnown && (c.row < t.top || c.row > t.bottom) {
		return untouched
	}
	fx := untouched
	switch {
	case !c.rowKnown && t.bottom == t.rows-1:
		fx = t.lostCorner()
	case insert:
		fx = t.scrollDown()
	default:
		fx = t.scrollUp()
	}
	if !c.rowKnown {
		// The cursor may have been outside the region, where nothing happens.
		c.lo, c.wrap = 0, false
		t.afterUncertain = false
		return fx
	}
	c.setCol(0)
	t.afterUncertain = false
	return fx
}

func (t *term) scrollRegion(n int, up bool) effect {
	if n == 0 {
		return untouched
	}
	if up {
		return t.scrollUp()
	}
	return t.scrollDown()
}

func (t *term) setTopBottom(top, bottom int) {
	top = max(top, 1)
	if bottom == 0 || bottom > t.rows {
		bottom = t.rows
	}
	if top >= bottom {
		return
	}
	t.top, t.bottom = top-1, bottom-1
	t.setCursorPos(1, 1)
}

// leftRightMargins is DECSLRM, which only DECLRMM enables.
func (t *term) leftRightMargins(left, right int) {
	if !t.leftRight {
		return
	}
	left = max(left, 1)
	if right == 0 || right > t.cols {
		right = t.cols
	}
	if left >= right {
		return
	}
	t.margins = left != 1 || right != t.cols
	t.setCursorPos(1, 1)
}

func (t *term) saveCursor() {
	scr := t.active()
	scr.saved = &savedCursor{cur: scr.cur, origin: t.origin}
}

func (t *term) restoreCursor() {
	scr := t.active()
	saved := savedCursor{cur: cursor{rowKnown: true, pen: pen{}}}
	if scr.saved != nil {
		saved = *scr.saved
	}
	c := saved.cur
	c.row = min(c.row, t.rows-1)
	c.lo, c.hi = min(c.lo, t.cols-1), min(c.hi, t.cols-1)
	scr.cur = c
	t.origin = saved.origin
	t.afterUncertain = false
}

// switchScreen goes to the alternate or primary screen; a cursor copied
// across keeps everything but the hyperlink.
func (t *term) switchScreen(alt bool, copyCursor bool) {
	if t.alt == alt {
		return
	}
	from := t.cur()
	t.alt = alt
	if copyCursor {
		t.active().cur = *from
	}
	t.active().link = ""
	t.afterUncertain = false
}

// altScreen is DECSET and DECRST 47, 1047 and 1049.
func (t *term) altScreen(mode int, on bool) effect {
	fx := untouched
	switch mode {
	case 47:
		t.switchScreen(on, true)
	case 1047:
		if !on && t.alt {
			fx = t.eraseDisplay(2)
		}
		t.switchScreen(on, true)
	case 1049:
		if !on {
			t.switchScreen(false, false)
			t.restoreCursor()
			return fx
		}
		t.saveCursor()
		was := t.alt
		from := *t.cur()
		t.alt = true
		fx = t.eraseDisplay(2)
		if !was {
			t.active().cur = from
			t.active().link = ""
		}
		t.afterUncertain = false
	}
	return fx
}

// softReset is DECSTR: modes, margins, pen and saved cursor go back to their defaults; the cursor stays.
func (t *term) softReset() {
	t.insert, t.origin, t.autowrap, t.reverseWrap, t.reverseWrapExt = false, false, true, false, false
	t.leftRight, t.margins, t.shifted = false, false, false
	t.resetRegion()
	scr := t.active()
	scr.cur.pen = pen{}
	scr.cur.charset = charset{}
	scr.saved = nil
}

// fullReset is RIS: everything starts over on a cleared primary screen.
func (t *term) fullReset() {
	*t = newTerm(t.cols, t.rows)
	for i := range t.screens {
		t.screens[i].cur.rowKnown = true
		t.screens[i].cur.hi = 0
		t.screens[i].corner = [2]cell{blank(""), blank("")}
	}
}

// alignment is DECALN: margins reset, the screen fills with E and the cursor goes home.
func (t *term) alignment() effect {
	t.resetRegion()
	t.margins = false
	t.origin = false
	t.setCursorPos(1, 1)
	t.active().corner = [2]cell{{state: cellFilled}, {state: cellFilled}}
	return erased
}

// repeat is REP: the last printed character n more times.
func (t *term) repeat(n int) effect {
	if !t.hasLast {
		return untouched
	}
	if !t.lastCertain {
		t.lose()
		return t.lostCorner()
	}
	fx := untouched
	for range max(n, 1) {
		fx = worst(fx, t.print(t.last))
	}
	return fx
}

// resize starts over at a new size: Ghostty reflows the screens, so nothing
// about the cursors or corners is known any more.
func (t *term) resize(cols, rows int) {
	t.cols, t.rows = cols, rows
	t.resetRegion()
	t.margins = false
	t.resetTabs()
	for i := range t.screens {
		scr := &t.screens[i]
		pen, cs := scr.cur.pen, scr.cur.charset
		scr.cur = cursor{hi: cols - 1, pen: pen, charset: cs}
		if scr.saved != nil {
			saved := *scr.saved
			saved.cur.rowKnown, saved.cur.lo, saved.cur.hi, saved.cur.wrap = false, 0, cols-1, false
			scr.saved = &saved
		}
		scr.corner = [2]cell{}
		scr.drawn = false
	}
	t.afterUncertain = false
}
