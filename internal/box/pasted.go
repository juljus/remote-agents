package box

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/pastefile"
)

// StorePasted keeps each image file the laptop sends on r in the pasted dir,
// named by its content hash, and writes its path to w, one per line. Files
// pasted more than markerTTL ago go first; pasting one again keeps it.
func StorePasted(l boxlayout.Layout, r io.Reader, w io.Writer, now time.Time) error {
	dir := l.PastedDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating pasted dir: %w", err)
	}
	if err := pruneOld(dir, now); err != nil {
		return err
	}
	frames := pastefile.NewReader(r)
	for {
		data, err := frames.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		path, err := storePasted(dir, data, now)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, path); err != nil {
			return err
		}
	}
}

func storePasted(dir string, data []byte, now time.Time) (string, error) {
	ext, ok := pastefile.ImageExt(data[:min(len(data), pastefile.SniffLen)])
	if !ok {
		return "", errors.New("pasted file is not an image")
	}
	sum := sha256.Sum256(data)
	path := filepath.Join(dir, hex.EncodeToString(sum[:])+"."+ext)
	err := os.Chtimes(path, now, now)
	if err == nil {
		return path, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("refreshing pasted file: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".paste-*")
	if err != nil {
		return "", fmt.Errorf("creating pasted file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return "", errors.Join(fmt.Errorf("writing pasted file: %w", err), tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return "", errors.Join(fmt.Errorf("writing pasted file: %w", err), os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", errors.Join(fmt.Errorf("committing pasted file: %w", err), os.Remove(tmp.Name()))
	}
	return path, nil
}
