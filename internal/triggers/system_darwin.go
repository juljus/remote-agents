package triggers

import (
	"context"
	"time"
)

// WatchSystem emits wake and network-change triggers on out until ctx ends.
func WatchSystem(ctx context.Context, out chan<- string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	w := Watcher{
		Ticks:   ticker.C,
		Clock:   SystemClock(time.Now()),
		Network: SystemNetwork,
		Wake:    WakeDetector{Threshold: 3 * time.Second},
		Out:     out,
	}
	return w.Run(ctx)
}
