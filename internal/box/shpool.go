package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/reaper"
	"remote-agents/internal/spec"
)

func shpoolBase(l boxlayout.Layout) []string {
	return []string{l.Shpool(), "--socket", l.ShpoolSocket(), "--config-file", l.ShpoolConfig(), "--no-daemonize"}
}

// shpoolAttachArgv force-attaches so a stale attachment from a dead link is
// replaced. The launcher command only runs when the session does not exist yet.
func shpoolAttachArgv(l boxlayout.Layout, session, token string) []string {
	return append(shpoolBase(l), "attach", "--force", "--cmd", l.Binary()+" launch "+token, session)
}

// Attach runs shpool attached to the spec's session and returns its exit
// code. shpool also exits 0 when another terminal takes the session over, so
// a session still running after a clean exit is reported as taken over. A
// stale attempt of a run, one a newer attempt replaced, attaches nothing; if
// one took the session between its claim and its attach, Attach takes the
// session back.
func Attach(l boxlayout.Layout, s spec.Spec, stdio IO) (int, error) {
	if s.Mode != spec.ModeSession && s.Mode != spec.ModeReattach {
		return 1, fmt.Errorf("attach needs a session or reattach spec, got mode %q", s.Mode)
	}
	token, err := s.Encode()
	if err != nil {
		return 1, err
	}
	claimed, err := claimAttachment(l, s)
	if err != nil {
		return 1, err
	}
	if !claimed {
		fmt.Fprintf(stdio.Stderr, "\r\nremote-agents: a newer connection attempt holds %s\r\n", s.Session)
		return spec.ExitStaleAttempt, nil
	}
	argv := shpoolAttachArgv(l, s.Session, token)
	for {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr
		err = cmd.Run()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			return 1, fmt.Errorf("running shpool: %w", err)
		}
		code := cmd.ProcessState.ExitCode()
		if code == 255 {
			fmt.Fprintf(stdio.Stderr, "\r\nremote-agents: the agent exited with status 255, reported as %d\r\n", spec.ExitAgent255)
			return spec.ExitAgent255, nil
		}
		if code != 0 {
			return code, nil
		}
		running, err := outlives(l, s.Session, takeoverGrace)
		if err != nil {
			return 1, err
		}
		if !running {
			return 0, nil
		}
		// Every other attachment claims the session first, except a stale attempt of this run.
		owns, err := ownsAttachment(l, s)
		if err != nil {
			return 1, err
		}
		if !owns {
			fmt.Fprintf(stdio.Stderr, "\r\nremote-agents: another terminal took over %s; it keeps running\r\n", s.Session)
			return spec.ExitTakenOver, nil
		}
	}
}

// takeoverGrace covers shpool still tearing down a session whose agent just exited.
const takeoverGrace = 2 * time.Second

