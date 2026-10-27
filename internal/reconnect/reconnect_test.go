package reconnect

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// script returns attempt results in order; an attempt with block=true waits for ctx cancellation.
type step struct {
	result Result
	block  bool
}

type fakeAttempts struct {
	mu      sync.Mutex
	steps   []step
	calls   int
	started chan int
}

func newFake(steps ...step) *fakeAttempts {
	return &fakeAttempts{steps: steps, started: make(chan int, 16)}
}

func (f *fakeAttempts) attempt(ctx context.Context) Result {
	f.mu.Lock()
	i := f.calls
	f.calls++
	f.mu.Unlock()
	f.started <- i
	if i >= len(f.steps) {
		panic("unexpected extra attempt")
	}
	s := f.steps[i]
	if s.block {
		<-ctx.Done()
		return Result{Transport: true, Detail: "killed"}
	}
	return s.result
}

type recorder struct {
	mu       sync.Mutex
	drops    []int
	triggers []string
	starts   int
	ends     int
}

func (r *recorder) AttemptStarting(int) { r.mu.Lock(); r.starts++; r.mu.Unlock() }
func (r *recorder) AttemptEnded(n int, res Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends++
	if res.Transport {
		r.drops = append(r.drops, n)
	}
}
func (r *recorder) Triggered(reason string) {
	r.mu.Lock()
	r.triggers = append(r.triggers, reason)
	r.mu.Unlock()
}

// manualTimer lets tests decide when the retry interval elapses.
type manualTimer struct{ fire chan time.Time }

func (m manualTimer) after(time.Duration) <-chan time.Time { return m.fire }

type outcome struct {
	code int
	err  error
}

func run(t *testing.T, ctx context.Context, f *fakeAttempts, triggers chan string, timer manualTimer, obs *recorder) chan outcome {
	t.Helper()
	return start(ctx, Loop{Attempt: f.attempt, Triggers: triggers, Interval: time.Second, After: timer.after, Observer: obs})
}

func start(ctx context.Context, loop Loop) chan outcome {
	done := make(chan outcome, 1)
	go func() {
		code, err := loop.Run(ctx)
		done <- outcome{code, err}
	}()
	return done
}

