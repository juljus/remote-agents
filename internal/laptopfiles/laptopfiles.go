// Package laptopfiles is where the box may write on the laptop, and how. Box
// agents run without permission prompts, so the box gets no other way in: a
// file pasted into a session is written back over its laptop origin, or
// beside it as "name (box).ext" when the laptop's copy changed since; other
// files the box saves land in ~/Downloads, or over a file pasted into the
// session. Nothing hidden is written: a dotfile is code. A file replaced goes
// to the Trash.
package laptopfiles

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
)

// ErrRefused means the box asked to write where it may not.
var ErrRefused = errors.New("refused")

// Fetch writes the content the box sends to w.
type Fetch func(w io.Writer) error

type Outcome string

const (
	// Written: the origin held its base, and now holds the box's content.
	Written Outcome = "written"
	// Unchanged: the origin held the box's content already.
	Unchanged Outcome = "unchanged"
	// Conflict: the origin changed on the laptop, so the box's content went beside it.
	Conflict Outcome = "conflict"
)

// Result is where a write-back's content went, and its hash. Leftover, when
// set, is what the write-back left behind once the content was in place, such
// as an old version the Trash refused, and LeftoverErr says why.
type Result struct {
	Outcome     Outcome
	Path        string
	Sum         string
	Leftover    string
	LeftoverErr error
}

// Files writes what the box sends, within the bounds of the package doc.
type Files struct {
	Records Records
	Home    string
	// Trash moves a file to the Trash.
	Trash func(path string) error
}

// WriteBack writes the content fetch gives for a file pasted into session
// from origin: over origin while it holds its base, else beside it.
func (f Files) WriteBack(session, origin string, fetch Fetch) (Result, error) {
	if err := checkPath(origin); err != nil {
		return Result{}, err
	}
	if _, err := f.pastedEntry(session, origin); err != nil {
		return Result{}, err
	}
	tmp, sum, err := fetchInto(filepath.Dir(origin), fetch)
	if err != nil {
		return Result{}, err
	}
	defer removeIfThere(tmp)
	var res Result
	err = f.Records.update(session, func(rec *sessionRecord) error {
		e, ok := rec.Files[origin]
		if !ok {
			return fmt.Errorf("%w: %s was not pasted into this session", ErrRefused, origin)
		}
		current, mode, err := regularSum(origin)
		if err != nil {
			return err
		}
		switch current {
		case sum:
			e.Base = sum
			res = Result{Outcome: Unchanged, Path: origin, Sum: sum}
		case e.Base:
			left, err := f.replace(origin, tmp, mode)
			if err != nil {
				return err
			}
			e.Base = sum
			res = Result{Outcome: Written, Path: origin, Sum: sum, Leftover: left.path, LeftoverErr: left.err}
		default:
			path, err := f.writeCopy(origin, tmp, e.Copy, mode)
			if err != nil {
				return err
			}
			e.Copy = sum
			res = Result{Outcome: Conflict, Path: path, Sum: sum}
		}
		rec.Files[origin] = e
		return nil
	})
	return res, err
}

// pastedEntry is the record of origin in session, refusing a file not pasted
// into it and one whose dir no longer resolves where it did.
func (f Files) pastedEntry(session, origin string) (entry, error) {
	e, ok, err := f.Records.lookup(session, origin)
	if err != nil {
		return entry{}, err
	}
	if !ok {
		return entry{}, fmt.Errorf("%w: %s was not pasted into this session", ErrRefused, origin)
	}
	if hidden(origin) {
		return entry{}, fmt.Errorf("%w: %s is hidden, and nothing hidden is written from the box; the box keeps its version", ErrRefused, origin)
	}
	dir := filepath.Dir(origin)
	// A dir that cannot be resolved now, such as one on a volume not mounted, may be back later.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return entry{}, err
	}
	if resolved != dir {
		return entry{}, fmt.Errorf("%w: %s now leads to %s", ErrRefused, dir, resolved)
	}
	return e, nil
}