// outlives reports whether session is still running after grace.
func outlives(l boxlayout.Layout, session string, grace time.Duration) (bool, error) {
	deadline := time.Now().Add(grace)
	for {
		sessions, err := listSessions(l)
		if err != nil {
			return false, err
		}
		if !slices.ContainsFunc(sessions, func(s shpoolSession) bool { return s.Name == session }) {
			return false, nil
		}
		if time.Now().After(deadline) {
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Exec replaces the process with the agent, without shpool.
func Exec(l boxlayout.Layout, s spec.Spec) error {
	if s.Mode != spec.ModeExec {
		return fmt.Errorf("exec needs an exec spec, got mode %q", s.Mode)
	}
	path, env, err := prepareAgent(l, s, os.Environ())
	if err != nil {
		return err
	}
	if err := os.Chdir(s.Workdir); err != nil {
		return fmt.Errorf("entering workdir: %w", err)
	}
	if err := syscall.Exec(path, s.Argv, env); err != nil {
		return fmt.Errorf("exec %s: %w", path, err)
	}
	return nil
}

type shpoolSession struct {
	Name            string `json:"name"`
	StartedAtUnixMS int64  `json:"started_at_unix_ms"`
	// LastDisconnectedAtUnixMS is null until a client first detaches.
	LastDisconnectedAtUnixMS *int64 `json:"last_disconnected_at_unix_ms"`
	Status                   string `json:"status"`
	ShellPID                 int    `json:"shell_pid"`
}

// detachedSince is when the session lost its last client.
func (s shpoolSession) detachedSince() time.Time {
	if s.LastDisconnectedAtUnixMS == nil {
		return time.UnixMilli(s.StartedAtUnixMS)
	}
	return time.UnixMilli(*s.LastDisconnectedAtUnixMS)
}

func parseSessions(raw []byte) ([]shpoolSession, error) {
	var reply struct {
		Sessions []shpoolSession `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("parsing shpool list: %w", err)
	}
	for _, s := range reply.Sessions {
		if s.Status != "Attached" && s.Status != "Disconnected" {
			return nil, fmt.Errorf("session %s has unknown status %q", s.Name, s.Status)
		}
	}
	sort.Slice(reply.Sessions, func(i, j int) bool { return reply.Sessions[i].StartedAtUnixMS < reply.Sessions[j].StartedAtUnixMS })
	return reply.Sessions, nil
}

func formatSessions(w io.Writer, sessions []shpoolSession, statuses map[string]reaper.Status, now time.Time, loc *time.Location) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tSTATUS\tACTIVITY\tDETACHED\tSTARTED")
	for _, s := range sessions {
		status, detached := "detached", shortDuration(now.Sub(s.detachedSince()))
		if s.Status == "Attached" {
			status, detached = "attached", "-"
		}
		activity := "-"
		if st, ok := statuses[s.Name]; ok {
			switch {
			case len(st.Working) > 0:
				activity = "working: " + strings.Join(st.Working, ", ")
			case st.IdleSince != 0:
				activity = "idle " + shortDuration(now.Sub(time.UnixMilli(st.IdleSince)))
			default:
				activity = "idle"
			}
		}
		started := time.UnixMilli(s.StartedAtUnixMS).In(loc).Format("2006-01-02 15:04")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Name, status, activity, detached, started)
	}
	return tw.Flush()
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func listSessions(l boxlayout.Layout) ([]shpoolSession, error) {
	cmd := exec.Command(l.Shpool(), append(shpoolBase(l)[1:], "list", "--json")...)
	cmd.Stderr = os.Stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("shpool list: %w", err)
	}
	return parseSessions(raw)
}

// List prints the running sessions with what their reaper last saw.
func List(l boxlayout.Layout, w io.Writer) error {
	sessions, err := listSessions(l)
	if err != nil {
		return err
	}
	statuses := map[string]reaper.Status{}
	for _, s := range sessions {
		st, found, err := reaper.ReadStatus(l.Status(s.Name))
		if err != nil {
			return err
		}
		if found {
			statuses[s.Name] = st
		}
	}
	now := time.Now()
	if err := formatSessions(w, sessions, statuses, now, time.Local); err != nil {
		return err
	}
	reaped, err := reapedSessions(l)
	if err != nil {
		return err
	}
	return formatReaped(w, reaped, now, time.Local)
}

// formatReaped lists the sessions reaped in the last markerTTL, with the command that resumes each.
func formatReaped(w io.Writer, reaped []reapedSessionRecord, now time.Time, loc *time.Location) error {
	var recent []reapedSessionRecord
	for _, r := range reaped {
		if now.Sub(time.UnixMilli(r.At)) < markerTTL {
			recent = append(recent, r)
		}
	}
	if len(recent) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nREAPED\tENDED\tWHY\tRESUME WITH")
	for _, r := range recent {
		cmd, err := resumeCommand(r)
		if err != nil {
			return err
		}
		why := "idle"
		if r.Reason == reaper.ReapedCap {
			why = "detached too long"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Spec.Session, time.UnixMilli(r.At).In(loc).Format("2006-01-02 15:04"), why, cmd)
	}
	return tw.Flush()
}