func waitStart(t *testing.T, f *fakeAttempts, want int) {
	t.Helper()
	select {
	case got := <-f.started:
		if got != want {
			t.Fatalf("attempt %d started, want %d", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("attempt %d never started", want)
	}
}

func waitDone(t *testing.T, done chan outcome) outcome {
	t.Helper()
	select {
	case o := <-done:
		return o
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not finish")
	}
	return outcome{}
}

func TestRemoteExitEndsLoopWithCode(t *testing.T) {
	f := newFake(step{result: Result{ExitCode: 3}})
	obs := &recorder{}
	done := run(t, context.Background(), f, make(chan string), manualTimer{make(chan time.Time)}, obs)
	waitStart(t, f, 0)
	if o := waitDone(t, done); o.err != nil || o.code != 3 {
		t.Fatalf("Run = (%d, %v), want (3, nil)", o.code, o.err)
	}
	if len(obs.drops) != 0 {
		t.Fatalf("unexpected drops %v", obs.drops)
	}
}

func TestTransportDropWaitsIntervalThenReconnects(t *testing.T) {
	f := newFake(step{result: Result{Transport: true}}, step{result: Result{ExitCode: 0}})
	timer := manualTimer{make(chan time.Time)}
	obs := &recorder{}
	done := run(t, context.Background(), f, make(chan string), timer, obs)
	waitStart(t, f, 0)
	select {
	case <-f.started:
		t.Fatal("reconnected before the retry interval elapsed")
	case <-time.After(50 * time.Millisecond):
	}
	timer.fire <- time.Now()
	waitStart(t, f, 1)
	if o := waitDone(t, done); o.err != nil {
		t.Fatalf("Run: %v", o.err)
	}
	if len(obs.drops) != 1 || obs.drops[0] != 1 {
		t.Fatalf("drops = %v, want [1]", obs.drops)
	}
	if obs.starts != 2 || obs.ends != 2 {
		t.Fatalf("starts=%d ends=%d, want 2/2", obs.starts, obs.ends)
	}
}

func TestTriggerKillsLiveAttemptAndReconnectsImmediately(t *testing.T) {
	f := newFake(step{block: true}, step{result: Result{ExitCode: 0}})
	triggers := make(chan string, 1)
	obs := &recorder{}
	done := run(t, context.Background(), f, triggers, manualTimer{make(chan time.Time)}, obs)
	waitStart(t, f, 0)
	triggers <- "wake"
	waitStart(t, f, 1)
	if o := waitDone(t, done); o.err != nil {
		t.Fatalf("Run: %v", o.err)
	}
	if len(obs.triggers) != 1 || obs.triggers[0] != "wake" {
		t.Fatalf("triggers = %v", obs.triggers)
	}
}

func TestTriggerCutsRetryWaitShort(t *testing.T) {
	f := newFake(step{result: Result{Transport: true}}, step{result: Result{ExitCode: 0}})
	triggers := make(chan string, 1)
	obs := &recorder{}
	done := run(t, context.Background(), f, triggers, manualTimer{make(chan time.Time)}, obs)
	waitStart(t, f, 0)
	triggers <- "network"
	waitStart(t, f, 1)
	if o := waitDone(t, done); o.err != nil {
		t.Fatalf("Run: %v", o.err)
	}
}

func TestCancelWhileConnectedStopsAttempt(t *testing.T) {
	f := newFake(step{block: true})
	ctx, cancel := context.WithCancel(context.Background())
	done := run(t, ctx, f, make(chan string), manualTimer{make(chan time.Time)}, &recorder{})
	waitStart(t, f, 0)
	cancel()
	if o := waitDone(t, done); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", o.err)
	}
}

func TestCancelWhileWaitingStops(t *testing.T) {
	f := newFake(step{result: Result{Transport: true}})
	ctx, cancel := context.WithCancel(context.Background())
	obs := &recorder{}
	done := run(t, ctx, f, make(chan string), manualTimer{make(chan time.Time)}, obs)
	waitStart(t, f, 0)
	cancel()
	if o := waitDone(t, done); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", o.err)
	}
}

func TestConsecutiveFailuresCountUpAndResetAfterReconnect(t *testing.T) {
	f := newFake(
		step{result: Result{Transport: true}},
		step{result: Result{Transport: true}},
		step{block: true},
		step{result: Result{ExitCode: 0}},
	)
	timer := manualTimer{make(chan time.Time)}
	triggers := make(chan string, 1)
	obs := &recorder{}
	loop := Loop{Attempt: f.attempt, Triggers: triggers, Interval: time.Second, After: timer.after, Observer: obs,
		// An attempt that lasted longer than this counts as having been connected.
		StableAfter: 10 * time.Millisecond}
	done := start(context.Background(), loop)
	waitStart(t, f, 0)
	timer.fire <- time.Now()
	waitStart(t, f, 1)
	timer.fire <- time.Now()
	waitStart(t, f, 2)
	time.Sleep(30 * time.Millisecond)
	triggers <- "wake"
	waitStart(t, f, 3)
	if o := waitDone(t, done); o.err != nil {
		t.Fatalf("Run: %v", o.err)
	}
	want := []int{1, 2, 1}
	if len(obs.drops) != len(want) {
		t.Fatalf("drops = %v, want %v", obs.drops, want)
	}
	for i := range want {
		if obs.drops[i] != want[i] {
			t.Fatalf("drops = %v, want %v", obs.drops, want)
		}
	}
}

func TestFatalAttemptErrorEndsLoop(t *testing.T) {
	boom := errors.New("ssh not installed")
	f := newFake(step{result: Result{Err: boom}})
	done := run(t, context.Background(), f, make(chan string), manualTimer{make(chan time.Time)}, &recorder{})
	waitStart(t, f, 0)
	if o := waitDone(t, done); !errors.Is(o.err, boom) {
		t.Fatalf("err = %v, want %v", o.err, boom)
	}
}
