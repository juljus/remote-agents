package box

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/reaper"
)

// A reaped agent gets SIGINT, as Ctrl+C or /exit would end it, then
// SIGTERM and SIGKILL if it is still running after each grace period.
const (
	interruptGrace = 10 * time.Second
	terminateGrace = 5 * time.Second
)

// transcriptTail is how much of the transcript the interrupt check reads.
const transcriptTail = 256 << 10

type reapRequest struct {
	reason        reaper.Reason
	claudeSession string
}

// watcher runs in the launcher beside a Claude agent and asks for the agent
// to be ended once the reaper's policy says so.
type watcher struct {
	l       boxlayout.Layout
	cfg     reaper.Config
	session string
	self    int
	timer   *reaper.Timer
	cpu     reaper.CPUMeter
	// lastErr and unknownID keep a failing check from repeating its log line.
	lastErr   string
	unknownID bool
	stop      chan struct{}
	stopped   chan struct{}
}

// newWatcher makes the launcher the subreaper of the agent's processes, so
// a tool's background process stays visible once its shell has exited.
func newWatcher(l boxlayout.Layout, session string, now time.Time) (*watcher, error) {
	cfg, err := reaper.LoadConfig(l.ReaperConfig())
	if err != nil {
		return nil, err
	}
	if err := becomeSubreaper(); err != nil {
		return nil, fmt.Errorf("becoming subreaper: %w", err)
	}
	if err := os.MkdirAll(l.ActivityDir(), 0o700); err != nil {
		return nil, fmt.Errorf("creating activity dir: %w", err)
	}
	// A previous run of the same session left its reports behind.
	for _, path := range []string{l.HookState(session), l.Status(session)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("clearing %s: %w", filepath.Base(path), err)
		}
	}
	return &watcher{
		l: l, cfg: cfg, session: session, self: os.Getpid(), timer: reaper.NewTimer(cfg, now),
		stop: make(chan struct{}), stopped: make(chan struct{}),
	}, nil
}

func (w *watcher) log(format string, args ...any) {
	if err := reaperLog(w.l, time.Now(), w.session+": "+format, args...); err != nil {
		fmt.Fprintf(os.Stderr, "remote-agents: %v\n", err)
	}
}

// start checks the session every poll until it asks for a reap or is stopped.
func (w *watcher) start(agent int) <-chan reapRequest {
	reap := make(chan reapRequest, 1)
	go func() {
		defer close(w.stopped)
		tick := time.NewTicker(w.cfg.Poll)
		defer tick.Stop()
		for {
			select {
			case <-w.stop:
				return
			case now := <-tick.C:
				req, err := w.check(now, agent)
				if err != nil {
					if err.Error() != w.lastErr {
						w.log("check failed, not ending the session: %v", err)
					}
					w.lastErr = err.Error()
					continue
				}
				w.lastErr = ""
				if req.reason != "" {
					reap <- req
					return
				}
			}
		}
	}()
	return reap
}

// finish stops the checks and removes the session's reports.
func (w *watcher) finish() {
	close(w.stop)
	<-w.stopped
	for _, path := range []string{w.l.HookState(w.session), w.l.HookState(w.session) + ".lock", w.l.Status(w.session)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			w.log("removing %s: %v", filepath.Base(path), err)
		}
	}
}

func (w *watcher) check(now time.Time, agent int) (reapRequest, error) {
	sessions, err := listSessions(w.l)
	if err != nil {
		return reapRequest{}, err
	}
	var sess *shpoolSession
	for i := range sessions {
		if sessions[i].Name == w.session {
			sess = &sessions[i]
		}
	}
	if sess == nil {
		return reapRequest{}, fmt.Errorf("shpool does not list %s", w.session)
	}
	// Processes are read after shpool list has been waited for, so every
	// zombie child besides the agent is an adopted orphan, never our own command.
	procs, err := reaper.ReadProcs(os.DirFS("/proc"))
	if err != nil {
		return reapRequest{}, err
	}
	if err := w.reapAdopted(procs, agent); err != nil {
		return reapRequest{}, err
	}
	obs, err := w.observe(procs, agent)
	if err != nil {
		return reapRequest{}, err
	}
	working := obs.Working()
	d := w.timer.Step(now, sess.Status == "Attached", sess.detachedSince(), len(working) > 0, obs.LastWrite)
	status := reaper.Status{Working: working, Updated: now.UnixMilli()}
	if !d.IdleSince.IsZero() {
		status.IdleSince = d.IdleSince.UnixMilli()
	}
	if err := reaper.WriteStatus(w.l.Status(w.session), status); err != nil {
		return reapRequest{}, err
	}
	if d.Reap == "" {
		return reapRequest{}, nil
	}
	if obs.Hooks.ClaudeSession == "" {
		if !w.unknownID {
			w.log("not ending the session (%s): its Claude session id is unknown, so it could not be resumed", d.Reap)
		}
		w.unknownID = true
		return reapRequest{}, nil
	}
	why := "detached since " + sess.detachedSince().UTC().Format(time.RFC3339)
	if d.Reap == reaper.ReapedIdle {
		why += ", idle since " + d.IdleSince.UTC().Format(time.RFC3339)
	}
	w.log("ending Claude session %s (%s): %s", obs.Hooks.ClaudeSession, d.Reap, why)
	return reapRequest{reason: d.Reap, claudeSession: obs.Hooks.ClaudeSession}, nil
}

