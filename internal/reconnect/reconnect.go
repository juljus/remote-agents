// Package reconnect runs a link attempt over and over until the remote side
// finishes, reconnecting on transport loss, on a retry interval, and
// immediately on external triggers such as wake from sleep or a network change.
package reconnect

import (
	"context"
	"time"
)

// Result describes how one attempt ended.
type Result struct {
	// Transport is true when the link itself failed; the remote side may still be running.
	Transport bool
	// ExitCode is the remote command's exit status; meaningful only when Transport is false.
	ExitCode int
	// Detail is a human-readable reason for a transport failure.
	Detail string
	// Err is a failure that retrying cannot fix; it ends the loop.
	Err error
}

// Attempt runs one connection. When ctx is cancelled it must tear the
// connection down and return.
type Attempt func(ctx context.Context) Result

type Observer interface {
	AttemptStarting(failures int)
	// AttemptEnded reports the result and the number of consecutive transport failures so far.
	AttemptEnded(failures int, r Result)
	Triggered(reason string)
}

type Loop struct {
	Attempt  Attempt
	Triggers <-chan string
	// Interval is the pause between a transport failure and the next attempt.
	Interval time.Duration
	After    func(time.Duration) <-chan time.Time
	Observer Observer
	// StableAfter is how long an attempt must last to count as having connected,
	// which resets the consecutive failure count.
	StableAfter time.Duration
}

// Run returns the remote exit code once an attempt ends without a transport
// failure, the attempt's Err when it cannot be retried, or ctx.Err() when ctx
// is cancelled.
func (l Loop) Run(ctx context.Context) (int, error) {
	failures := 0
	for {
		l.Observer.AttemptStarting(failures)
		began := time.Now()
		res, triggered, err := l.runAttempt(ctx)
		if err != nil {
			return 0, err
		}
		if res.Err != nil {
			return 0, res.Err
		}
		if !res.Transport {
			l.Observer.AttemptEnded(failures, res)
			return res.ExitCode, nil
		}
		if time.Since(began) >= l.StableAfter {
			failures = 0
		}
		failures++
		l.Observer.AttemptEnded(failures, res)
		if triggered {
			continue
		}
		if err := l.wait(ctx); err != nil {
			return 0, err
		}
	}
}

func (l Loop) runAttempt(ctx context.Context) (Result, bool, error) {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan Result, 1)
	go func() { done <- l.Attempt(attemptCtx) }()
	select {
	case res := <-done:
		return res, false, nil
	case reason := <-l.Triggers:
		l.Observer.Triggered(reason)
		cancel()
		res := <-done
		res.Transport = true
		res.Detail = reason
		return res, true, nil
	case <-ctx.Done():
		<-done
		return Result{}, false, ctx.Err()
	}
}

func (l Loop) wait(ctx context.Context) error {
	select {
	case <-l.After(l.Interval):
		return nil
	case reason := <-l.Triggers:
		l.Observer.Triggered(reason)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
