package paste

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func newFilter(out *bytes.Buffer) *Filter {
	return &Filter{W: out, Rewrite: func(content []byte) []byte { return bytes.ToUpper(content) }}
}

func write(t *testing.T, f *Filter, chunks ...string) {
	t.Helper()
	for _, c := range chunks {
		if n, err := f.Write([]byte(c)); err != nil || n != len(c) {
			t.Fatalf("Write(%q) = %d, %v", c, n, err)
		}
	}
}

func TestFilterRewritesBracketedPastesOnly(t *testing.T) {
	const want = "ab\x1b[200~PATH\x1b[201~cd"
	for name, chunks := range map[string][]string{
		"one write":      {"ab\x1b[200~path\x1b[201~cd"},
		"split markers":  {"ab\x1b[2", "00~pa", "th\x1b[20", "1~cd"},
		"byte at a time": strings.Split("ab\x1b[200~path\x1b[201~cd", ""),
	} {
		var out bytes.Buffer
		f := newFilter(&out)
		write(t, f, chunks...)
		if out.String() != want || f.HoldFor() != 0 {
			t.Errorf("%s: wrote %q (holding %v), want %q", name, out.String(), f.HoldFor(), want)
		}
	}
}

func TestFilterPassesKeysWithoutDelay(t *testing.T) {
	var out bytes.Buffer
	f := newFilter(&out)
	write(t, f, "hello\r\x1b[A\x03")
	if out.String() != "hello\r\x1b[A\x03" || f.HoldFor() != 0 {
		t.Fatalf("wrote %q, holding %v", out.String(), f.HoldFor())
	}
}

func TestFilterHoldsAPossibleMarkerOnlyBriefly(t *testing.T) {
	var out bytes.Buffer
	f := newFilter(&out)
	// A lone Esc could start a paste marker; it waits for the next byte, but not for long.
	write(t, f, "x\x1b")
	if out.String() != "x" || f.HoldFor() <= 0 || f.HoldFor() > 50*time.Millisecond {
		t.Fatalf("wrote %q, holding %v", out.String(), f.HoldFor())
	}
	if err := f.Flush(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "x\x1b" || f.HoldFor() != 0 {
		t.Fatalf("after flush wrote %q, holding %v", out.String(), f.HoldFor())
	}
	// Esc followed by an ordinary key goes out at once.
	write(t, f, "\x1b", "[B")
	if out.String() != "x\x1b\x1b[B" {
		t.Fatalf("wrote %q", out.String())
	}
}

func TestFilterGivesUpOnAPasteThatNeverEnds(t *testing.T) {
	var out bytes.Buffer
	f := newFilter(&out)
	write(t, f, "\x1b[200~typed by hand")
	if out.Len() != 0 || f.HoldFor() == 0 {
		t.Fatalf("wrote %q, holding %v", out.String(), f.HoldFor())
	}
	if err := f.Flush(); err != nil {
		t.Fatal(err)
	}
	write(t, f, " then more")
	if out.String() != "\x1b[200~typed by hand then more" {
		t.Fatalf("wrote %q", out.String())
	}
}

func TestFilterPassesLargePastesUnchanged(t *testing.T) {
	var out bytes.Buffer
	f := newFilter(&out)
	big := strings.Repeat("text ", maxPaste/5+10)
	write(t, f, "\x1b[200~"+big[:maxPaste/2], big[maxPaste/2:]+"\x1b[20", "1~after\x1b[200~p\x1b[201~")
	want := "\x1b[200~" + big + "\x1b[201~after\x1b[200~P\x1b[201~"
	if out.String() != want {
		t.Fatalf("large paste changed: got %d bytes, want %d", out.Len(), len(want))
	}
}
