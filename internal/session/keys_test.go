package session

import "testing"

func TestIsInterrupt(t *testing.T) {
	yes := []string{
		"\x03",
		"abc\x03",
		"\x1b[99;5u",
		"\x1b[99;5:1u",
		"\x1b[99;5:2u",
		"\x1b[99::67;5u",
		"\x1b[99;69u", // ctrl with caps lock
		"\x1b[27;5;99~",
		"\x1b[I\x1b[99;5u",
	}
	no := []string{
		"",
		"c",
		"\x1b[99;5:3u", // key release
		"\x1b[99;6u",   // ctrl+shift
		"\x1b[99;3u",   // alt
		"\x1b[100;5u",  // ctrl+d
		"\x1b[I",       // focus in
		"\x1b[200~paste\x1b[201~",
	}
	for _, s := range yes {
		if !isInterrupt([]byte(s)) {
			t.Errorf("%q should be an interrupt", s)
		}
	}
	for _, s := range no {
		if isInterrupt([]byte(s)) {
			t.Errorf("%q should not be an interrupt", s)
		}
	}
}
