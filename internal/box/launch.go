package box

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/jsonfile"
	"remote-agents/internal/reaper"
	"remote-agents/internal/spec"
)

// basePath follows Ubuntu's /etc/environment; the agent's own shell setup extends it.
const basePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

const markerTTL = 7 * 24 * time.Hour

const hookBinEnvVar = "CMUX_CLAUDE_HOOK_CMUX_BIN"

var reservedEnv = map[string]bool{"PATH": true, "BROWSER": true, SessionEnvVar: true, hookBinEnvVar: true}

// AgentEnv is the environment the agent runs with: base (shpool's clean
// session env) plus the spec's terminal variables plus the shim wiring.
func AgentEnv(base []string, l boxlayout.Layout, s spec.Spec) ([]string, error) {
	overrides := map[string]string{}
	for k, v := range s.Env {
		if reservedEnv[k] {
			return nil, fmt.Errorf("spec may not set %s", k)
		}
		overrides[k] = v
	}
	overrides["PATH"] = l.Shims() + ":" + l.ClaudeBin() + ":" + basePath
	overrides["BROWSER"] = filepath.Join(l.Shims(), "xdg-open")
	if s.Session != "" {
		overrides[SessionEnvVar] = s.Session
	}
	if s.HookRelay {
		overrides[hookBinEnvVar] = filepath.Join(l.HookBin(), "cmux")
	}
	env := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, replaced := overrides[k]; !replaced {
			env = append(env, kv)
		}
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env, nil
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// lookPath resolves name against pathList rather than the launcher's own PATH.
func lookPath(name, pathList string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(pathList) {
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found in %s", name, pathList)
}

// prepareAgent checks the workdir and resolves the agent command.
func prepareAgent(l boxlayout.Layout, s spec.Spec, base []string) (string, []string, error) {
	info, err := os.Stat(s.Workdir)
	if err != nil {
		return "", nil, fmt.Errorf("remote workdir: %w", err)
	}
	if !info.IsDir() {
		return "", nil, fmt.Errorf("remote workdir %s is not a directory", s.Workdir)
	}
	env, err := AgentEnv(base, l, s)
	if err != nil {
		return "", nil, err
	}
	path, err := lookPath(s.Argv[0], envValue(env, "PATH"))
	if err != nil {
		return "", nil, err
	}
	return path, env, nil
}

type marker struct {
	ExitCode int `json:"exit_code"`
	// Reason is set when the reaper ended the agent; ClaudeSession is then the session to resume.
	Reason        reaper.Reason `json:"reason,omitempty"`
	ClaudeSession string        `json:"claude_session,omitempty"`
}

// Launch runs inside the shpool session. It returns the exit code for the session.
func Launch(l boxlayout.Layout, s spec.Spec, stdio IO, base []string) int {
	fail := func(err error) int {
		fmt.Fprintf(stdio.Stderr, "remote-agents: %v\n", err)
		return 1
	}
	if s.Mode == spec.ModeReattach {
		rec, reaped, err := readReapedSession(l, s.Session)
		if err != nil {
			return fail(err)
		}
		if !reaped {
			return fail(fmt.Errorf("session %s is not running", s.Session))
		}
		if s, err = reattachTarget(rec); err != nil {
			return fail(err)
		}
	}
	if s.Mode != spec.ModeSession {
		return fail(fmt.Errorf("launch needs a session spec, got mode %q", s.Mode))
	}
	now := time.Now()
	for _, dir := range []string{l.EndedDir(), l.ReapedDir(), l.ReapedSessionsDir()} {
		if err := pruneOld(dir, now); err != nil {
			return fail(err)
		}
	}
	if err := withAttachments(l, func() error { return pruneOld(l.AttachmentsDir(), now) }); err != nil {
		return fail(err)
	}
	m, ended, err := readMarker(l, s.RunID)
	if err != nil {
		return fail(err)
	}
	if ended && (m.Reason == "" || s.Claude == nil) {
		fmt.Fprintf(stdio.Stderr, "remote-agents: session %s ended while the link was down (exit code %d)\n", s.Session, m.ExitCode)
		return m.ExitCode
	}
	resumed, err := resumeAfterReap(l, &s, m, ended)
	if err != nil {
		return fail(err)
	}
	path, env, err := prepareAgent(l, s, base)
	if err != nil {
		return fail(err)
	}
	var w *watcher
	if s.Claude != nil {
		if s.Argv, err = withActivityHooks(l, s.Argv, s.Workdir); err != nil {
			return fail(err)
		}
		if w, err = newWatcher(l, s.Session, now); err != nil {
			return fail(err)
		}
	}
	if resumed != "" {
		announceResume(stdio.Stderr, resumed)
	}
	code, reaped, err := runChild(path, s, env, stdio, w)
	if err != nil {
		return fail(err)
	}
	if err := writeMarker(l, s.RunID, marker{ExitCode: code, Reason: reaped.reason, ClaudeSession: reaped.claudeSession}); err != nil {
		return fail(err)
	}
	if reaped.reason != "" {
		if err := writeReaped(l, s, reaped.claudeSession, reaped.reason, time.Now()); err != nil {
			return fail(err)
		}
		if err := forgetAfterExit(l, s.Session); err != nil {
			return fail(err)
		}
	}
	return code
}

// resumeAfterReap points s at the Claude session the reaper ended, when the
// run's marker says so, and reports the reap a resume of it follows.
func resumeAfterReap(l boxlayout.Layout, s *spec.Spec, m marker, ended bool) (reaper.Reason, error) {
	if ended {
		s.Argv = slices.Concat(s.Claude.Resume, []string{"--resume", m.ClaudeSession})
		if _, err := takeReaped(l, m.ClaudeSession); err != nil {
			return "", err
		}
		return m.Reason, nil
	}
	if s.Claude == nil || s.Claude.Resumes == "" {
		return "", nil
	}
	return takeReaped(l, s.Claude.Resumes)
}

// runChild runs the agent until it exits. With a watcher, the agent is ended
// when the watcher asks; the request is returned with the exit code.
func runChild(path string, s spec.Spec, env []string, stdio IO, w *watcher) (int, reapRequest, error) {
	cmd := exec.Command(path, s.Argv[1:]...)
	cmd.Args[0] = s.Argv[0]
	cmd.Dir = s.Workdir
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return 0, reapRequest{}, fmt.Errorf("starting %s: %w", path, err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var reap <-chan reapRequest
	if w != nil {
		reap = w.start(cmd.Process.Pid)
	}
	var reaped reapRequest
	var escalate <-chan time.Time
	next := []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}
	send := func(sig syscall.Signal) error {
		if err := cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("sending %v: %w", sig, err)
		}
		return nil
	}
	for {
		select {
		case sig := <-sigs:
			if err := cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return 0, reapRequest{}, fmt.Errorf("forwarding %v: %w", sig, err)
			}
		case reaped = <-reap:
			reap = nil
			if err := send(syscall.SIGINT); err != nil {
				return 0, reapRequest{}, err
			}
			escalate = time.After(interruptGrace)
		case <-escalate:
			sig := next[0]
			w.log("agent still running after the reap's grace period; sending %v", sig)
			if err := send(sig); err != nil {
				return 0, reapRequest{}, err
			}
			escalate = nil
			if next = next[1:]; len(next) > 0 {
				escalate = time.After(terminateGrace)
			}
		case err := <-waited:
			if w != nil {
				w.finish()
				if reaped.reason != "" {
					w.sweep(cmd.Process.Pid)
				}
			}
			code, err := exitCode(cmd, err)
			return code, reaped, err
		}
	}
}