// hidden reports whether a component of path starts with a dot, as dotfiles
// and the dirs of shell and tool configs do: the box can put such a path on
// the clipboard for the user to paste, and writing it could run code.
func hidden(path string) bool {
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// regularSum hashes path, giving "" when it is gone or not a regular file,
// and the mode a file written in its place gets.
func regularSum(path string) (string, fs.FileMode, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0o644, nil
	}
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0o644, nil
	}
	src, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	h := sha256.New()
	if _, err := io.Copy(h, src); err != nil {
		return "", 0, fmt.Errorf("reading %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), info.Mode().Perm(), nil
}

// fetchInto fetches into a hidden temporary file in dir, so that it can take
// the place of a file there in one rename.
func fetchInto(dir string, fetch Fetch) (string, string, error) {
	tmp, err := os.CreateTemp(dir, ".remote-agents-*")
	if err != nil {
		return "", "", fmt.Errorf("creating a file in %s: %w", dir, err)
	}
	h := sha256.New()
	if err := fetch(io.MultiWriter(tmp, h)); err != nil {
		return "", "", errors.Join(err, tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return "", "", errors.Join(fmt.Errorf("writing %s: %w", tmp.Name(), err), os.Remove(tmp.Name()))
	}
	return tmp.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

// leftover is what a replace left behind after its rename, and why.
type leftover struct {
	path string
	err  error
}

// replace puts tmp in place of path in one rename and moves what path held
// to the Trash. A hard link keeps the old version through the rename. Past
// the rename the content is in place, so what fails then is a leftover, not
// a failed replace.
func (f Files) replace(path, tmp string, mode fs.FileMode) (leftover, error) {
	if err := os.Chmod(tmp, mode); err != nil {
		return leftover{}, err
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return leftover{}, linkNew(tmp, path)
	}
	held, err := os.MkdirTemp(filepath.Dir(path), ".remote-agents-old-*")
	if err != nil {
		return leftover{}, fmt.Errorf("keeping the old %s: %w", filepath.Base(path), err)
	}
	old := filepath.Join(held, filepath.Base(path))
	if err := os.Link(path, old); err != nil {
		return leftover{}, errors.Join(fmt.Errorf("keeping the old %s: %w", filepath.Base(path), err), os.Remove(held))
	}
	if err := os.Rename(tmp, path); err != nil {
		return leftover{}, errors.Join(fmt.Errorf("replacing %s: %w", path, err), os.Remove(old), os.Remove(held))
	}
	if err := f.Trash(old); err != nil {
		return leftover{old, fmt.Errorf("moving the old version to the Trash: %w", err)}, nil
	}
	if err := os.Remove(held); err != nil {
		return leftover{held, err}, nil
	}
	return leftover{}, nil
}

// writeCopy puts tmp beside origin as its conflict copy. A copy there that
// holds what the last one written held is replaced; anything else there goes
// to the Trash first.
func (f Files) writeCopy(origin, tmp, lastCopy string, mode fs.FileMode) (string, error) {
	path := filepath.Join(filepath.Dir(origin), conflictName(filepath.Base(origin)))
	if err := os.Chmod(tmp, mode); err != nil {
		return "", err
	}
	current, _, err := regularSum(path)
	if err != nil {
		return "", err
	}
	if current != "" && current == lastCopy {
		return path, os.Rename(tmp, path)
	}
	if _, err := os.Lstat(path); err == nil {
		if err := f.Trash(path); err != nil {
			return "", fmt.Errorf("moving %s to the Trash: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return path, linkNew(tmp, path)
}

// conflictName is "name (box).ext" for name.
func conflictName(name string) string {
	stem, ext := splitExt(name)
	return stem + " (box)" + ext
}

func splitExt(name string) (string, string) {
	ext := filepath.Ext(name)
	if ext == name {
		return name, ""
	}
	return strings.TrimSuffix(name, ext), ext
}

// linkNew gives tmp the name path, failing if path exists; tmp goes either way.
func linkNew(tmp, path string) error {
	if err := os.Link(tmp, path); err != nil {
		return err
	}
	return os.Remove(tmp)
}

func removeIfThere(path string) {
	// The temporary file is gone after a commit; any other error leaves a hidden file and nothing worse.
	_ = os.Remove(path)
}

// maxNumbered bounds the "name (n).ext" a save tries before giving up.
const maxNumbered = 1000

// Save writes the content fetch gives, named name unless dest names the
// file, to dest: a file pasted into session (replacing it, its old version
// to the Trash), or a path in ~/Downloads under a name no file there has.
// dest may start with ~/, is relative to ~/Downloads, and when it names a
// dir the file goes into it; empty, it is ~/Downloads. The result says where
// the file went.
func (f Files) Save(session, name, dest string, fetch Fetch) (Result, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return Result{}, fmt.Errorf("%w: %q is not one file name", ErrRefused, name)
	}
	downloads, err := filepath.EvalSymlinks(filepath.Join(f.Home, "Downloads"))
	if err != nil {
		return Result{}, fmt.Errorf("finding ~/Downloads: %w", err)
	}
	path := downloads
	switch {
	case dest == "~" || strings.HasPrefix(dest, "~/"):
		path = filepath.Join(f.Home, dest[1:])
	case filepath.IsAbs(dest):
		path = filepath.Clean(dest)
	case dest != "":
		path = filepath.Join(downloads, dest)
	}
	asDir := strings.HasSuffix(dest, "/")
	if session != "" && dest != "" && !asDir {
		if _, err := f.pastedEntry(session, path); err == nil {
			return f.saveOverPasted(session, path, fetch)
		} else if !errors.Is(err, ErrRefused) {
			return Result{}, err
		}
	}
	dir, file := path, name
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return Result{}, err
	case asDir:
		return Result{}, fmt.Errorf("%w: %s is not a dir", ErrRefused, dest)
	default:
		dir, file = filepath.Dir(path), filepath.Base(path)
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	if dir != downloads && !strings.HasPrefix(dir, downloads+string(filepath.Separator)) {
		return Result{}, fmt.Errorf("%w: %s is neither in ~/Downloads nor a file pasted into this session", ErrRefused, dest)
	}
	tmp, _, err := fetchInto(dir, fetch)
	if err != nil {
		return Result{}, err
	}
	defer removeIfThere(tmp)
	if err := os.Chmod(tmp, 0o644); err != nil {
		return Result{}, err
	}
	stem, ext := splitExt(file)
	for n := 0; n < maxNumbered; n++ {
		candidate := filepath.Join(dir, file)
		if n > 0 {
			candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
		}
		err := linkNew(tmp, candidate)
		if err == nil {
			return Result{Outcome: Written, Path: candidate}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return Result{}, err
		}
	}
	return Result{}, fmt.Errorf("%s and %d numbered names are all taken", filepath.Join(dir, file), maxNumbered)
}

// saveOverPasted replaces a file pasted into session, whatever it holds now,
// and makes the content saved its base.
func (f Files) saveOverPasted(session, origin string, fetch Fetch) (Result, error) {
	tmp, sum, err := fetchInto(filepath.Dir(origin), fetch)
	if err != nil {
		return Result{}, err
	}
	defer removeIfThere(tmp)
	var left leftover
	err = f.Records.update(session, func(rec *sessionRecord) error {
		e, ok := rec.Files[origin]
		if !ok {
			return fmt.Errorf("%w: %s was not pasted into this session", ErrRefused, origin)
		}
		info, err := os.Lstat(origin)
		mode := fs.FileMode(0o644)
		switch {
		case err == nil && !info.Mode().IsRegular():
			return fmt.Errorf("%w: %s is not a regular file", ErrRefused, origin)
		case err == nil:
			mode = info.Mode().Perm()
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		if left, err = f.replace(origin, tmp, mode); err != nil {
			return err
		}
		e.Base = sum
		rec.Files[origin] = e
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Outcome: Written, Path: origin, Sum: sum, Leftover: left.path, LeftoverErr: left.err}, nil
}
