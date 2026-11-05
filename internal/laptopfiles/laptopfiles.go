// Package laptopfiles is where the box may write on the laptop, and how. Box
// agents run without permission prompts, so the box gets no other way in: a
// file pasted into a session is written back over its laptop origin, or
// beside it as "name (box).ext" when the laptop's copy changed since, and a
// file the box saves goes where the user approves. A write-back lands on its
// own only on a plain file of the user's: one outside home, under ~/Library,
// hidden, executable or reached through a symlink waits for the user's
// approval, as every save does. A file replaced goes to the Trash.
package laptopfiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
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
	// Gate asks the user before the writes that need their approval.
	Gate *Gate
}

// place is where a write lands, and why it needs the user's approval if it does.
type place struct {
	path  string
	risks []string
}

// writeBackGrant is what lets session write back to p: its write-backs of
// the file, on the risks the prompt named.
func (p place) writeBackGrant(session string) grant {
	return grant{session: session, use: writeBackUse, path: p.path, terms: strings.Join(p.risks, ", ")}
}

// WriteBack writes the content fetch gives for a file pasted into session
// from origin: over origin while it holds its base, else beside it. A risky
// place waits for the user's approval, once per session.
func (f Files) WriteBack(ctx context.Context, session, origin string, fetch Fetch) (Result, error) {
	if err := checkPath(origin); err != nil {
		return Result{}, err
	}
	e, ok, err := f.Records.lookup(session, origin)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, notPasted(origin)
	}
	asked, err := f.writeBackPlace(origin)
	if err != nil {
		return Result{}, err
	}
	if len(asked.risks) > 0 {
		reason, err := writeBackReason(session, asked, e.Base)
		if err != nil {
			return Result{}, err
		}
		if err := f.Gate.ask(ctx, asked.writeBackGrant(session), reason); err != nil {
			return Result{}, err
		}
	}
	tmp, sum, err := fetchInto(filepath.Dir(asked.path), fetch)
	if err != nil {
		return Result{}, err
	}
	defer removeIfThere(tmp)
	var res Result
	err = f.Records.update(session, func(rec *sessionRecord) error {
		e, ok := rec.Files[origin]
		if !ok {
			return notPasted(origin)
		}
		// What the user approved, or saw no need to, must be where the write lands.
		now, err := f.writeBackPlace(origin)
		if err != nil {
			return err
		}
		if now.path != asked.path || len(now.risks) > 0 && !f.Gate.approved(now.writeBackGrant(session)) {
			return fmt.Errorf("%w: %s changed while the box wrote it back; paste it again to offer the box's version anew", ErrRefused, origin)
		}
		target := now.path
		mode, err := modeFor(target)
		if err != nil {
			return err
		}
		current, err := regularSum(target)
		if err != nil {
			return err
		}
		switch current {
		case sum:
			e.Base = sum
			res = Result{Outcome: Unchanged, Path: target, Sum: sum}
		case e.Base:
			left, err := f.replace(target, tmp, mode)
			if err != nil {
				return err
			}
			e.Base = sum
			res = Result{Outcome: Written, Path: target, Sum: sum, Leftover: left.path, LeftoverErr: left.err}
		default:
			path, err := f.writeCopy(target, tmp, e.Copy, mode)
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

func notPasted(origin string) error {
	return fmt.Errorf("%w: %s was not pasted into this session", ErrRefused, origin)
}

// writeBackPlace is where a write-back of origin lands: origin, with its dir
// resolved. A dir that cannot be resolved now, such as one on a volume not
// mounted, may be back later, so that fails without refusing.
func (f Files) writeBackPlace(origin string) (place, error) {
	dir := filepath.Dir(origin)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return place{}, err
	}
	p, err := f.placeOf(filepath.Join(resolved, filepath.Base(origin)))
	if err != nil {
		return place{}, err
	}
	if resolved != dir {
		p.risks = append(p.risks, fmt.Sprintf("%q now leads to %q", dir, resolved))
	}
	return p, nil
}

// placeOf says why writing path, whose dir has its symlinks resolved, needs
// the user's approval. The box can put any path on the clipboard for the
// user to paste, and a file outside home, under ~/Library, hidden or
// executable, such as a dotfile or a launch agent, may run as code once
// written; a symlink stands for a file elsewhere.
func (f Files) placeOf(path string) (place, error) {
	home, err := filepath.EvalSymlinks(f.Home)
	if err != nil {
		return place{}, fmt.Errorf("finding your home: %w", err)
	}
	p := place{path: path}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		p.risks = append(p.risks, "outside your home")
	} else {
		parts := strings.Split(rel, string(filepath.Separator))
		// The spelling is the file system's when Origin recorded it; a case-blind check costs nothing more.
		if strings.EqualFold(parts[0], "Library") {
			p.risks = append(p.risks, "under ~/Library")
		}
		if slices.ContainsFunc(parts, func(part string) bool { return strings.HasPrefix(part, ".") }) {
			p.risks = append(p.risks, "hidden")
		}
	}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return place{}, err
	case info.Mode()&fs.ModeSymlink != 0:
		p.risks = append(p.risks, "a symlink")
	case !info.Mode().IsRegular():
		return place{}, fmt.Errorf("%w: %s is not a regular file", ErrRefused, path)
	case info.Mode().Perm()&0o111 != 0:
		p.risks = append(p.risks, "executable")
	}
	return p, nil
}

