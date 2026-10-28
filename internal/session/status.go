package session

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var spinner = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// reasons maps ssh failure text to a few words; the first match wins.
var reasons = []struct{ needle, short string }{
	{"timed out", "timed out"},
	{"timeout", "timed out"},
	{"network is unreachable", "network unreachable"},
	{"connection refused", "connection refused"},
	{"no route to host", "no route to host"},
	{"could not resolve hostname", "host not found"},
	{"connection closed", "connection closed"},
	{"broken pipe", "connection lost"},
	{"permission denied", "permission denied"},
}

// shortReason names why ssh failed, or gives its own text made printable.
func shortReason(detail string) string {
	lower := strings.ToLower(detail)
	for _, r := range reasons {
		if strings.Contains(lower, r.needle) {
			return r.short
		}
	}
	return printable(detail)
}

// printable keeps ASCII text one column per byte: controls go, the rest becomes '?'.
func printable(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

// statusText is one status line that stays clear of the last column, so the
// terminal never wraps it. As the width shrinks, the reason is shortened and
// dropped first, then the host, then the detach hint.
func statusText(frame int, host string, elapsed time.Duration, reason string, width int) string {
	const sep, ellipsis, detach = " - ", "...", " - Ctrl+C detaches"
	limit := width - 1
	secs := int(elapsed / time.Second)
	short := fmt.Sprintf("%c reconnecting%s%d:%02d", spinner[frame%len(spinner)], sep, secs/60, secs%60)
	withHost := strings.Replace(short, "reconnecting", "reconnecting to "+printable(host), 1)
	// reason is printable ASCII, so its length in bytes is its width.
	if room := limit - utf8.RuneCountInString(withHost) - len(sep) - len(detach); reason != "" && room > len(ellipsis) {
		if len(reason) > room {
			reason = reason[:room-len(ellipsis)] + ellipsis
		}
		return withHost + sep + reason + detach
	}
	for _, line := range []string{withHost + detach, short + detach, short} {
		if utf8.RuneCountInString(line) <= limit {
			return line
		}
	}
	return string([]rune(short)[:max(limit, 0)])
}

// outage draws the reconnect status while the link is down: one line,
// redrawn in place on every tick and cleared when the outage ends.
type outage struct {
	out      io.Writer
	width    func() (int, error)
	now      func() time.Time
	host     string
	interval time.Duration
	logf     func(format string, args ...any)

	mu          sync.Mutex
	active      bool
	since       time.Time
	reason      string
	frame       int
	widthLogged bool
	stop        chan struct{}
	stopped     chan struct{}
}

// Down starts the status on a fresh line, or updates the reason of the one shown.
func (o *outage) Down(detail string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reason = shortReason(detail)
	if o.active {
		o.draw("")
		return
	}
	o.active, o.since, o.frame = true, o.now(), 0
	o.stop, o.stopped = make(chan struct{}), make(chan struct{})
	o.draw("\r\n")
	go o.animate(o.stop, o.stopped)
}

func (o *outage) animate(stop <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)
	tick := time.NewTicker(o.interval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			o.advance()
		}
	}
}

func (o *outage) advance() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.active {
		return
	}
	o.frame++
	o.draw("")
}

// draw rereads the width each time, so a resized terminal gets a line that fits.
func (o *outage) draw(prefix string) {
	width, err := o.width()
	if err != nil {
		if !o.widthLogged {
			o.logf("reconnect status: reading terminal width: %v", err)
			o.widthLogged = true
		}
		fmt.Fprint(o.out, prefix)
		return
	}
	fmt.Fprint(o.out, prefix+"\r\x1b[2K"+statusText(o.frame, o.host, o.now().Sub(o.since), o.reason, width))
}

// End clears the status line and stops drawing; it reports whether one was shown.
func (o *outage) End() bool {
	o.mu.Lock()
	if !o.active {
		o.mu.Unlock()
		return false
	}
	o.active = false
	fmt.Fprint(o.out, "\r\x1b[2K")
	close(o.stop)
	stopped := o.stopped
	o.mu.Unlock()
	<-stopped
	return true
}
