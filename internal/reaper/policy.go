package reaper

import "time"

// Reason records why a session ended without its user.
type Reason string

const (
	ReapedIdle Reason = "reaped-idle"
	ReapedCap  Reason = "reaped-cap"
)

// Observation is one look at a session.
type Observation struct {
	Hooks HookState
	// Interrupted: the user interrupted the open turn, which then sends no Stop.
	Interrupted bool
	ToolShells  int
	// BusyLeftovers: processes the tools left behind used CPU since the last look.
	BusyLeftovers bool
	// LastWrite is the newest write to the session's transcript or its subagents'.
	LastWrite time.Time
}

// waitingOnUser: the open turn asked its user and nothing was written since,
// which would be the user's answer. A turn that waits on its user is idle.
func (o Observation) waitingOnUser() bool {
	w := o.Hooks.WaitingSince
	return w > o.Hooks.TurnStarted && !o.LastWrite.After(time.UnixMilli(w))
}

// Working lists what keeps the session busy; none means idle.
func (o Observation) Working() []string {
	var why []string
	if o.Hooks.TurnOpen() && !o.Interrupted && !o.waitingOnUser() {
		why = append(why, "turn")
	}
	if o.ToolShells > 0 {
		why = append(why, "tool")
	}
	if o.BusyLeftovers {
		why = append(why, "background process")
	}
	if o.Hooks.BackgroundTasks > 0 {
		why = append(why, "background task")
	}
	if o.Hooks.SessionCrons > 0 {
		why = append(why, "scheduled")
	}
	return why
}

// Timer turns observations of one session into the decision to end it.
type Timer struct {
	cfg Config
	// busy is the last time the session was seen working or attached.
	busy time.Time
}

func NewTimer(cfg Config, started time.Time) *Timer { return &Timer{cfg: cfg, busy: started} }

type Decision struct {
	Reap Reason
	// IdleSince is when the detached session went quiet; zero while attached or working.
	IdleSince time.Time
}

// Step decides at now. detachedSince matters only while detached.
func (t *Timer) Step(now time.Time, attached bool, detachedSince time.Time, working bool, lastWrite time.Time) Decision {
	if attached || working {
		t.busy = now
	}
	if attached {
		return Decision{}
	}
	if now.Sub(detachedSince) >= t.cfg.WorkingCap {
		return Decision{Reap: ReapedCap}
	}
	if working {
		return Decision{}
	}
	idle := latest(t.busy, detachedSince, lastWrite)
	if now.Sub(idle) >= t.cfg.IdleAfter {
		return Decision{Reap: ReapedIdle, IdleSince: idle}
	}
	return Decision{IdleSince: idle}
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}