// modeFor is the mode a file written in place of path gets: that of the
// regular file there, else 0o644.
func modeFor(path string) (fs.FileMode, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0o644, nil
	case err != nil:
		return 0, err
	case info.Mode().IsRegular():
		return info.Mode().Perm(), nil
	}
	return 0o644, nil
}

// writeBackReason is the prompt for the write-backs of session to p, whose
// content at the paste had hash base, and says what the first does. The box
// picks the paths, so they are quoted: a name holding a newline or a bracket
// cannot pass for more of the text.
func writeBackReason(session string, p place, base string) (string, error) {
	current, err := regularSum(p.path)
	if err != nil {
		return "", err
	}
	head := fmt.Sprintf("write back the edits of box session %s to %s (%s)", session, strconv.Quote(p.path), strings.Join(p.risks, ", "))
	if current == base {
		return head + ": replace it now, the old version to the Trash", nil
	}
	copyPath := conflictPath(p.path)
	action := "create"
	if _, err := os.Lstat(copyPath); err == nil {
		action = "replace"
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return fmt.Sprintf("%s: %s %s beside it now, as it changed on the laptop", head, action, strconv.Quote(copyPath)), nil
}

// saveReason is the prompt for a save of session to p, which replaces a file
// there when existing.
func saveReason(session string, p place, existing bool) string {
	from := "the box"
	if session != "" {
		from = "box session " + session
	}
	action := "create"
	notes := slices.Clone(p.risks)
	if existing {
		action = "replace"
		notes = append(notes, "the old version goes to the Trash")
	}
	text := fmt.Sprintf("%s %s with a file from %s", action, strconv.Quote(p.path), from)
	if len(notes) > 0 {
		text += " (" + strings.Join(notes, ", ") + ")"
	}
	return text
}

// regularSum hashes path, giving "" when it is gone or not a regular file.
func regularSum(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	h := sha256.New()
	if _, err := io.Copy(h, src); err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
// to the Trash. A hard link keeps the old version through the rename, of a
// symlink itself when path is one, never of its target. Past the rename the
// content is in place, so what fails then is a leftover, not a failed
// replace.
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
	if err := unix.Linkat(unix.AT_FDCWD, path, unix.AT_FDCWD, old, 0); err != nil {
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
	path := conflictPath(origin)
	if err := os.Chmod(tmp, mode); err != nil {
		return "", err
	}
	current, err := regularSum(path)
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

// conflictPath is where the conflict copy of origin goes.
func conflictPath(origin string) string {
	return filepath.Join(filepath.Dir(origin), conflictName(filepath.Base(origin)))
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

// Save writes the content fetch gives to dest once the user approves, at
// most once per destination and session. dest names the file, or a dir that
// takes it under name, or a free "name (n).ext" when a file there has name.
// dest may start with ~/ or be absolute; relative, it is in ~/Downloads,
// which it is when empty. A file dest names is replaced, its old version to
// the Trash; one pasted into session takes the content saved as its base.
func (f Files) Save(ctx context.Context, session, name, dest string, fetch Fetch) (Result, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return Result{}, fmt.Errorf("%w: %q is not one file name", ErrRefused, name)
	}
	if session != "" {
		if err := checkSession(session); err != nil {
			return Result{}, err
		}
	}
	p, existing, err := f.savePlace(name, dest)
	if err != nil {
		return Result{}, err
	}
	gr := saveGrant(session, p, existing)
	if err := f.Gate.ask(ctx, gr, saveReason(session, p, existing)); err != nil {
		return Result{}, err
	}
	tmp, sum, err := fetchInto(filepath.Dir(p.path), fetch)
	if err != nil {
		return Result{}, err
	}
	defer removeIfThere(tmp)
	pasted := false
	if session != "" {
		if _, pasted, err = f.Records.lookup(session, p.path); err != nil {
			return Result{}, err
		}
	}
	var left leftover
	if !pasted {
		left, err = f.put(gr, tmp)
	} else {
		err = f.Records.update(session, func(rec *sessionRecord) error {
			var err error
			if left, err = f.put(gr, tmp); err != nil {
				return err
			}
			if e, ok := rec.Files[p.path]; ok {
				e.Base = sum
				rec.Files[p.path] = e
			}
			return nil
		})
	}
	if err != nil {
		return Result{}, err
	}
	return Result{Outcome: Written, Path: p.path, Sum: sum, Leftover: left.path, LeftoverErr: left.err}, nil
}

// savePlace is where a save of name to dest lands, and whether a file is there.
func (f Files) savePlace(name, dest string) (place, bool, error) {
	downloads, err := filepath.EvalSymlinks(filepath.Join(f.Home, "Downloads"))
	if err != nil {
		return place{}, false, fmt.Errorf("finding ~/Downloads: %w", err)
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
	dir, file, intoDir := path, name, true
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return place{}, false, err
	case strings.HasSuffix(dest, "/"):
		return place{}, false, fmt.Errorf("%w: %s is not a dir", ErrRefused, dest)
	default:
		dir, file, intoDir = filepath.Dir(path), filepath.Base(path), false
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return place{}, false, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	if intoDir {
		if file, err = freeName(dir, file); err != nil {
			return place{}, false, err
		}
	}
	p, err := f.placeOf(filepath.Join(dir, file))
	if err != nil {
		return place{}, false, err
	}
	_, err = os.Lstat(p.path)
	switch {
	case err == nil:
		return p, true, nil
	case errors.Is(err, fs.ErrNotExist):
		return p, false, nil
	}
	return place{}, false, err
}

// maxNumbered bounds the "name (n).ext" a save tries before giving up.
const maxNumbered = 1000

// freeName is the first of name, "name (1).ext", ... that nothing in dir has.
func freeName(dir, name string) (string, error) {
	stem, ext := splitExt(name)
	for n := range maxNumbered {
		candidate := name
		if n > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		_, err := os.Lstat(filepath.Join(dir, candidate))
		if errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("%s and %d numbered names are all taken", filepath.Join(dir, name), maxNumbered)
}

// saveGrant is what lets session save to p. A create approved does not
// cover a replace: the user may have put work in the file since.
func saveGrant(session string, p place, existing bool) grant {
	return grant{session: session, use: saveUse, path: p.path, terms: fmt.Sprintf("existing=%t; %s", existing, strings.Join(p.risks, ", "))}
}

// put writes tmp where gr let a save go, as a new file or over the one
// there, as the user was told. A place that changed since leads elsewhere or
// to something else, so it is refused.
func (f Files) put(gr grant, tmp string) (leftover, error) {
	path := gr.path
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err != nil || dir != filepath.Dir(path) {
		return leftover{}, fmt.Errorf("%w: %s moved since you approved the save; save again", ErrRefused, filepath.Dir(path))
	}
	now, err := f.placeOf(path)
	if err != nil {
		return leftover{}, err
	}
	_, err = os.Lstat(path)
	existing := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return leftover{}, err
	}
	if saveGrant(gr.session, now, existing) != gr {
		return leftover{}, fmt.Errorf("%w: %s changed since you approved the save; save again", ErrRefused, path)
	}
	if existing {
		mode, err := modeFor(path)
		if err != nil {
			return leftover{}, err
		}
		return f.replace(path, tmp, mode)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return leftover{}, err
	}
	err = linkNew(tmp, path)
	if errors.Is(err, fs.ErrExist) {
		return leftover{}, fmt.Errorf("%w: %s changed since you approved the save; save again", ErrRefused, path)
	}
	return leftover{}, err
}
