// Package hookexec runs a relayed cmux hook command in the wrapper's own
// terminal context, which cmux's socket only accepts from its descendants.
package hookexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"

	"remote-agents/internal/linkproto"
)

// BinEnvVar names the cmux CLI that cmux's hook settings invoke.
const BinEnvVar = "CMUX_CLAUDE_HOOK_CMUX_BIN"

// Hook is a cmux CLI call that one of cmux's injected hook commands makes.
type Hook struct {
	Argv []string
	// Env holds the variables cmux's command sets for the call.
	Env map[string]string
	// Quiet is set when cmux's command answers "{}" in place of the call's
	// failure, so that a cmux hiccup never fails the agent's hook.
	Quiet bool
}

// quietAnswer is what a quiet hook prints when its call fails.
var quietAnswer = []byte("{}\n")

type Executor struct {
	// BaseEnv is the wrapper's environment; Env, cmux's launch environment
	// for the hook's argument list, overrides it.
	BaseEnv []string
	Env     func(argv []string) map[string]string
	// Allowed are the hooks cmux injected. The cmux CLI can also drive
	// terminals, so the box may reach nothing else.
	Allowed []Hook
}

func (e Executor) Run(ctx context.Context, req linkproto.HookRequest) (linkproto.HookResult, error) {
	i := slices.IndexFunc(e.Allowed, func(h Hook) bool { return slices.Equal(h.Argv, req.Argv) })
	if i < 0 {
		return linkproto.HookResult{}, fmt.Errorf("refusing cmux %q: not a hook cmux injected", req.Argv)
	}
	hook := e.Allowed[i]
	res, err := e.run(ctx, hook, req)
	if hook.Quiet && (err != nil || res.ExitCode != 0) {
		return linkproto.HookResult{Stdout: quietAnswer}, nil
	}
	return res, err
}

func (e Executor) run(ctx context.Context, hook Hook, req linkproto.HookRequest) (linkproto.HookResult, error) {
	env := e.Env(req.Argv)
	bin := env[BinEnvVar]
	if !filepath.IsAbs(bin) {
		return linkproto.HookResult{}, fmt.Errorf("%s must be an absolute path, got %q", BinEnvVar, bin)
	}
	cmd := exec.CommandContext(ctx, bin, req.Argv...)
	cmd.Env = append([]string(nil), e.BaseEnv...)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range hook.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(req.Stdin)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return linkproto.HookResult{}, fmt.Errorf("running %s: %w", bin, err)
	}
	code := 0
	if exitErr != nil {
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok {
			return linkproto.HookResult{}, fmt.Errorf("unexpected process state %T", exitErr.Sys())
		}
		if status.Signaled() {
			return linkproto.HookResult{}, fmt.Errorf("%s killed by %v", bin, status.Signal())
		}
		code = status.ExitStatus()
	}
	return linkproto.HookResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: code}, nil
}
