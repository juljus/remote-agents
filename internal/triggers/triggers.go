// Package triggers detects events after which an existing ssh connection is
// probably dead: waking from sleep and network changes.
package triggers

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// Sample is a paired reading of the wall clock and a monotonic clock that
// does not advance while the machine sleeps.
type Sample struct {
	Wall time.Time
	Mono time.Duration
}

// WakeDetector reports a wake when the wall clock moved further than the
// monotonic clock by more than Threshold between two samples.
type WakeDetector struct {
	Threshold time.Duration
	last      Sample
	primed    bool
}

func (d *WakeDetector) Observe(s Sample) (time.Duration, bool) {
	prev, primed := d.last, d.primed
	d.last, d.primed = s, true
	if !primed {
		return 0, false
	}
	gap := s.Wall.Sub(prev.Wall) - (s.Mono - prev.Mono)
	return gap, gap > d.Threshold
}

type IfaceAddr struct {
	Iface string
	Addr  netip.Addr
}

// Snapshot is the part of the network configuration a connection depends on.
type Snapshot struct {
	Addrs         []IfaceAddr
	DefaultRoutes []string
}

// Fingerprint keeps only global IPv4 addresses and default routes. Link-local
// interfaces such as awdl0 flap constantly, and IPv6 privacy addresses rotate
// without breaking existing connections.
func (s Snapshot) Fingerprint() string {
	parts := make([]string, 0, len(s.Addrs)+len(s.DefaultRoutes))
	for _, a := range s.Addrs {
		if !a.Addr.Is4() || !a.Addr.IsGlobalUnicast() {
			continue
		}
		parts = append(parts, "addr "+a.Iface+" "+a.Addr.String())
	}
	for _, r := range s.DefaultRoutes {
		parts = append(parts, "route "+r)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

type Watcher struct {
	Ticks   <-chan time.Time
	Clock   func() Sample
	Network func() (Snapshot, error)
	Wake    WakeDetector
	// Out receives trigger reasons; sends never block, a pending trigger absorbs new ones.
	Out chan<- string
}

func (w *Watcher) Run(ctx context.Context) error {
	w.Wake.Observe(w.Clock())
	snap, err := w.Network()
	if err != nil {
		return fmt.Errorf("reading network state: %w", err)
	}
	fingerprint := snap.Fingerprint()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.Ticks:
		}
		if gap, woke := w.Wake.Observe(w.Clock()); woke {
			w.emit(fmt.Sprintf("wake (clock jumped %s)", gap.Round(time.Second)))
		}
		snap, err := w.Network()
		if err != nil {
			return fmt.Errorf("reading network state: %w", err)
		}
		if next := snap.Fingerprint(); next != fingerprint {
			fingerprint = next
			w.emit("network change")
		}
	}
}

func (w *Watcher) emit(reason string) {
	select {
	case w.Out <- reason:
	default:
	}
}

// SystemClock samples the real clocks relative to start.
func SystemClock(start time.Time) func() Sample {
	return func() Sample {
		now := time.Now()
		return Sample{Wall: now.Round(0), Mono: now.Sub(start)}
	}
}
