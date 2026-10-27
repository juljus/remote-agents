//go:build !linux

package box

import "errors"

// The box side runs on Linux; other systems only build it for tests.
func becomeSubreaper() error { return errors.New("child subreapers need Linux") }
