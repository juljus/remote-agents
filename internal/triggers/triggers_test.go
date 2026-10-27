package triggers

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestWakeDetector(t *testing.T) {
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	d := WakeDetector{Threshold: 3 * time.Second}
	steps := []struct {
		wall time.Duration
		mono time.Duration
		woke bool
	}{
		{0, 0, false}, // first sample only primes the detector
		{1 * time.Second, 1 * time.Second, false},         // normal tick
		{2100 * time.Millisecond, 2 * time.Second, false}, // small drift
		{10 * time.Minute, 3 * time.Second, true},         // slept ~10 minutes
		{10*time.Minute + time.Second, 4 * time.Second, false},
		{9 * time.Minute, 5 * time.Second, false}, // wall clock stepped back: not a wake
		{9*time.Minute + 5*time.Second, 6 * time.Second, true},
	}
	for i, s := range steps {
		_, woke := d.Observe(Sample{Wall: base.Add(s.wall), Mono: s.mono})
		if woke != s.woke {
			t.Fatalf("step %d: woke = %v, want %v", i, woke, s.woke)
		}
	}
}

func TestFingerprintIgnoresLinkLocalLoopbackAndOrder(t *testing.T) {
	a := Snapshot{
		Addrs: []IfaceAddr{
			{"en0", netip.MustParseAddr("192.168.1.20")},
			{"lo0", netip.MustParseAddr("127.0.0.1")},
			{"awdl0", netip.MustParseAddr("fe80::1")},
			{"utun4", netip.MustParseAddr("100.64.0.3")},
		},
		DefaultRoutes: []string{"en0 192.168.1.1"},
	}
	b := Snapshot{
		Addrs: []IfaceAddr{
			{"utun4", netip.MustParseAddr("100.64.0.3")},
			{"en0", netip.MustParseAddr("192.168.1.20")},
			{"awdl0", netip.MustParseAddr("fe80::99")},
		},
		DefaultRoutes: []string{"en0 192.168.1.1"},
	}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("fingerprints differ:\n%s\n%s", a.Fingerprint(), b.Fingerprint())
	}
	c := b
	c.DefaultRoutes = []string{"en1 10.0.0.1"}
	if c.Fingerprint() == b.Fingerprint() {
		t.Fatal("default route change not reflected in fingerprint")
	}
	d := b
	d.Addrs = []IfaceAddr{{"en0", netip.MustParseAddr("192.168.1.21")}}
	if d.Fingerprint() == b.Fingerprint() {
		t.Fatal("address change not reflected in fingerprint")
	}
	e := b
	e.Addrs = append(append([]IfaceAddr(nil), b.Addrs...), IfaceAddr{"en0", netip.MustParseAddr("2001:db8::1234")})
	if e.Fingerprint() != b.Fingerprint() {
		t.Fatal("rotating IPv6 address changed the fingerprint")
	}
}

type fakeSources struct {
	samples   chan Sample
	snapshots chan Snapshot
}

func (f fakeSources) clock() Sample              { return <-f.samples }
func (f fakeSources) network() (Snapshot, error) { return <-f.snapshots, nil }

func TestWatchEmitsWakeAndNetworkChange(t *testing.T) {
	src := fakeSources{samples: make(chan Sample, 8), snapshots: make(chan Snapshot, 8)}
	ticks := make(chan time.Time)
	out := make(chan string, 4)
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	net1 := Snapshot{DefaultRoutes: []string{"en0 192.168.1.1"}}
	net2 := Snapshot{DefaultRoutes: []string{"en0 10.0.0.1"}}

	src.samples <- Sample{Wall: base, Mono: 0}
	src.snapshots <- net1
	w := Watcher{
		Ticks:   ticks,
		Clock:   src.clock,
		Network: src.network,
		Wake:    WakeDetector{Threshold: 3 * time.Second},
		Out:     out,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- w.Run(ctx) }()

	src.samples <- Sample{Wall: base.Add(time.Second), Mono: time.Second}
	src.snapshots <- net1
	ticks <- time.Now()
	src.samples <- Sample{Wall: base.Add(time.Hour), Mono: 2 * time.Second}
	src.snapshots <- net1
	ticks <- time.Now()
	expect(t, out, "wake")
	src.samples <- Sample{Wall: base.Add(time.Hour + time.Second), Mono: 3 * time.Second}
	src.snapshots <- net2
	ticks <- time.Now()
	expect(t, out, "network")
	select {
	case got := <-out:
		t.Fatalf("unexpected extra trigger %q", got)
	default:
	}
}

func expect(t *testing.T, out chan string, prefix string) {
	t.Helper()
	select {
	case got := <-out:
		if !strings.HasPrefix(got, prefix) {
			t.Fatalf("trigger %q, want prefix %q", got, prefix)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no %s trigger", prefix)
	}
}
