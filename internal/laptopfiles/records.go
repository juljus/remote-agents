package laptopfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"remote-agents/internal/jsonfile"
	"remote-agents/internal/spec"
)

// recordTTL is how long a session's record stays after its last paste or
// write; the box drops its copies after a week without either.
const recordTTL = 14 * 24 * time.Hour

// Records keeps, per session, the laptop files pasted into it or saved from
// it: the content the laptop had at the paste, save or last write-back (the
// base), that of the last copy written beside the file when the laptop's had
// changed, and the one box file it follows: the session's copy of a pasted
// file, or the box file saved to it. Whichever came last of a paste and a
// save decides, and the box hears which when it offers the other.
type Records struct{ Dir string }

type entry struct {
	Base    string `json:"base"`
	Copy    string `json:"copy,omitempty"`
	BoxCopy string `json:"box_copy,omitempty"`
	Linked  string `json:"linked,omitempty"`
}

// from refuses, as superseded, a write-back of boxPath to origin, whose
// entry is e, when origin follows another box file. A paste recorded before
// pastes named their box copy names none, and takes any until pasted again.
func (e entry) from(origin, boxPath string) error {
	follows := e.Linked
	if follows == "" {
		follows = e.BoxCopy
	}
	if follows != "" && follows != boxPath {
		return fmt.Errorf("%w: %s follows %s on the box, not %s", ErrSuperseded, origin, follows, boxPath)
	}
	return nil
}

type sessionRecord struct {
	Files map[string]entry `json:"files"`
}

// linkedFrom is the set of laptop files that follow boxPath.
func (rec sessionRecord) linkedFrom(boxPath string) map[string]bool {
	out := map[string]bool{}
	for origin, e := range rec.Files {
		if e.Linked == boxPath {
			out[origin] = true
		}
	}
	return out
}

// Pasted records that origin, holding content of hash sum, was pasted into
// session, the box keeping its copy at boxCopy; a write-back of that copy
// replaces it while it still holds that content. origin must be absolute
// with its symlinks resolved, as Origin makes it.
func (r Records) Pasted(session, origin, sum, boxCopy string) error {
	for _, path := range []string{origin, boxCopy} {
		if err := checkPath(path); err != nil {
			return err
		}
	}
	return r.update(session, func(rec *sessionRecord) error {
		rec.Files[origin] = entry{Base: sum, BoxCopy: boxCopy}
		return nil
	})
}

// Origin is how a pasted file's path is recorded: absolute, with its
// symlinks resolved, spelled as the file system holds it.
func Origin(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return diskPath(resolved)
}

func checkPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%w: %q is not an absolute, clean path", ErrRefused, path)
	}
	return nil
}

func checkSession(session string) error {
	if err := spec.ValidSessionName(session); err != nil {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}
	return nil
}

func (r Records) path(session string) string { return filepath.Join(r.Dir, session+".json") }

// read returns session's record.
func (r Records) read(session string) (sessionRecord, error) {
	if err := checkSession(session); err != nil {
		return sessionRecord{}, err
	}
	var rec sessionRecord
	_, err := jsonfile.Read(r.path(session), &rec)
	return rec, err
}

// lookup returns what is recorded about origin in session.
func (r Records) lookup(session, origin string) (entry, bool, error) {
	rec, err := r.read(session)
	if err != nil {
		return entry{}, false, err
	}
	e, ok := rec.Files[origin]
	return e, ok, nil
}

// update changes session's record with fn, holding the records' lock.
func (r Records) update(session string, fn func(*sessionRecord) error) error {
	if err := checkSession(session); err != nil {
		return err
	}
	return r.locked(func() error {
		var rec sessionRecord
		if _, err := jsonfile.Read(r.path(session), &rec); err != nil {
			return err
		}
		if rec.Files == nil {
			rec.Files = map[string]entry{}
		}
		if err := fn(&rec); err != nil {
			return err
		}
		if err := jsonfile.Write(r.path(session), rec); err != nil {
			return err
		}
		return r.prune(time.Now())
	})
}

// locked runs fn holding an exclusive flock on the records' lock file, which
// orders the wrappers' pastes and the link daemon's writes.
func (r Records) locked(fn func() error) error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", r.Dir, err)
	}
	f, err := os.OpenFile(filepath.Join(r.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening the pasted records' lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking the pasted records: %w", err)
	}
	return fn()
}

// prune removes the records of sessions untouched for recordTTL.
func (r Records) prune(now time.Time) error {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return fmt.Errorf("listing pasted records: %w", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat pasted record %s: %w", e.Name(), err)
		}
		if now.Sub(info.ModTime()) > recordTTL {
			if err := os.Remove(filepath.Join(r.Dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("pruning pasted record: %w", err)
			}
		}
	}
	return nil
}
