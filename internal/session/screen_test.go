package session

import (
	"strings"
	"testing"
)

// screen models a terminal of fixed width with autowrap: enough to see which
// rows a status line leaves behind. It understands CR, LF, CSI 2K and CSI <n>A.
type screen struct {
	width    int
	rows     [][]rune
	row, col int
}

func newScreen(width int) *screen { return &screen{width: width, rows: [][]rune{nil}} }

func (s *screen) Write(p []byte) (int, error) {
	in := []rune(string(p))
	for i := 0; i < len(in); i++ {
		switch r := in[i]; {
		case r == '\r':
			s.col = 0
		case r == '\n':
			s.down()
		case r == 0x1b && i+1 < len(in) && in[i+1] == '[':
			j := i + 2
			for j < len(in) && (in[j] >= '0' && in[j] <= '9' || in[j] == ';' || in[j] == '?' || in[j] == '<' || in[j] == '>') {
				j++
			}
			if j == len(in) {
				panic("truncated escape sequence")
			}
			s.csi(string(in[i+2:j]), in[j])
			i = j
		case r < 0x20:
			panic("unexpected control character")
		default:
			if s.col == s.width {
				s.col = 0
				s.down()
			}
			line := s.rows[s.row]
			for len(line) <= s.col {
				line = append(line, ' ')
			}
			line[s.col] = r
			s.rows[s.row] = line
			s.col++
		}
	}
	return len(p), nil
}

func (s *screen) down() {
	s.row++
	if s.row == len(s.rows) {
		s.rows = append(s.rows, nil)
	}
}

func (s *screen) csi(params string, final rune) {
	switch final {
	case 'K':
		if params != "2" {
			panic("unsupported erase " + params)
		}
		s.rows[s.row] = nil
	case 'A':
		n := 1
		if params != "" {
			n = 0
			for _, d := range params {
				n = n*10 + int(d-'0')
			}
		}
		s.row = max(0, s.row-n)
	case 'h', 'l', 'm', 'u':
		// Mode and colour changes draw nothing.
	default:
		panic("unsupported CSI " + string(final))
	}
}

// used are the rows holding any text.
func (s *screen) used() []string {
	var out []string
	for _, r := range s.rows {
		if line := strings.TrimRight(string(r), " "); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func TestScreenWraps(t *testing.T) {
	s := newScreen(4)
	s.Write([]byte("abcdef\r\x1b[2Kxy"))
	if got := s.used(); len(got) != 2 || got[0] != "abcd" || got[1] != "xy" {
		t.Fatalf("rows = %q", got)
	}
}
