package codexauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"remote-agents/internal/atomicfile"
)

// EnvVar moves Codex's home, which the box's sessions may not see.
const EnvVar = "CODEX_HOME"

// Paths is a Codex home: auth.json, and the lock every Codex takes to write it.
type Paths struct {
	Home string
}

func DefaultPaths(home string) Paths { return Paths{Home: filepath.Join(home, ".codex")} }

func (p Paths) AuthFile() string { return filepath.Join(p.Home, "auth.json") }
func (p Paths) lockFile() string { return filepath.Join(p.Home, "auth.lock") }

// Config is Codex's config.toml, which may move its logins out of auth.json.
func (p Paths) Config() string { return filepath.Join(p.Home, "config.toml") }

// authMode is the mode Codex gives auth.json.
const authMode = 0o600

// lockWait bounds the wait for Codex's lock, which a Codex holds only to
// read, change and write auth.json, never across a network request.
const lockWait = 45 * time.Second

// ErrLoginChanged means the live login is no longer the one read: a Codex
// refreshed it or signed in again meanwhile.
var ErrLoginChanged = errors.New("the box's Codex login changed meanwhile; run the command again")

// Store is Codex's login on this box and the services a login talks to. It
// never refreshes the live login: Codex does that itself.
type Store struct {
	Paths     Paths
	Endpoints Endpoints
	Client    *http.Client
	Now       func() time.Time
}

// ReadLive reads the live login, nil when Codex is signed out, under the
// lock Codex takes to write it: Codex writes the file in place.
func (s Store) ReadLive(ctx context.Context) (*Login, error) {
	var live *Login
	err := s.withLock(ctx, func() (err error) {
		live, err = s.readLive()
		return err
	})
	return live, err
}

func (s Store) readLive() (*Login, error) {
	data, err := os.ReadFile(s.Paths.AuthFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l, err := ParseLogin(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Paths.AuthFile(), err)
	}
	return &l, nil
}

// LiveTx changes the live login inside WithLive.
type LiveTx struct {
	store Store
}

// WithLive runs fn holding Codex's lock, if the live login is still read.
func (s Store) WithLive(ctx context.Context, read *Login, fn func(LiveTx) error) error {
	return s.withLock(ctx, func() error {
		now, err := s.readLive()
		if err != nil {
			return err
		}
		same, err := sameLive(read, now)
		if err != nil {
			return err
		}
		if !same {
			return ErrLoginChanged
		}
		return fn(LiveTx{store: s})
	})
}

func sameLive(a, b *Login) (bool, error) {
	if a == nil || b == nil {
		return a == b, nil
	}
	x, err := a.MarshalJSON()
	if err != nil {
		return false, err
	}
	y, err := b.MarshalJSON()
	if err != nil {
		return false, err
	}
	return bytes.Equal(x, y), nil
}

// Replace makes l the live login, laid out as Codex writes auth.json.
func (tx LiveTx) Replace(l Login) error {
	path := tx.store.Paths.AuthFile()
	// A rename would put a file where the link was, which Codex would not follow.
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	data, err := l.fields.Marshal("  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, data, authMode)
}

// withLock runs fn holding Codex's exclusive flock on auth.lock.
func (s Store) withLock(ctx context.Context, fn func() error) error {
	if err := os.MkdirAll(s.Paths.Home, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.Paths.lockFile(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("locking %s: %w", s.Paths.lockFile(), err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for a Codex to release %s: %w", s.Paths.lockFile(), ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fn()
}
