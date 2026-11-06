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
// the session replaces its copy. A laptop file a laptop-save of the session
// made has a record at session-saves/<session>/<key>.json, of the box file it
// follows, wherever that is. A laptop file may have both: which one it
// follows is the laptop's to say, and a write-back of the other is refused
// as superseded.

// copyRecord is what the box keeps about a session's copy: its laptop path,
// the hash of the content the laptop last pasted or took, and that of a
// version the laptop refused, which the box does not offer again. A refused
// version reached no laptop, so it stays a change the laptop lacks. Path is
// the box file a saved laptop file follows, and empty for a paste.
type copyRecord struct {
	Origin  string `json:"origin"`
	Synced  string `json:"synced"`
	Refused string `json:"refused,omitempty"`
	Path    string `json:"path,omitempty"`
}

// pastedCopy is one session's copy of a pasted laptop file, or the box file
// a saved one follows when Linked.
type pastedCopy struct {
	Session string
	Origin  string
	Path    string
	Linked  bool
	Synced  string
	Refused string
	record  string
}

// rec is the record c was read from.
func (c pastedCopy) rec() copyRecord {
	rec := copyRecord{Origin: c.Origin, Synced: c.Synced, Refused: c.Refused}
	if c.Linked {
		rec.Path = c.Path
	}
	return rec
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
	if err := checkSaveSent(l, session, origin); err != nil {
		return "", err
	}
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
			// Pasting again asks the laptop afresh about a version it refused.
			if rec.Refused != "" {
				rec.Refused = ""
				if err := jsonfile.Write(dir+".json", rec); err != nil {
					return "", err
				}
			}
			return path, nil
		case pending:
			copySum, err := fileSum(path)
			if err != nil {
				return "", err
			}
			if copySum == rec.Refused {
				return "", fmt.Errorf("the laptop refused the agent's version of %s, which stays at %s on the box; have the agent laptop-save it, or remove it to paste the file again", filepath.Base(origin), path)
			}
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

// checkSaveSent fails while the box file session saved to origin holds a
// change the laptop lacks yet, which a paste of origin would cut off. A
// version the laptop refused will not go, so it holds up no paste.
func checkSaveSent(l boxlayout.Layout, session, origin string) error {
	var rec copyRecord
	found, err := jsonfile.Read(filepath.Join(l.SessionSavesDir(), session, originKey(origin)+".json"), &rec)
	if err != nil || !found {
		return err
	}
	current, err := fileSum(rec.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case current != rec.Synced && current != rec.Refused:
		return fmt.Errorf("the agent's change to %s, saved to %s, has not reached the laptop yet; paste it again once it has", rec.Path, filepath.Base(origin))
	}
	return nil
}

// linkSaved records that the laptop file origin follows the box file path
// since session saved it there, the laptop then holding content of hash sum,
// for the link holder to send it path's later changes.
func linkSaved(l boxlayout.Layout, session, origin, path, sum string) error {
	dir := filepath.Join(l.SessionSavesDir(), session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the session's saves dir: %w", err)
	}
	return withFlock(l.PastedLock(), func() error {
		return jsonfile.Write(filepath.Join(dir, originKey(origin)+".json"), copyRecord{Origin: origin, Synced: sum, Path: path})
	})
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

// sessionCopies lists every session's copies of pasted files, then the box
// files its saved laptop files follow. A record it cannot read is in the
// error, and the others are listed all the same.
func sessionCopies(l boxlayout.Layout) ([]pastedCopy, error) {
	pastes, pastesErr := listRecords(l.SessionPastesDir(), false)
	saves, savesErr := listRecords(l.SessionSavesDir(), true)
	return append(pastes, saves...), errors.Join(pastesErr, savesErr)
}

// listRecords lists the records in each session's dir in root, of saved
// laptop files when saved.
func listRecords(root string, saved bool) ([]pastedCopy, error) {
	sessions, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", filepath.Base(root), err)
	}
	var out []pastedCopy
	var errs []error
	for _, s := range sessions {
		if !s.IsDir() {
			continue
		}
		dir := filepath.Join(root, s.Name())
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
			path := rec.Path
			if !saved {
				key := filepath.Base(record[:len(record)-len(".json")])
				path = filepath.Join(dir, key, filepath.Base(rec.Origin))
			}
			out = append(out, pastedCopy{
				Session: s.Name(),
				Origin:  rec.Origin,
				Path:    path,
				Linked:  saved,
				Synced:  rec.Synced,
				Refused: rec.Refused,
				record:  record,
			})
		}
	}
	return out, errors.Join(errs...)
}

// markSynced records that the laptop has content of hash sum for c.
func markSynced(l boxlayout.Layout, c pastedCopy, sum string) error {
	return updateCopy(l, c, func(rec *copyRecord) { rec.Synced, rec.Refused = sum, "" })
}

// markRefused records that the laptop refused c's content of hash sum.
func markRefused(l boxlayout.Layout, c pastedCopy, sum string) error {
	return updateCopy(l, c, func(rec *copyRecord) { rec.Refused = sum })
}

// unlink drops the record of c, a box file that is gone, so that the laptop
// file following it no longer does. It reports false when the record changed
// since c was read, and stays.
func unlink(l boxlayout.Layout, c pastedCopy) (bool, error) {
	removed := false
	err := whileUnchanged(l, c, func(copyRecord) error {
		removed = true
		return removeIfThere(c.record)
	})
	return removed, err
}

// updateCopy changes c's record with fn.
func updateCopy(l boxlayout.Layout, c pastedCopy, fn func(*copyRecord)) error {
	return whileUnchanged(l, c, func(rec copyRecord) error {
		fn(&rec)
		return jsonfile.Write(c.record, rec)
	})
}

// whileUnchanged runs fn on c's record holding the pasted lock, unless the
// record changed since c was read, as a paste or save in between changes it.
func whileUnchanged(l boxlayout.Layout, c pastedCopy, fn func(copyRecord) error) error {
	return withFlock(l.PastedLock(), func() error {
		var rec copyRecord
		found, err := jsonfile.Read(c.record, &rec)
		if err != nil || !found || rec != c.rec() {
			return err
		}
		return fn(rec)
	})
}

// pruneSessionCopies removes the copies whose file and record are older than
// pastedTTL and that hold what the laptop has, then the session dirs left
// empty. A box file a saved laptop file follows stays; only its record goes.
// The caller holds the pasted lock.
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
		var removeCopy error
		if !c.Linked {
			removeCopy = os.RemoveAll(filepath.Dir(c.Path))
		}
		if err := errors.Join(removeCopy, removeIfThere(c.record)); err != nil {
			return fmt.Errorf("pruning a session's paste: %w", err)
		}
	}
	return errors.Join(listErr, removeEmptyDirs(l.SessionPastesDir()), removeEmptyDirs(l.SessionSavesDir()))
}

// removeEmptyDirs removes the sessions' dirs in root left empty.
func removeEmptyDirs(root string) error {
	sessions, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing %s: %w", filepath.Base(root), err)
	}
	for _, s := range sessions {
		dir := filepath.Join(root, s.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("listing %s: %w", filepath.Base(root), err)
		}
		if len(entries) == 0 {
			if err := removeIfThere(dir); err != nil {
				return fmt.Errorf("pruning a session's dir: %w", err)
			}
		}
	}
	return nil
}

func staleCopy(c pastedCopy, now time.Time) (bool, error) {
	record, err := os.Stat(c.record)
	if err != nil {
		return false, err
	}
	if now.Sub(record.ModTime()) <= pastedTTL {
		return false, nil
	}
	file, err := os.Stat(c.Path)
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
