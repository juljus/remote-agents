//go:build !e2e

package main

import (
	"context"
	"errors"
	"fmt"

	"remote-agents/internal/laptopfiles"
	"remote-agents/internal/touchid"
)

// approver asks the user at the laptop with Touch ID, or their password.
func approver() laptopfiles.Approver {
	return laptopfiles.ApproverFunc(func(ctx context.Context, reason string) error {
		err := touchid.Ask(ctx, reason)
		if e := (touchid.Error{}); errors.As(err, &e) && e.Cancelled() {
			return fmt.Errorf("%w: %v", laptopfiles.ErrDeclined, err)
		}
		return err
	})
}
