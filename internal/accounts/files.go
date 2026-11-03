// Package accounts keeps several logins of an agent on the box under names
// and switches the box between them. The live login stays where the agent
// keeps it; a vault holds every other account's login, and a copy of the
// live one that each command brings up to date.
package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"remote-agents/internal/atomicfile"
)

// Names start with a letter, so a picker can tell them from row numbers.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidName keeps names usable as file names and in remote command lines.
func ValidName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid account name %q: want a lowercase letter, then up to 31 lowercase letters, digits and dashes", name)
	}
	return nil
}

// files is a vault's dir: an entry file per account, the lock that orders
// every use of their refresh tokens, and the dirs sign-ins run in.
type files struct {
	dir string
}

const entryMode = 0o600

// scratchPrefix names the agent config dirs that Add signs in to and Start runs in.
const scratchPrefix = ".login-"

// signInMaxAge is how long a sign-in may take before its leftover dir is a
// crash's to remove.
const signInMaxAge = time.Hour

func (f files) entryPath(name string) string { return filepath.Join(f.dir, name+".json") }
func (f files) lockPath() string             { return filepath.Join(f.dir, ".lock") }

// ensureDir creates the vault dir, or narrows an existing one, to mode 0700.
func (f files) ensureDir() error {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(f.dir, 0o700)
}

// state is a vault's entries, read under its lock.
type state[E any] struct {
	entries map[string]E
}

func (s *state[E]) names() []string {
	names := make([]string, 0, len(s.entries))
	for n := range s.entries {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// locked runs fn holding the vault's lock, on the entries parse reads.
func locked[E any](f files, parse func([]byte) (E, error), fn func(*state[E]) error) error {
	if err := f.ensureDir(); err != nil {
		return err
	}
	lock, err := os.OpenFile(f.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking the account vault: %w", err)
	}
	s, err := read(f, parse)
	if err != nil {
		return err
	}
	return fn(s)
}

func read[E any](f files, parse func([]byte) (E, error)) (*state[E], error) {
	s := &state[E]{entries: map[string]E{}}
	names, err := entryNames(f.dir)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		data, err := os.ReadFile(f.entryPath(name))
		if err != nil {
			return nil, err
		}
		e, err := parse(data)
		if err != nil {
			return nil, fmt.Errorf("account %s: %w", name, err)
		}
		s.entries[name] = e
	}
	return s, nil
}

// Saved reports whether the vault in dir holds any account.
func Saved(dir string) (bool, error) {
	names, err := entryNames(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return len(names) > 0, err
}

// entryNames are the accounts of the vault in dir.
func entryNames(dir string) ([]string, error) {
	dirents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range dirents {
		// The lock, record and temporary files have names no account can have.
		if name, ok := strings.CutSuffix(d.Name(), ".json"); ok && ValidName(name) == nil {
			names = append(names, name)
		}
	}
	return names, nil
}

func writeEntry[E any](f files, s *state[E], name string, e E) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(f.entryPath(name), data, entryMode); err != nil {
		return err
	}
	s.entries[name] = e
	return nil
}

// renameEntry moves old's entry to new, which must be free.
func renameEntry[E any](f files, s *state[E], old, new string) error {
	if err := ValidName(new); err != nil {
		return err
	}
	if _, ok := s.entries[old]; !ok {
		return fmt.Errorf("no account %s", old)
	}
	if _, ok := s.entries[new]; ok {
		return fmt.Errorf("account %s exists already", new)
	}
	if err := os.Rename(f.entryPath(old), f.entryPath(new)); err != nil {
		return err
	}
	s.entries[new] = s.entries[old]
	delete(s.entries, old)
	return atomicfile.SyncDir(f.dir)
}

func removeEntry[E any](f files, s *state[E], name string) error {
	delete(s.entries, name)
	return atomicfile.Remove(f.entryPath(name))
}

// sameJSON reports whether two entries marshal alike.
func sameJSON[E any](a, b E) (bool, error) {
	x, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	y, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return string(x) == string(y), nil
}

// scratch runs fn in a new empty dir of the vault, which it removes after.
func (f files) scratch(fn func(dir string) error) (err error) {
	if err := f.ensureDir(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(f.dir, scratchPrefix)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	return fn(dir)
}

// removeStaleScratch removes the scratch dirs a crashed command left behind; the
// caller holds the vault lock.
func (f files) removeStaleScratch() error {
	dirs, err := filepath.Glob(filepath.Join(f.dir, scratchPrefix+"*"))
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if time.Since(info.ModTime()) < signInMaxAge {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}

// Added says what Add kept.
type Added int

const (
	// AddedNew: the new login is saved under the name.
	AddedNew Added = iota
	// AddedLive: the box was logged in to the account already, without a
	// name; its login is saved under the name and the new one dropped.
	AddedLive
	// AddedAlready: the name is the box's login already, which stays.
	AddedAlready
)
