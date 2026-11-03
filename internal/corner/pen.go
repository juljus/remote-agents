package corner

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// pen is the graphic rendition new cells get, as Ghostty keeps it. Colours
// are the SGR parameters that set them; "" is the default colour.
type pen struct {
	bold, faint, italic, blink, inverse, invisible, strike, overline bool
	// underline is the SGR 4 sub-parameter: 0 none, 1 single, 2 double, 3 curly, 4 dotted, 5 dashed.
	underline  int
	fg, bg, ul string
}

// apply follows one SGR sequence the way Ghostty parses it; codes it ignores are skipped.
func (p *pen) apply(params ansi.Params) {
	if len(params) == 0 {
		*p = pen{}
		return
	}
	for i := 0; i < len(params); {
		code := params[i].Param(0)
		if params[i].HasMore() {
			i += p.applyColon(params[i:])
			continue
		}
		i++
		switch code {
		case 0:
			*p = pen{}
		case 1:
			p.bold = true
		case 2:
			p.faint = true
		case 3:
			p.italic = true
		case 4:
			p.underline = 1
		case 5, 6:
			p.blink = true
		case 7:
			p.inverse = true
		case 8:
			p.invisible = true
		case 9:
			p.strike = true
		case 21:
			p.underline = 2
		case 22:
			p.bold, p.faint = false, false
		case 23:
			p.italic = false
		case 24:
			p.underline = 0
		case 25:
			p.blink = false
		case 27:
			p.inverse = false
		case 28:
			p.invisible = false
		case 29:
			p.strike = false
		case 39:
			p.fg = ""
		case 49:
			p.bg = ""
		case 53:
			p.overline = true
		case 55:
			p.overline = false
		case 59:
			p.ul = ""
		case 38, 48, 58:
			colour, used := extendedColour(code, params[i:])
			i += used
			if used > 0 {
				p.setExtended(code, colour)
			}
		default:
			switch {
			case code >= 30 && code <= 37, code >= 90 && code <= 97:
				p.fg = strconv.Itoa(code)
			case code >= 40 && code <= 47, code >= 100 && code <= 107:
				p.bg = strconv.Itoa(code)
			}
		}
	}
}

// applyColon handles a code followed by colon sub-parameters and returns how many parameters it used.
func (p *pen) applyColon(params ansi.Params) int {
	n := 1
	for n < len(params) && params[n-1].HasMore() {
		n++
	}
	sub := func(i int) int { return params[i].Param(0) }
	switch code := sub(0); code {
	case 4:
		// Ghostty draws a style it does not know as a single underline.
		if style := sub(1); n == 2 && style <= 5 {
			p.underline = style
		} else if n == 2 {
			p.underline = 1
		}
	case 38, 48, 58:
		switch {
		case n == 3 && sub(1) == 5:
			p.setExtended(code, "5;"+strconv.Itoa(sub(2)&0xff))
		case n == 5 && sub(1) == 2:
			p.setExtended(code, rgb(sub(2), sub(3), sub(4)))
		case n == 6 && sub(1) == 2:
			p.setExtended(code, rgb(sub(3), sub(4), sub(5)))
		}
	}
	return n
}

// extendedColour reads the semicolon form of a 38, 48 or 58 colour from the
// parameters after the code and returns it with how many it used; none
// used means Ghostty ignores the code and reads on from the next parameter.
func extendedColour(code int, rest ansi.Params) (string, int) {
	if len(rest) == 0 {
		return "", 0
	}
	switch rest[0].Param(0) {
	case 5:
		if len(rest) >= 2 {
			return "5;" + strconv.Itoa(rest[1].Param(0)&0xff), 2
		}
	case 2:
		if len(rest) >= 4 {
			return rgb(rest[1].Param(0), rest[2].Param(0), rest[3].Param(0)), 4
		}
	}
	return "", 0
}

func rgb(r, g, b int) string {
	return "2;" + strconv.Itoa(r&0xff) + ";" + strconv.Itoa(g&0xff) + ";" + strconv.Itoa(b&0xff)
}

func (p *pen) setExtended(code int, colour string) {
	value := strconv.Itoa(code) + ";" + colour
	switch code {
	case 38:
		p.fg = value
	case 48:
		p.bg = value
	case 58:
		p.ul = value
	}
}

// sgr is the sequence that sets the pen to p from any other.
func (p pen) sgr() string {
	var b strings.Builder
	b.WriteString("\x1b[0")
	for _, f := range []struct {
		on   bool
		code string
	}{
		{p.bold, "1"}, {p.faint, "2"}, {p.italic, "3"}, {p.blink, "5"}, {p.inverse, "7"},
		{p.invisible, "8"}, {p.strike, "9"}, {p.overline, "53"},
	} {
		if f.on {
			b.WriteString(";" + f.code)
		}
	}
	switch p.underline {
	case 0:
	case 1:
		b.WriteString(";4")
	default:
		b.WriteString(";4:" + strconv.Itoa(p.underline))
	}
	for _, c := range []string{p.fg, p.bg, p.ul} {
		if c != "" {
			b.WriteString(";" + c)
		}
	}
	b.WriteString("m")
	return b.String()
}

// blankSGR is the pen an erase with background bg would leave cells in.
func blankSGR(bg string) string {
	if bg == "" {
		return "\x1b[0m"
	}
	return "\x1b[0;" + bg + "m"
}
