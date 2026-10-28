// Package sshrun builds and runs the ssh commands that carry sessions and the link.
package sshrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/reconnect"
)

// commonOptions make a dead link visible within seconds, never prompt during
// a reconnect, and keep each connection independent of any ControlMaster in
// the user's ssh config, which could still hold a dead socket.
var commonOptions = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=4",
	"-o", "ServerAliveInterval=2",
	"-o", "ServerAliveCountMax=2",
	"-o", "ControlMaster=no",
	"-o", "ControlPath=none",
}

// Box is an ssh destination and the remote-agents install to run there.
type Box struct {
	Host    string
	Install string
}

func (b Box) command(args ...string) string {
	return `exec "$HOME/` + boxlayout.BinaryRel(b.Install) + `" ` + strings.Join(args, " ")
}

func argv(ssh string, opts []string, host, remote string) []string {
	out := append([]string{ssh}, commonOptions...)
	out = append(out, opts...)
	return append(out, host, remote)
}

// SessionArgv disables the escape character so every keystroke reaches the agent.
func SessionArgv(ssh string, b Box, token string) []string {
	return argv(ssh, []string{"-tt", "-e", "none"}, b.Host, b.command("attach", token))
}

// PasteArgv stores the image files framed on its stdin in the box's pasted dir.
func PasteArgv(ssh string, b Box) []string {
	return argv(ssh, []string{"-T"}, b.Host, b.command("paste-store"))
}

func ExecArgv(ssh string, b Box, token string, tty bool) []string {
	opts := []string{"-T"}
	if tty {
		opts = []string{"-tt", "-e", "none"}
	}
	return argv(ssh, opts, b.Host, b.command("exec", token))
}

// LinkArgv carries the shim relay over the session's stdio; sshd on the box
// would create a -R unix socket owned by root, unusable by the box user.
func LinkArgv(ssh string, b Box) []string {
	return argv(ssh, []string{"-T"}, b.Host, b.command("link-hold"))
}

// TunnelArgv connects ssh's stdio to the box's localhost:port, one TCP connection per ssh.
func TunnelArgv(ssh, host string, port uint16) []string {
	out := append([]string{ssh}, commonOptions...)
	return append(out, "-W", fmt.Sprintf("localhost:%d", port), host)
}

func ListeningArgv(ssh string, b Box, port uint16) []string {
	return argv(ssh, []string{"-T"}, b.Host, b.command("listening", fmt.Sprint(port)))
}

func ListArgv(ssh string, b Box) []string {
	return argv(ssh, []string{"-T"}, b.Host, b.command("ls"))
}

type Stdio struct {
	Stdin  io.Reader
	Stdout io.Writer
	// Stderr, when set, also receives ssh's stderr; it is always captured for Detail.
	Stderr io.Writer
	// Terminal is set when Stdin is a terminal of ssh's own. ssh then runs in
	// a session of its own with it as the controlling terminal, so resizing
	// the terminal signals ssh.
	Terminal bool
}

// lastLine keeps the last non-empty line written to it.
type lastLine struct {
	mu   sync.Mutex
	buf  []byte
	last string
}

func (l *lastLine) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(string(l.buf[:i])); line != "" {
			l.last = line
		}
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 4096 {
		l.buf = l.buf[len(l.buf)-4096:]
	}
	return len(p), nil
}

func (l *lastLine) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rest := strings.TrimSpace(string(l.buf)); rest != "" {
		return rest
	}
	return l.last
}

// Run runs one ssh command. ssh's exit status 255 and death by signal are
// transport failures; any other status is the remote command's. When ctx is
// cancelled ssh gets SIGTERM, so it restores the terminal, then SIGKILL after killAfter.
func Run(ctx context.Context, argv []string, stdio Stdio, killAfter time.Duration) reconnect.Result {
	stderr := &lastLine{}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout = stdio.Stdin, stdio.Stdout
	if stdio.Terminal {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	}
	cmd.Stderr = stderr
	if stdio.Stderr != nil {
		cmd.Stderr = io.MultiWriter(stdio.Stderr, stderr)
	}
	// A child of ssh, such as a LocalCommand, can inherit the output pipes and outlive it.
	cmd.WaitDelay = killAfter
	if err := cmd.Start(); err != nil {
		return reconnect.Result{Err: fmt.Errorf("starting ssh: %w", err)}
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var waitErr error
	cancelled := false
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		cancelled = true
		waitErr = terminate(cmd, waited, killAfter)
	}
	return classify(cmd, waitErr, cancelled, stderr.String())
}

func terminate(cmd *exec.Cmd, waited <-chan error, killAfter time.Duration) error {
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("terminating ssh: %w", err)
	}
	select {
	case err := <-waited:
		return err
	case <-time.After(killAfter):
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("killing ssh: %w", err)
	}
	return <-waited
}

func classify(cmd *exec.Cmd, waitErr error, cancelled bool, detail string) reconnect.Result {
	var exitErr *exec.ExitError
	// ErrWaitDelay: ssh exited 0 but something else still held its output; ssh's status stands.
	if waitErr != nil && !errors.As(waitErr, &exitErr) && !errors.Is(waitErr, exec.ErrWaitDelay) {
		return reconnect.Result{Err: waitErr}
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		return reconnect.Result{Err: fmt.Errorf("unexpected process state %T", cmd.ProcessState.Sys())}
	}
	switch {
	case cancelled:
		return reconnect.Result{Transport: true, Detail: "connection closed"}
	case status.Signaled():
		return reconnect.Result{Transport: true, Detail: fmt.Sprintf("ssh ended by signal %d (%s)", status.Signal(), status.Signal())}
	case status.ExitStatus() == 255:
		if detail == "" {
			detail = "ssh exited with status 255"
		}
		return reconnect.Result{Transport: true, Detail: detail}
	}
	return reconnect.Result{ExitCode: status.ExitStatus()}
}
