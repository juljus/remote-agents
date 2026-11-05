//go:build !e2e

package main

import (
	"remote-agents/internal/laptopfiles"
	"remote-agents/internal/touchid"
)

// approver asks the user at the laptop with Touch ID, or their password.
func approver() laptopfiles.Approver { return laptopfiles.ApproverFunc(touchid.Ask) }
