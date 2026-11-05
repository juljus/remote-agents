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
	"remote-agents/internal/jsonfile"
	"remote-agents/internal/pastefile"
)

// A session's copy of a pasted laptop file lives at
// session-pastes/<session>/<key>/<name>, its record beside its dir as
// <key>.json; key comes from the laptop path, so pasting a file again into
// the session replaces its copy.

// copyRecord is what the box keeps about a session's copy: its laptop path,
// and the hash of the content the laptop last pasted, took or refused.
type copyRecord struct {
	Origin string `json:"origin"`
	Synced string `json:"synced"`
}

// pastedCopy is one session's copy of a pasted laptop file.
type pastedCopy struct {
	Session string
	Origin  string
	Path    string
	Synced  string
	record  string
}

func originKey(origin string) string {
	sum := sha256.Sum256([]byte(origin))
	return hex.EncodeToString(sum[:8])
}

// keepSessionCopy copies the stored paste of origin, of content hash sum,
// over session's copy of it. A copy holding a change the laptop lacks yet
// stays: when the laptop's file is as it was, the change is the newer one and
// still goes back; when both changed, the paste fails until the change has
// reached the laptop, where the conflict rule keeps both. The caller holds
// the pasted lock.
func keepSessionCopy(l boxlayout.Layout, session, origin, stored, sum string) (string, error) {
	dir := filepath.Join(l.SessionPastesDir(), session, originKey(origin))
	path := filepath.Join(dir, filepath.Base(origin))
	var rec copyRecord
	found, err := jsonfile.Read(dir+".json", &rec)
	if err != nil {
		return "", err
	}
	if found {
		pending, err := changedSince(path, rec.Synced)
		if err != nil {
			return "", err
		}
		switch {
		case pending && sum == rec.Synced:
			return path, nil
		case pending:
			return "", fmt.Errorf("the agent's change to %s has not reached the laptop yet; paste it again once it has", filepath.Base(origin))
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating the session's paste dir: %w", err)
	}
	if err := copyFile(stored, path); err != nil {
		return "", err
	}
	if err := jsonfile.Write(dir+".json", copyRecord{Origin: origin, Synced: sum}); err != nil {
		return "", err
	}
	return path, nil
}

// changedSince reports whether path holds content other than of hash synced;
// a copy that is gone holds nothing to lose.
func changedSince(path, synced string) (bool, error) {
	sum, err := fileSum(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sum != synced, nil
}

// copyFile replaces dst with a copy of src in one rename.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".paste-*")
	if err != nil {
		return fmt.Errorf("creating the session's copy: %w", err)
	}
	if _, err := io.Copy(tmp, in); err != nil {
		return errors.Join(fmt.Errorf("copying %s: %w", filepath.Base(dst), err), tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(fmt.Errorf("writing %s: %w", filepath.Base(dst), err), os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return errors.Join(fmt.Errorf("committing %s: %w", filepath.Base(dst), err), os.Remove(tmp.Name()))
	}
	return nil
}

// sessionCopies lists every session's copies of pasted files. A record it
// cannot read is in the error, and the others are listed all the same.
func sessionCopies(l boxlayout.Layout) ([]pastedCopy, error) {
	sessions, err := os.ReadDir(l.SessionPastesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing session pastes: %w", err)
	}
	var out []pastedCopy
	var errs []error
	for _, s := range sessions {
		if !s.IsDir() {
			continue
		}
		dir := filepath.Join(l.SessionPastesDir(), s.Name())
		records, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			var rec copyRecord
			found, err := jsonfile.Read(record, &rec)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if !found {
				continue
			}
			key := filepath.Base(record[:len(record)-len(".json")])
			out = append(out, pastedCopy{
				Session: s.Name(),
				Origin:  rec.Origin,
				Path:    filepath.Join(dir, key, filepath.Base(rec.Origin)),
				Synced:  rec.Synced,
				record:  record,
			})
		}
	}
	return out, errors.Join(errs...)
}

// markSynced records that the laptop has content of hash sum for c, unless
// c's record changed since c was read, as a paste in between changes it.
func markSynced(l boxlayout.Layout, c pastedCopy, sum string) error {
	return withFlock(l.PastedLock(), func() error {
		var rec copyRecord
		found, err := jsonfile.Read(c.record, &rec)
		if err != nil || !found || rec != (copyRecord{Origin: c.Origin, Synced: c.Synced}) {
			return err
		}
		rec.Synced = sum
		return jsonfile.Write(c.record, rec)
	})
}

// pruneSessionCopies removes the copies whose file and record are older than
// pastedTTL and that hold what the laptop has, then the session dirs left
// empty. The caller holds the pasted lock.
func pruneSessionCopies(l boxlayout.Layout, now time.Time) error {
	copies, listErr := sessionCopies(l)
	for _, c := range copies {
		stale, err := staleCopy(c, now)
		if err != nil {
			return err
		}
		if !stale {
			continue
		}
		if err := errors.Join(os.RemoveAll(filepath.Dir(c.Path)), removeIfThere(c.record)); err != nil {
			return fmt.Errorf("pruning a session's paste: %w", err)
		}
	}
	sessions, err := os.ReadDir(l.SessionPastesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing session pastes: %w", err)
	}
	for _, s := range sessions {
		dir := filepath.Join(l.SessionPastesDir(), s.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("listing session pastes: %w", err)
		}
		if len(entries) == 0 {
			if err := removeIfThere(dir); err != nil {
				return fmt.Errorf("pruning a session's paste dir: %w", err)
			}
		}
	}
	return listErr
}

func staleCopy(c pastedCopy, now time.Time) (bool, error) {
	record, err := os.Stat(c.record)
	if err != nil {
		return false, err
	}
	if now.Sub(record.ModTime()) <= pastedTTL {
		return false, nil
	}
	file, err := os.Lstat(c.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if now.Sub(file.ModTime()) <= pastedTTL {
		return false, nil
	}
	sum, err := fileSum(c.Path)
	if err != nil {
		return false, err
	}
	return sum == c.Synced, nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SendFile is the box's side of send-file: the laptop sends a frame with the
// path of a box file, and the box answers with a frame of its content.
func SendFile(r io.Reader, w io.Writer) error {
	frame, err := pastefile.NewReader(r).Next()
	if err != nil {
		return fmt.Errorf("reading the path: %w", err)
	}
	path := string(frame)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%q is not absolute", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	switch {
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	case info.Size() > pastefile.MaxSize:
		return fmt.Errorf("%s is over %d MB", path, pastefile.MaxSize>>20)
	}
	return pastefile.CopyFrame(w, f, info.Size())
}
