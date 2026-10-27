package box

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/reaper"
)

// forgetAfterExit starts Forget for this launcher's session. shpool keeps a
// detached session listed after its shell exits, which would show a reaped
// session as still running.
func forgetAfterExit(l boxlayout.Layout, session string) error {
	cmd := exec.Command(l.Binary(), "forget", session, strconv.Itoa(os.Getpid()))
	// Its own session, so the terminal's hangup when this launcher exits does not reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting forget: %w", err)
	}
	return cmd.Process.Release()
}

// Forget waits for shell to exit, then removes session from shpool's table
// unless the entry has since become another shell's. Failures go to the
// reaper log: nothing else sees this process.
func Forget(l boxlayout.Layout, session string, shell int, timeout time.Duration) (err error) {
	defer func() {
		if err != nil {
			err = errors.Join(err, reaperLog(l, time.Now(), "%s: forget: %v", session, err))
		}
	}()
	proc := os.DirFS("/proc")
	deadline := time.Now().Add(timeout)
	for {
		exited, err := reaper.Exited(proc, shell)
		if err != nil {
			return err
		}
		if exited {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("launcher %d still running after %s", shell, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
	sessions, err := listSessions(l)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if s.Name != session || s.ShellPID != shell {
			continue
		}
		cmd := exec.Command(l.Shpool(), append(shpoolBase(l)[1:], "kill", session)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("shpool kill %s: %w: %s", session, err, out)
		}
	}
	return nil
}
