package claudeauth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fastTiming = lockTiming{stale: time.Second, update: 50 * time.Millisecond}

func TestDirLockExcludesUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	first, err := acquireDirLock(context.Background(), path, fastTiming)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := acquireDirLock(ctx, path, fastTiming); err == nil || !strings.Contains(err.Error(), "still held") {
		t.Fatalf("second acquire = %v, want still held", err)
	}
	got := make(chan error, 1)
	go func() {
		l, err := acquireDirLock(context.Background(), path, fastTiming)
		if err == nil {
			err = l.release()
		}
		got <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := first.release(); err != nil {
		t.Fatal(err)
	}
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("lock left behind: %v", err)
	}
}

func TestDirLockStaysFreshWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l, err := acquireDirLock(context.Background(), path, fastTiming)
	if err != nil {
		t.Fatal(err)
	}
	// Held past stale, it is still touched, so no one takes it.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := acquireDirLock(ctx, path, fastTiming); err == nil {
		t.Fatal("a held lock was taken")
	}
	if err := l.release(); err != nil {
		t.Fatal(err)
	}
}

func TestDirLockRemovesAnAbandonedLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * fastTiming.stale)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l, err := acquireDirLock(ctx, path, fastTiming)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.release(); err != nil {
		t.Fatal(err)
	}
}

func TestDirLockReportsATakenLockAndLeavesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l, err := acquireDirLock(context.Background(), path, fastTiming)
	if err != nil {
		t.Fatal(err)
	}
	// Someone judged it abandoned and took it over.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if err := l.release(); err == nil || !strings.Contains(err.Error(), "taken from this process") {
		t.Fatalf("release = %v, want taken", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the new holder's lock was removed: %v", err)
	}
}

func TestWithLocksReleasesInReverseAfterAFailedAcquire(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.lock"), filepath.Join(dir, "b.lock")
	if err := os.Mkdir(b, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ran := false
	err := withLocks(ctx, []string{a, b}, fastTiming, func() error { ran = true; return nil })
	if err == nil || ran {
		t.Fatalf("withLocks = %v, ran %v", err, ran)
	}
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Fatalf("first lock not released: %v", err)
	}
}
