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
	"remote-agents/internal/spec"
)

// pastedTTL is how long a pasted file stays after its last paste.
const pastedTTL = 7 * 24 * time.Hour

// PasteFiles is the box's side of paste-files. It answers which of the
// laptop's queried paths are on the box, keeps each file the laptop then
// sends as pasted/<content hash>/<name>, and once the laptop's side ends
// writes the path of each. Then pastes made more than pastedTTL ago go; the
// laptop has all it needs by then. Pasting the same content again keeps its
// dir. The prune and each file's store hold the pasted lock, so a prune never
// removes a file another store just kept; receiving the laptop's upload does
// not, so a stalled upload holds up no other.
func PasteFiles(l boxlayout.Layout, r io.Reader, w io.Writer, now time.Time) error {
	return pasteFiles(l, r, w, now, func(name string) (string, error) { return name, checkFileName(name) }, nil)
}

// PasteFilesInto is PasteFiles for a laptop that names the session pasted
// into and each file by its laptop path. The session gets a copy of each file
// of its own, the path written back: an agent's edit there changes no other
// session's paste, and goes back to the laptop file.
func PasteFilesInto(l boxlayout.Layout, session string, r io.Reader, w io.Writer, now time.Time) error {
	if err := spec.ValidSessionName(session); err != nil {
		return err
	}
	name := func(origin string) (string, error) {
		if !filepath.IsAbs(origin) || filepath.Clean(origin) != origin {
			return "", fmt.Errorf("pasted file %q is not an absolute, clean laptop path", origin)
		}
		return filepath.Base(origin), checkFileName(filepath.Base(origin))
	}
	keep := func(origin, stored, sum string) (string, error) {
		return keepSessionCopy(l, session, origin, stored, sum)
	}
	return pasteFiles(l, r, w, now, name, keep)
}

// pasteFiles runs a paste-files exchange. name gives the stored name of a
// file from its name frame; keep, when set, makes the path written for each
// file from its frame, its stored path and its content hash, holding the
// pasted lock.
func pasteFiles(l boxlayout.Layout, r io.Reader, w io.Writer, now time.Time, name func(string) (string, error), keep func(frame, stored, sum string) (string, error)) error {
	if err := createPastedDir(l); err != nil {
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
		frame, err := frames.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		fileName, err := name(string(frame))
		if err != nil {
			return err
		}
		path, err := receivePasted(l, frames, fileName, w, now, func(stored, sum string) (string, error) {
			if keep == nil {
				return stored, nil
			}
			return keep(string(frame), stored, sum)
		})
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
	return prunePasted(l, now)
}

func checkFileName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return fmt.Errorf("pasted file name %q is not one file name", name)
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

// receivePasted streams the next file of frames into the pasted dir, reporting
// progress to w, keeps it as name and gives the path keep makes of it.
func receivePasted(l boxlayout.Layout, frames *pastefile.Reader, name string, w io.Writer, now time.Time, keep func(stored, sum string) (string, error)) (string, error) {
	tmp, err := os.CreateTemp(l.PastedDir(), ".paste-*")
	if err != nil {
		return "", fmt.Errorf("creating pasted file: %w", err)
	}
	sum := sha256.New()
	if err := frames.CopyNext(io.MultiWriter(tmp, sum, &progress{w: w})); err != nil {
		return "", errors.Join(fmt.Errorf("receiving %s: %w", name, err), tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return "", errors.Join(fmt.Errorf("writing pasted file: %w", err), os.Remove(tmp.Name()))
	}
	hash := hex.EncodeToString(sum.Sum(nil))
	var path string
	if err := withFlock(l.PastedLock(), func() error {
		stored, err := keepPasted(l.PastedDir(), tmp.Name(), hash, name, now)
		if err != nil {
			return err
		}
		path, err = keep(stored, hash)
		return err
	}); err != nil {
		return "", errors.Join(err, removeIfThere(tmp.Name()))
	}
	return path, nil
}

// keepPasted moves tmp to dir/<sum>/name, over a copy there already that the
// agent may have changed, and counts the content's dir as pasted now.
func keepPasted(dir, tmp, sum, name string, now time.Time) (string, error) {
	contentDir := filepath.Join(dir, sum)
	path := filepath.Join(contentDir, name)
	if err := os.Mkdir(contentDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("creating pasted dir: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("committing pasted file: %w", err)
	}
	if err := os.Chtimes(contentDir, now, now); err != nil {
		return "", fmt.Errorf("refreshing pasted dir: %w", err)
	}
	return path, nil
}

// progress writes an empty frame to w each time another pastefile.ProgressEvery bytes pass through it.
type progress struct {
	w        io.Writer
	received int64
}

func (p *progress) Write(b []byte) (int, error) {
	before := p.received / pastefile.ProgressEvery
	p.received += int64(len(b))
	if p.received/pastefile.ProgressEvery > before {
		if err := pastefile.WriteFrame(p.w, nil); err != nil {
			return 0, fmt.Errorf("reporting progress: %w", err)
		}
	}
	return len(b), nil
}

// removeIfThere removes path unless it is gone already.
func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// prunePasted removes the pastes made more than pastedTTL ago, in either
// layout, and the sessions' copies untouched as long whose content the laptop has.
func prunePasted(l boxlayout.Layout, now time.Time) error {
	return withFlock(l.PastedLock(), func() error {
		return errors.Join(pruneOlderThan(l.PastedDir(), pastedTTL, now, os.RemoveAll), pruneSessionCopies(l, now))
	})
}

// createPastedDir creates the pasted dir unless it is there.
func createPastedDir(l boxlayout.Layout) error {
	if err := os.MkdirAll(l.PastedDir(), 0o700); err != nil {
		return fmt.Errorf("creating pasted dir: %w", err)
	}
	return nil
}

// StorePasted is paste-store, which laptops built before paste-files use. It
// keeps each image file the laptop sends on r in the pasted dir, named by its
// content hash, and writes its path to w, one per line, holding the pasted
// lock as PasteFiles does.
func StorePasted(l boxlayout.Layout, r io.Reader, w io.Writer, now time.Time) error {
	if err := createPastedDir(l); err != nil {
		return err
	}
	if err := prunePasted(l, now); err != nil {
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
