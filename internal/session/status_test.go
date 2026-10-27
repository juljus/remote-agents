package session

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const timedOut = "ssh: connect to host 198.51.100.76 port 22: Operation timed out"

func TestShortReason(t *testing.T) {
	cases := map[string]string{
		timedOut: "timed out",
		"Timeout, server 198.51.100.76 not responding.":                       "timed out",
		"Connection to 198.51.100.76 port 22 timed out":                       "timed out",
		"ssh: connect to host 198.51.100.76 port 22: Network is unreachable":  "network unreachable",
		"ssh: connect to host 198.51.100.76 port 22: Connection refused":      "connection refused",
		"ssh: connect to host 198.51.100.76 port 22: No route to host":        "no route to host",
		"ssh: Could not resolve hostname box: nodename nor servname provided": "host not found",
		"Connection closed by 198.51.100.76 port 22":                          "connection closed",
		"client_loop: send disconnect: Broken pipe":                           "connection lost",
		"agent@box: Permission denied (publickey).":                           "permission denied",
		"ssh ended by signal 9 (killed)":                                      "ssh ended by signal 9 (killed)",
		"weird\x1b[31m é detail\r\n":                                          "weird[31m ? detail",
	}
	for detail, want := range cases {
		if got := shortReason(detail); got != want {
			t.Errorf("shortReason(%q) = %q, want %q", detail, got, want)
		}
	}
}

func TestStatusTextShowsEverythingWhenItFits(t *testing.T) {
	got := statusText(0, "box-1.example.com", 67*time.Second, "timed out", 120)
	want := "⠋ reconnecting to box-1.example.com - 1:07 - timed out - Ctrl+C detaches"
	if got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
}

func TestStatusTextShortensTheReasonFirst(t *testing.T) {
	got := statusText(1, "box-1.example.com", 5*time.Second, "something long went wrong here", 75)
	want := "⠙ reconnecting to box-1.example.com - 0:05 - somethin... - Ctrl+C detaches"
	if utf8.RuneCountInString(want) != 74 {
		t.Fatalf("bad fixture: %d", utf8.RuneCountInString(want))
	}
	if got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
	got = statusText(1, "box-1.example.com", 5*time.Second, "something", 65)
	if got != "⠙ reconnecting to box-1.example.com - 0:05 - Ctrl+C detaches" {
		t.Fatalf("status without room for a reason = %q", got)
	}
}

func TestStatusTextDropsTheHostThenTheHintWhenNarrow(t *testing.T) {
	cases := map[int]string{
		40: "⠋ reconnecting - 0:21 - Ctrl+C detaches",
		25: "⠋ reconnecting - 0:21",
		10: "⠋ reconne",
	}
	for width, want := range cases {
		if got := statusText(0, "box-1.example.com", 21*time.Second, "timed out", width); got != want {
			t.Errorf("width %d: %q, want %q", width, got, want)
		}
	}
}

func TestStatusTextNeverReachesTheLastColumn(t *testing.T) {
	for width := 1; width <= 130; width++ {
		got := statusText(7, "box-1.example.com", 3725*time.Second, shortReason(timedOut), width)
		if n := utf8.RuneCountInString(got); n > width-1 {
			t.Fatalf("width %d: %d columns: %q", width, n, got)
		}
	}
}

// fakeClock is a settable now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// recordingScreen keeps the rows after every write, one entry per redraw.
type recordingScreen struct {
	*screen
	after [][]string
}

func (r *recordingScreen) Write(p []byte) (int, error) {
	n, err := r.screen.Write(p)
	r.after = append(r.after, r.used())
	return n, err
}

func newOutage(out *recordingScreen, width int, clock *fakeClock, interval time.Duration) *outage {
	return &outage{
		out:      out,
		width:    func() (int, error) { return width, nil },
		now:      clock.now,
		host:     "box-1.example.com",
		interval: interval,
		logf:     func(string, ...any) {},
	}
}

func TestOutageKeepsOneRowAtWidth40(t *testing.T) {
	scr := &recordingScreen{screen: newScreen(40)}
	scr.screen.Write([]byte("agent screen"))
	clock := &fakeClock{t: time.Unix(0, 0)}
	o := newOutage(scr, 40, clock, time.Hour)
	o.Down(timedOut)
	for i := range 30 {
		clock.add(700 * time.Millisecond)
		if i%5 == 0 {
			o.Down("ssh: connect to host 198.51.100.76 port 22: Network is unreachable but with a very long tail")
		}
		o.advance()
	}
	for i, rows := range scr.after {
		if len(rows) != 2 || rows[0] != "agent screen" {
			t.Fatalf("redraw %d left rows %q", i, rows)
		}
		if n := utf8.RuneCountInString(rows[1]); n > 39 {
			t.Fatalf("redraw %d: status is %d columns", i, n)
		}
	}
	last := scr.after[len(scr.after)-1][1]
	if !strings.Contains(last, "0:21") {
		t.Fatalf("elapsed not shown: %q", last)
	}
	if !o.End() {
		t.Fatal("End reported no status shown")
	}
	if rows := scr.used(); len(rows) != 1 || scr.col != 0 {
		t.Fatalf("after End rows %q, cursor column %d", rows, scr.col)
	}
	if o.End() {
		t.Fatal("second End reported a status")
	}
}

func TestOutputGateClearsTheStatusBeforeTheSessionRedraws(t *testing.T) {
	scr := &recordingScreen{screen: newScreen(40)}
	o := newOutage(scr, 40, &fakeClock{}, time.Hour)
	g := outputGate{w: scr, outage: o}
	o.Down(timedOut)
	fmt.Fprint(g, "redrawn")
	if rows := scr.used(); len(rows) != 1 || rows[0] != "redrawn" {
		t.Fatalf("rows = %q", rows)
	}
	// A second drop starts on a fresh line below what the session drew.
	o.Down(timedOut)
	if rows := scr.used(); len(rows) != 2 || rows[0] != "redrawn" || !strings.Contains(rows[1], "reconnecting") {
		t.Fatalf("rows = %q", rows)
	}
}

// lockedBuffer is written by the ticker goroutine and read by the test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestOutageAnimatesUntilEnd(t *testing.T) {
	out := &lockedBuffer{}
	o := &outage{
		out:      out,
		width:    func() (int, error) { return 80, nil },
		now:      time.Now,
		host:     "box",
		interval: 5 * time.Millisecond,
		logf:     func(string, ...any) {},
	}
	o.Down(timedOut)
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "⠹") {
		if time.Now().After(deadline) {
			t.Fatalf("spinner never advanced: %q", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	o.End()
	ended := out.String()
	time.Sleep(30 * time.Millisecond)
	if out.String() != ended {
		t.Fatalf("drew after End: %q", strings.TrimPrefix(out.String(), ended))
	}
	if !strings.HasSuffix(ended, "\r\x1b[2K") {
		t.Fatalf("End did not clear the line: %q", ended)
	}
}

func TestOutageLogsAnUnreadableWidthOnce(t *testing.T) {
	var logs []string
	out := &lockedBuffer{}
	o := &outage{
		out:      out,
		width:    func() (int, error) { return 0, fmt.Errorf("no tty") },
		now:      time.Now,
		host:     "box",
		interval: time.Hour,
		logf:     func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	o.Down(timedOut)
	o.advance()
	o.End()
	if len(logs) != 1 || !strings.Contains(logs[0], "no tty") {
		t.Fatalf("logs = %q", logs)
	}
	if strings.Contains(out.String(), "reconnecting") {
		t.Fatalf("drew without a width: %q", out.String())
	}
}
