package linkproto

import (
	"os"
	"testing"
)

// shortTempDir keeps unix socket paths under the 104-byte sun_path limit on macOS.
func shortTempDir(t *testing.T) (string, error) {
	dir, err := os.MkdirTemp("/tmp", "ra-test-")
	if err != nil {
		return "", err
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir, nil
}
