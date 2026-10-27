// Command remote-agents-box is the box side of remote-agents. Invoked as
// xclip, wl-paste, xdg-open or cmux (through symlinks) it acts as that shim.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"remote-agents/internal/box"
	"remote-agents/internal/boxlayout"
	"remote-agents/internal/linkproto"
	"remote-agents/internal/spec"
)

const shimTimeout = 15 * time.Second

// forgetTimeout bounds the wait for a reaped session's launcher to exit.
const forgetTimeout = time.Minute

func main() {
	os.Exit(run())
}

func run() int {
	layout, err := installLayout()
	if err != nil {
		fmt.Fprintf(os.Stderr, "remote-agents-box: %v\n", err)
		return 1
	}
	stdio := box.IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}

	if name := filepath.Base(os.Args[0]); slices.Contains(box.ShimNames, name) {
		return runShim(name, layout, stdio)
	}
	code, err := runCommand(layout, stdio, os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "remote-agents-box: %v\n", err)
		return 1
	}
	return code
}

// installLayout is the install this binary runs from; shims reach it through symlinks.
func installLayout() (boxlayout.Layout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return boxlayout.Layout{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return boxlayout.Layout{}, err
	}
	return boxlayout.FromExecutable(home, exe)
}

func runShim(name string, layout boxlayout.Layout, stdio box.IO) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// Hooks are bounded by the agent's own hook timeout; a permission prompt may legitimately wait long.
	if name != "cmux" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shimTimeout)
		defer cancel()
	}
	laptop := linkproto.Client{Socket: layout.LaptopSocket(), DialTimeout: 3 * time.Second}
	return box.RunShim(ctx, name, os.Args[1:], stdio, laptop, os.Getenv)
}

// runCommand returns the process exit code; launch passes the agent's code through.
func runCommand(layout boxlayout.Layout, stdio box.IO, args []string) (int, error) {
	if len(args) == 0 {
		return 1, errors.New("usage: remote-agents-box attach|launch|exec <spec> | ls | link-hold | activity | listening <port>")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "attach", "launch", "exec":
		if len(rest) != 1 {
			return 1, fmt.Errorf("usage: remote-agents-box %s <spec>", cmd)
		}
		s, err := spec.Decode(rest[0])
		if err != nil {
			return 1, err
		}
		switch cmd {
		case "attach":
			return box.Attach(layout, s, stdio)
		case "exec":
			return 1, box.Exec(layout, s)
		}
		return box.Launch(layout, s, stdio, os.Environ()), nil
	case "ls":
		if len(rest) != 0 {
			return 1, errors.New("usage: remote-agents-box ls")
		}
		return 0, box.List(layout, os.Stdout)
	case "link-hold":
		if len(rest) != 0 {
			return 1, errors.New("usage: remote-agents-box link-hold")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
		defer stop()
		logf := func(err error) { fmt.Fprintf(os.Stderr, "remote-agents-box link-hold: %v\n", err) }
		return 0, box.LinkHold(ctx, layout, os.Stdin, os.Stdout, linkproto.HeartbeatTimeout, logf)
	case "activity":
		if len(rest) != 0 {
			return 1, errors.New("usage: remote-agents-box activity")
		}
		// Exit status 1 shows Claude a hook error without blocking the turn; 2 would block it.
		return 0, box.RecordActivity(layout, os.Getenv, os.Stdin, time.Now())
	case "forget":
		if len(rest) != 2 {
			return 1, errors.New("usage: remote-agents-box forget <session> <shell pid>")
		}
		shell, err := strconv.Atoi(rest[1])
		if err != nil {
			return 1, fmt.Errorf("invalid shell pid %q", rest[1])
		}
		return 0, box.Forget(layout, rest[0], shell, forgetTimeout)
	case "listening":
		if len(rest) != 1 {
			return 1, errors.New("usage: remote-agents-box listening <port>")
		}
		return listening(rest[0])
	}
	return 1, fmt.Errorf("unknown command %q", cmd)
}

func listening(arg string) (int, error) {
	port, err := strconv.ParseUint(arg, 10, 16)
	if err != nil {
		return 1, fmt.Errorf("invalid port %q", arg)
	}
	tcp, err := os.Open("/proc/net/tcp")
	if err != nil {
		return 1, err
	}
	defer tcp.Close()
	tcp6, err := os.Open("/proc/net/tcp6")
	if err != nil {
		return 1, err
	}
	defer tcp6.Close()
	found, err := box.ListeningOnLocalhost(uint16(port), tcp, tcp6)
	if err != nil {
		return 1, err
	}
	if !found {
		return spec.ExitNotListening, nil
	}
	return 0, nil
}
