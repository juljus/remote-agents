//go:build e2e

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"remote-agents/internal/laptopfiles"
)

// approver, in the e2e tests' build, runs the command RA_E2E_APPROVER names
// with the reason as its argument, and its exit status answers: nobody is
// at the laptop to touch the sensor.
func approver() laptopfiles.Approver {
	command := os.Getenv("RA_E2E_APPROVER")
	return laptopfiles.ApproverFunc(func(ctx context.Context, reason string) error {
		if out, err := exec.CommandContext(ctx, command, reason).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", command, err, out)
		}
		return nil
	})
}