func (w *watcher) observe(procs []reaper.Proc, agent int) (reaper.Observation, error) {
	hooks, err := reaper.ReadHookState(w.l.HookState(w.session))
	if err != nil {
		return reaper.Observation{}, err
	}
	// A self-detached daemon may be busy for another session; only the session's own leftovers count.
	own, _ := reaper.Leftovers(procs, w.self, agent)
	obs := reaper.Observation{
		Hooks:         hooks,
		ToolShells:    reaper.ToolShells(procs, agent),
		BusyLeftovers: w.cpu.Used(own) >= reaper.BusyTicks(w.cfg.Poll),
	}
	if hooks.Transcript == "" {
		return obs, nil
	}
	if obs.LastWrite, err = lastWrite(hooks.Transcript); err != nil {
		return reaper.Observation{}, err
	}
	if hooks.TurnOpen() {
		if obs.Interrupted, err = interrupted(hooks.Transcript, time.UnixMilli(hooks.TurnStarted)); err != nil {
			return reaper.Observation{}, err
		}
	}
	return obs, nil
}

// reapAdopted collects orphans the launcher adopted as subreaper once they exit.
func (w *watcher) reapAdopted(procs []reaper.Proc, agent int) error {
	for _, p := range procs {
		if p.PPID != w.self || p.PID == agent || !p.Zombie {
			continue
		}
		var status syscall.WaitStatus
		if _, err := syscall.Wait4(p.PID, &status, syscall.WNOHANG, nil); err != nil {
			return fmt.Errorf("collecting exited process %d: %w", p.PID, err)
		}
	}
	return nil
}

// sweep ends the processes left in the session once a reaped agent has
// exited, MCP servers it orphaned included: a reaped session leaves nothing
// of its own running on the box. Each gets SIGTERM, then SIGKILL after
// terminateGrace. Self-detached daemons are spared, since others may use them.
func (w *watcher) sweep(agent int) {
	deadline := time.Now().Add(terminateGrace)
	signalled := map[int]syscall.Signal{}
	spared := map[int]bool{}
	for {
		procs, err := reaper.ReadProcs(os.DirFS("/proc"))
		if err == nil {
			err = w.reapAdopted(procs, agent)
		}
		if err != nil {
			w.log("ending left-behind processes: %v", err)
			return
		}
		left, daemons := reaper.Leftovers(procs, w.self, agent)
		for _, d := range daemons {
			if !spared[d.PID] {
				w.log("left running: %d %s (daemon)", d.PID, procName(d))
				spared[d.PID] = true
			}
		}
		if len(left) == 0 {
			return
		}
		sig := syscall.SIGTERM
		if time.Now().After(deadline) {
			sig = syscall.SIGKILL
		}
		var names []string
		for _, p := range left {
			if signalled[p.PID] == sig {
				continue
			}
			// ESRCH: it exited since the listing.
			if err := syscall.Kill(p.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
				w.log("sending %v to %d: %v", sig, p.PID, err)
				return
			}
			signalled[p.PID] = sig
			names = append(names, fmt.Sprintf("%d %s", p.PID, procName(p)))
		}
		if len(names) > 0 {
			w.log("sent %v to left-behind processes: %s", sig, strings.Join(names, ", "))
		}
		if sig == syscall.SIGKILL && len(names) == 0 && time.Now().After(deadline.Add(terminateGrace)) {
			w.log("left-behind processes survived SIGKILL: %d", len(left))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func procName(p reaper.Proc) string {
	if len(p.Args) == 0 {
		return "?"
	}
	return filepath.Base(p.Args[0])
}

// lastWrite is the newest modification of the transcript and its subagents' transcripts.
func lastWrite(transcript string) (time.Time, error) {
	var newest time.Time
	note := func(path string) error {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	}
	if err := note(transcript); err != nil {
		return time.Time{}, err
	}
	dir := filepath.Join(strings.TrimSuffix(transcript, ".jsonl"), "subagents")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return newest, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("listing subagent transcripts: %w", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			if err := note(filepath.Join(dir, e.Name())); err != nil {
				return time.Time{}, err
			}
		}
	}
	return newest, nil
}

func interrupted(transcript string, since time.Time) (bool, error) {
	f, err := os.Open(transcript)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("opening transcript: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat transcript: %w", err)
	}
	offset := max(info.Size()-transcriptTail, 0)
	tail, err := io.ReadAll(io.NewSectionReader(f, offset, info.Size()-offset))
	if err != nil {
		return false, fmt.Errorf("reading transcript: %w", err)
	}
	return reaper.Interrupted(tail, since), nil
}
