package corner

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// sgrParams parses the parameters of one SGR sequence, as the writer's parser hands them over.
func sgrParams(t *testing.T, seq string) ansi.Params {
	t.Helper()
	var got ansi.Params
	p := ansi.NewParser()
	p.SetHandler(ansi.Handler{HandleCsi: func(cmd ansi.Cmd, params ansi.Params) {
		got = append(ansi.Params(nil), params...)
	}})
	for _, b := range []byte(seq) {
		p.Advance(b)
	}
	return got
}

func TestPenFollowsSGR(t *testing.T) {
	for _, tc := range []struct {
		name, seqs, want string
	}{
		{"reset by empty", "\x1b[1;31m\x1b[m", "\x1b[0m"},
		{"attributes", "\x1b[1;2;3;5;7;8;9;53m", "\x1b[0;1;2;3;5;7;8;9;53m"},
		{"22 clears bold and faint", "\x1b[1;2;3m\x1b[22m", "\x1b[0;3m"},
		{"basic and bright colours", "\x1b[31;102m", "\x1b[0;31;102m"},
		{"256 colours", "\x1b[38;5;196;48;5;17;58;5;300m", "\x1b[0;38;5;196;48;5;17;58;5;44m"},
		{"direct colours", "\x1b[38;2;1;2;3;48;2;4;5;6m", "\x1b[0;38;2;1;2;3;48;2;4;5;6m"},
		{"colon direct colour with colour space", "\x1b[38:2::10:20:30m", "\x1b[0;38;2;10;20;30m"},
		{"colon direct colour without colour space", "\x1b[48:2:10:20:30m", "\x1b[0;48;2;10;20;30m"},
		{"colon indexed colour", "\x1b[58:5:9m", "\x1b[0;58;5;9m"},
		{"underline styles", "\x1b[4:3m", "\x1b[0;4:3m"},
		{"double underline", "\x1b[21m", "\x1b[0;4:2m"},
		{"unknown underline style is single", "\x1b[4:9m", "\x1b[0;4m"},
		{"underline off by colon", "\x1b[4m\x1b[4:0m", "\x1b[0m"},
		{"resets", "\x1b[1;3;4;5;7;8;9;53;31;41;58;5;1m\x1b[23;24;25;27;28;29;55;39;49;59m", "\x1b[0;1m"},
		{"short direct colour reads on", "\x1b[38;2;1m", "\x1b[0;1;2m"},
		{"unknown codes are skipped", "\x1b[3;73;31m", "\x1b[0;3;31m"},
		{"unknown colon group is skipped whole", "\x1b[1:2:3;31m", "\x1b[0;31m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p pen
			for _, seq := range splitCSI(tc.seqs) {
				p.apply(sgrParams(t, seq))
			}
			if got := p.sgr(); got != tc.want {
				t.Fatalf("sgr() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPenSGRRoundTrips(t *testing.T) {
	var p pen
	p.apply(sgrParams(t, "\x1b[1;4:3;7;38;2;9;8;7;105;58;5;3m"))
	var again pen
	again.apply(sgrParams(t, p.sgr()))
	if again != p {
		t.Fatalf("pen %+v reads back as %+v", p, again)
	}
}

// splitCSI cuts concatenated CSI sequences apart.
func splitCSI(s string) []string {
	var out []string
	start := 0
	for i := 1; i < len(s); i++ {
		if s[i] == 0x1b {
			out = append(out, s[start:i])
			start = i
		}
	}
	return append(out, s[start:])
}
