package accounts

import (
	"bytes"
	"strings"
	"testing"
)

func TestChoose(t *testing.T) {
	l := Listing{Accounts: []Row{{Name: "", Active: false}, {Name: "home"}, {Name: "work", Active: true}}}
	cases := []struct{ in, want, err string }{
		{"1\n", "home", ""},
		{"home\n", "home", ""},
		{" 2 \n", "", ""},
		{"work\n", "", ""},
		{"\n", "", ""},
		{"home", "home", ""},
		{"3\n", "", "no account 3"},
		{"x\n", "", "no account x"},
		{"", "", "reading the choice"},
	}
	for _, c := range cases {
		var out bytes.Buffer
		got, err := Choose(strings.NewReader(c.in), &out, l, "rclaude")
		if got != c.want || (err == nil) != (c.err == "") || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%q: got %q, %v", c.in, got, err)
		}
		if out.String() != "Switch to (1-2 or a name, Enter keeps work): " {
			t.Errorf("prompt %q", out.String())
		}
	}
	if _, err := Choose(strings.NewReader("1\n"), &bytes.Buffer{}, Listing{Accounts: []Row{{Active: true}}}, "rcodex"); err == nil || !strings.Contains(err.Error(), "rcodex account add") {
		t.Fatalf("no accounts: %v", err)
	}
}