func exitCode(cmd *exec.Cmd, waitErr error) (int, error) {
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return 0, fmt.Errorf("waiting for agent: %w", waitErr)
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		return 0, fmt.Errorf("unexpected process state %T", cmd.ProcessState.Sys())
	}
	if status.Signaled() {
		return 128 + int(status.Signal()), nil
	}
	return status.ExitStatus(), nil
}

func writeMarker(l boxlayout.Layout, runID string, m marker) error {
	if err := os.MkdirAll(l.EndedDir(), 0o700); err != nil {
		return fmt.Errorf("creating marker dir: %w", err)
	}
	return jsonfile.Write(l.EndedMarker(runID), m)
}

// readMarker leaves the marker in place: the reply carrying the exit code may
// itself be lost to a drop, and the next reconnect must still find it.
func readMarker(l boxlayout.Layout, runID string) (marker, bool, error) {
	var m marker
	found, err := jsonfile.Read(l.EndedMarker(runID), &m)
	return m, found, err
}

// pruneOld removes the files in dir older than markerTTL.
func pruneOld(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing markers: %w", err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("stat marker %s: %w", e.Name(), err)
		}
		if now.Sub(info.ModTime()) > markerTTL {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return fmt.Errorf("pruning marker: %w", err)
			}
		}
	}
	return nil
}
