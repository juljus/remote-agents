// Package atomicfile replaces files so that readers see the old content or
// the new, and a crash leaves one of them on disk.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Write replaces path with data and mode, through a temporary file beside it.
func Write(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	committed := false
	defer func() {
		if !committed {
			err = errors.Join(err, os.Remove(f.Name()))
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return errors.Join(fmt.Errorf("writing %s: %w", path, err), f.Close())
	}
	if _, err := f.Write(data); err != nil {
		return errors.Join(fmt.Errorf("writing %s: %w", path, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("writing %s: %w", path, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("committing %s: %w", path, err)
	}
	committed = true
	return SyncDir(dir)
}

// Remove removes path and makes the removal durable.
func Remove(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir makes the renames and removals in dir durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		return errors.Join(fmt.Errorf("syncing %s: %w", dir, err), d.Close())
	}
	return d.Close()
}
