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
	"strings"
	"syscall"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/pastefile"
)

// pastedTTL is how long a pasted file stays after its last paste.
const pastedTTL = 7 * 24 * time.Hour

// PasteFiles is the box's side of paste-files. It answers which of the
// laptop's queried paths are on the box, keeps each file the laptop then
// sends as pasted/<content hash>/<name>, and once the laptop's side ends
// writes the path of each. Pastes made more than pastedTTL ago go first;
// pasting the same content again keeps its dir. The prune and each file's
// store hold the pasted lock, so a prune never removes a file another store
// just kept; receiving the laptop's upload does not, so a stalled upload
// holds up no other.
func PasteFiles(l boxlayout.Layout, r io.Reader, w io.Writer, now time.Time) error {
	if err := preparePasted(l, now); err != nil {
		return err
	}
	frames := pastefile.NewReader(r)
	paths, err := frames.Query()
	if err != nil {
		return err
	}
	onBox := make([]bool, len(paths))
	for i, path := range paths {
		if onBox[i], err = onTheBox(path); err != nil {
			return err
		}
	}
	if err := pastefile.WriteAnswer(w, onBox); err != nil {
		return err
	}
	var stored []string
	for {
		name, err := frames.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		path, err := receivePasted(l, frames, string(name), now)
		if err != nil {
			return err
		}
		stored = append(stored, path)
	}
	for _, path := range stored {
		if err := pastefile.WriteFrame(w, []byte(path)); err != nil {
			return err
		}
	}
	return nil
}

// onTheBox reports whether path names anything on the box, as the agent would find it.
func onTheBox(path string) (bool, error) {
	if !filepath.IsAbs(path) {
		return false, fmt.Errorf("queried path %q is not absolute", path)
	}
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return false, nil
	}
	return false, fmt.Errorf("looking up %s: %w", path, err)
}

// receivePasted streams the next file of frames into the pasted dir and keeps it as name.
func receivePasted(l boxlayout.Layout, frames *pastefile.Reader, name string, now time.Time) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("pasted file name %q is not one file name", name)
	}
	tmp, err := os.CreateTemp(l.PastedDir(), ".paste-*")
	if err != nil {
		return "", fmt.Errorf("creating pasted file: %w", err)
	}
	sum := sha256.New()
	if err := frames.CopyNext(io.MultiWriter(tmp, sum)); err != nil {
		return "", errors.Join(fmt.Errorf("receiving %s: %w", name, err), tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return "", errors.Join(fmt.Errorf("writing pasted file: %w", err), os.Remove(tmp.Name()))
	}
	var path string
	if err := withFlock(l.PastedLock(), func() (err error) {
		path, err = keepPasted(l.PastedDir(), tmp.Name(), hex.EncodeToString(sum.Sum(nil)), name, now)
		return err
	}); err != nil {
		return "", errors.Join(err, removeIfThere(tmp.Name()))
	}
	return path, nil
}

// keepPasted moves tmp to dir/<sum>/name, or drops it when that is there
// already. Either way the content's dir counts as pasted now.
func keepPasted(dir, tmp, sum, name string, now time.Time) (string, error) {
	contentDir := filepath.Join(dir, sum)
	path := filepath.Join(contentDir, name)
	if err := os.Mkdir(contentDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("creating pasted dir: %w", err)
	}
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		if err := os.Remove(tmp); err != nil {
			return "", fmt.Errorf("dropping pasted copy: %w", err)
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Rename(tmp, path); err != nil {
			return "", fmt.Errorf("committing pasted file: %w", err)
		}
	default:
		return "", fmt.Errorf("looking up pasted file: %w", err)
	}
	if err := os.Chtimes(contentDir, now, now); err != nil {
		return "", fmt.Errorf("refreshing pasted dir: %w", err)
	}
	return path, nil
}

func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// preparePasted creates the pasted dir and prunes pastes older than pastedTTL from it.
func preparePasted(l boxlayout.Layout, now time.Time) error {
	dir := l.PastedDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating pasted dir: %w", err)
	}
	return withFlock(l.PastedLock(), func() error { return pruneOlderThan(dir, pastedTTL, now) })
}

// StorePasted is paste-store, which laptops built before paste-files use. It
// keeps each image file the laptop sends on r in the pasted dir, named by its
// content hash, and writes its path to w, one per line, holding the pasted
// lock as PasteFiles does.
func StorePasted(l boxlayout.Layout, r io.Reader, w io.Writer, now time.Time) error {
	if err := preparePasted(l, now); err != nil {
		return err
	}
	dir := l.PastedDir()
	frames := pastefile.NewReader(r)
	for {
		data, err := frames.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var path string
		if err := withFlock(l.PastedLock(), func() (err error) {
			path, err = storePasted(dir, data, now)
			return err
		}); err != nil {
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
