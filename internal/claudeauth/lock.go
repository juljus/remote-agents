package claudeauth

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"time"
)

// lockTiming is proper-lockfile's, which Claude Code locks with: a lock is a
// directory whose holder touches it every update; one untouched for stale is
// abandoned and anyone may remove it.
type lockTiming struct{ stale, update time.Duration }

var (
	// Claude Code's OAuth refresh lock.
	refreshLockTiming = lockTiming{stale: 60 * time.Second, update: 5 * time.Second}
	// proper-lockfile's defaults, which Claude Code's global config lock uses.
	configLockTiming = lockTiming{stale: 10 * time.Second, update: 5 * time.Second}
)

const lockPoll = 100 * time.Millisecond

// dirLock is one held proper-lockfile lock.
type dirLock struct {
	path    string
	stop    chan struct{}
	stopped chan struct{}
	// mtime and lost belong to keepFresh until it stops.
	mtime time.Time
	lost  error
}

// acquireDirLock waits until it holds the lock at path or ctx ends.
func acquireDirLock(ctx context.Context, path string, timing lockTiming) (*dirLock, error) {
	for {
		err := os.Mkdir(path, 0o777)
		if err == nil {
			info, err := os.Stat(path)
			if err != nil {
				return nil, errors.Join(fmt.Errorf("lock %s: %w", path, err), os.Remove(path))
			}
			l := &dirLock{path: path, stop: make(chan struct{}), stopped: make(chan struct{}), mtime: info.ModTime()}
			go l.keepFresh(timing.update)
			return l, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		info, err := os.Stat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return nil, fmt.Errorf("lock %s: %w", path, err)
		case time.Since(info.ModTime()) > timing.stale:
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("removing abandoned lock %s: %w", path, err)
			}
			continue
		}
		wait := lockPoll + rand.N(lockPoll)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s is still held (another Claude Code or remote-agents process is using the login): %w", path, ctx.Err())
		case <-time.After(wait):
		}
	}
}

// keepFresh touches the lock every update, as its holder must, and notes
// when someone else replaced or removed it.
func (l *dirLock) keepFresh(update time.Duration) {
	defer close(l.stopped)
	t := time.NewTicker(update)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
		if err := l.touch(); err != nil {
			l.lost = err
			return
		}
	}
}

func (l *dirLock) touch() error {
	if err := l.check(); err != nil {
		return err
	}
	now := time.Now()
	if err := os.Chtimes(l.path, now, now); err != nil {
		return fmt.Errorf("touching lock %s: %w", l.path, err)
	}
	info, err := os.Stat(l.path)
	if err != nil {
		return fmt.Errorf("lock %s: %w", l.path, err)
	}
	l.mtime = info.ModTime()
	return nil
}

// check fails once the lock is no longer the one this holder last touched.
func (l *dirLock) check() error {
	info, err := os.Stat(l.path)
	if err != nil {
		return fmt.Errorf("lock %s was taken from this process: %w", l.path, err)
	}
	if !info.ModTime().Equal(l.mtime) {
		return fmt.Errorf("lock %s was taken from this process", l.path)
	}
	return nil
}

// release removes the lock unless it was taken meanwhile, which it reports.
func (l *dirLock) release() error {
	close(l.stop)
	<-l.stopped
	if l.lost != nil {
		return l.lost
	}
	if err := l.check(); err != nil {
		return err
	}
	if err := os.Remove(l.path); err != nil {
		return fmt.Errorf("releasing lock %s: %w", l.path, err)
	}
	return nil
}

// withLocks runs fn holding the locks at paths, taken in order and released in reverse.
func withLocks(ctx context.Context, paths []string, timing lockTiming, fn func() error) (err error) {
	held := make([]*dirLock, 0, len(paths))
	defer func() {
		for i := len(held) - 1; i >= 0; i-- {
			err = errors.Join(err, held[i].release())
		}
	}()
	for _, p := range paths {
		l, err := acquireDirLock(ctx, p, timing)
		if err != nil {
			return err
		}
		held = append(held, l)
	}
	return fn()
}
