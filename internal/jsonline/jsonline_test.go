package jsonline

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type msg struct {
	A string `json:"a"`
}

func TestRoundTripAndLimits(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, msg{A: strings.Repeat("x", 10000)}); err != nil {
		t.Fatal(err)
	}
	if err := Write(&buf, msg{A: "y"}); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(&buf)
	if m, err := Read[msg](r, 20000); err != nil || len(m.A) != 10000 {
		t.Fatalf("long message: %v", err)
	}
	if m, err := Read[msg](r, 100); err != nil || m.A != "y" {
		t.Fatalf("short message: %+v %v", m, err)
	}
	if _, err := Read[msg](r, 100); !errors.Is(err, io.EOF) {
		t.Fatalf("end: %v", err)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]string{
		"too long":      `{"a":"` + strings.Repeat("x", 200) + `"}` + "\n",
		"unknown field": `{"a":"x","b":1}` + "\n",
		"truncated":     `{"a":"x"`,
	}
	for name, in := range cases {
		if _, err := Read[msg](bufio.NewReader(strings.NewReader(in)), 100); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}
