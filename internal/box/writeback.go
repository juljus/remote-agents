package box

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/linkproto"
)

// WriteBackPoll paces the look for changed session copies; a copy goes back
// once it has stayed the same for one poll, so a write in progress does not.
const WriteBackPoll = 400 * time.Millisecond

// WriteBackRetry is how long a copy whose write-back failed waits for the
// next try; each failure in a row doubles it, up to maxRetryDoublings times.
const WriteBackRetry = 30 * time.Second

const maxRetryDoublings = 5

// writeBackTimeout bounds one write-back; the laptop fetches up to 100 MB.
const writeBackTimeout = 10 * time.Minute

// fileSig tells one version of a file from the next without reading it.
type fileSig struct {
	size  int64
	mtime time.Time
	inode uint64
}

func sigOf(path string) (fileSig, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileSig{}, false, nil
	}
	if err != nil {
		return fileSig{}, false, err
	}
	if !info.Mode().IsRegular() {
		return fileSig{}, false, nil
	}
	sig := fileSig{size: info.Size(), mtime: info.ModTime()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		sig.inode = uint64(st.Ino)
	}
	return sig, true, nil
}

// copyWatch is what the watcher knows of one copy: how it looked at the last
// poll, which version of it the laptop has, and when a failed one goes again.
type copyWatch struct {
	seen     fileSig
	handled  fileSig
	failures int
	retryAt  time.Time
}

// watchSessionCopies sends each session copy the agent changed back to the
// laptop, until ctx ends. A link holder runs it, so copies changed while the
// link was down go once it is up; a copy that changed is hashed, and goes
// only when it holds other content than the laptop's.
func watchSessionCopies(ctx context.Context, l boxlayout.Layout, laptop Laptop, poll, retry time.Duration, logf func(string, ...any)) {
	watched := map[string]*copyWatch{}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		copies, err := sessionCopies(l)
		if err != nil {
			logf("write-back: %v", err)
			continue
		}
		present := map[string]bool{}
		for _, c := range copies {
			present[c.record] = true
			w, ok := watched[c.record]
			if !ok {
				w = &copyWatch{}
				watched[c.record] = w
			}
			if err := checkCopy(ctx, l, laptop, c, w, logf); err != nil {
				delay := retry << min(w.failures, maxRetryDoublings)
				w.failures++
				w.retryAt = time.Now().Add(delay)
				logf("write-back of %s: %v; trying again in %s", c.Path, err, delay)
			}
		}
		for record := range watched {
			if !present[record] {
				delete(watched, record)
			}
		}
	}
}

// checkCopy sends c back when it settled on content the laptop lacks.
func checkCopy(ctx context.Context, l boxlayout.Layout, laptop Laptop, c pastedCopy, w *copyWatch, logf func(string, ...any)) error {
	sig, ok, err := sigOf(c.Path)
	if err != nil || !ok {
		return err
	}
	if sig != w.seen {
		w.seen = sig
		return nil
	}
	if sig == w.handled || time.Now().Before(w.retryAt) {
		return nil
	}
	sum, err := fileSum(c.Path)
	if err != nil {
		return err
	}
	if sum != c.Synced {
		reqCtx, cancel := context.WithTimeout(ctx, writeBackTimeout)
		defer cancel()
		res, err := laptop.WriteBack(reqCtx, linkproto.WriteBackRequest{Session: c.Session, Origin: c.Origin, BoxPath: c.Path})
		if err != nil {
			return err
		}
		if err := markSynced(l, c, res.SHA256); err != nil {
			return err
		}
		logf("write-back of %s: %s, %s", c.Path, res.Outcome, res.Path)
	}
	w.handled, w.failures = sig, 0
	return nil
}
