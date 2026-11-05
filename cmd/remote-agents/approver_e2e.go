//go:build e2e

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"remote-agents/internal/laptopfiles"
)

// approver, in the e2e tests' build, runs the command RA_E2E_APPROVER names
// with the reason as its argument, and its exit status answers, non-zero for
// the user's no: nobody is at the laptop to touch the sensor.
func approver() laptopfiles.Approver {
	command := os.Getenv("RA_E2E_APPROVER")
	return laptopfiles.ApproverFunc(func(ctx context.Context, reason string) error {
		out, err := exec.CommandContext(ctx, command, reason).CombinedOutput()
		if exit := (*exec.ExitError)(nil); errors.As(err, &exit) && ctx.Err() == nil {
			return fmt.Errorf("%w: %s: %v: %s", laptopfiles.ErrDeclined, command, err, out)
		}
		if err != nil {
			return fmt.Errorf("%s: %w: %s", command, err, out)
		}
		return nil
	})
}
