package box

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
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

// writeBackTimeout bounds one write-back: the wait for the user's approval
// on the laptop, which a risky place needs, then a fetch of up to 100 MB.
const writeBackTimeout = linkproto.MaxApprovalWait + 10*time.Minute

// maxWriteBacks bounds the write-backs in flight at once. One may wait for
// the user to approve it, which the others need not wait for.
const maxWriteBacks = 4

// fileSig tells one version of a file from the next without reading it.
type fileSig struct {
	size  int64
	mtime time.Time
	inode uint64
}

// sigOf follows a symlink at path, as send-file does.
func sigOf(path string) (fileSig, bool, error) {
	info, err := os.Stat(path)
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
	seen fileSig
	// handled is the version last dealt with, and the record it left; a paste
	// changes the record, which makes the version worth another look.
	handled    fileSig
	handledRec copyRecord
	failures   int
	retryAt    time.Time
	sending    bool
	// absent counts the polls in a row that found no file.
	absent int
}

// failed puts off the next try of a copy whose last one failed.
func (w *copyWatch) failed(path string, err error, retry time.Duration, logf func(string, ...any)) {
	delay := retry << min(w.failures, maxRetryDoublings)
	w.failures++
	w.retryAt = time.Now().Add(delay)
	logf("write-back of %s: %v; trying again in %s", path, err, delay)
}

// sent is how a write-back of the copy at path, whose record is record,
// ended: the version sent, the record it left, or why it failed.
type sent struct {
	path   string
	record string
	sig    fileSig
	rec    copyRecord
	err    error
}

// watchSessionCopies sends each session copy the agent changed back to the
// laptop, until ctx ends. A link holder runs it, so copies changed while the
// link was down go once it is up; a copy that changed is hashed, and goes
// only when it holds other content than the laptop's. An older link holder
// lingers until its link times out, so the watchers take turns by a lock:
// two would fetch one copy twice and could land an older version last.
func watchSessionCopies(ctx context.Context, l boxlayout.Layout, laptop Laptop, poll, retry time.Duration, logf func(string, ...any)) {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	lock, err := awaitFlock(ctx, l.WriteBackLock(), ticker.C)
	if err != nil {
		if ctx.Err() == nil {
			logf("write-back: %v", err)
		}
		return
	}
	defer lock.Close()
	done := make(chan sent)
	var sending sync.WaitGroup
	// The lock stays held until every write-back sent has ended.
	defer sending.Wait()
	inFlight := 0
	watched := map[string]*copyWatch{}
	listErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-done:
			inFlight--
			if w, ok := watched[s.record]; ok {
				w.sending = false
				if s.err != nil {
					w.failed(s.path, s.err, retry, logf)
				} else {
					w.handled, w.handledRec, w.failures = s.sig, s.rec, 0
				}
			}
			continue
		case <-ticker.C:
		}
		copies, err := sessionCopies(l)
		// A broken record stays broken; once in the log is enough.
		if err != nil && err.Error() != listErr {
			logf("write-back: %v", err)
		}
		listErr = ""
		if err != nil {
			listErr = err.Error()
		}
		present := map[string]bool{}
		for _, c := range copies {
			present[c.record] = true
			// At the bound, a changed copy waits unhashed for a slot.
			if inFlight == maxWriteBacks {
				continue
			}
			w, ok := watched[c.record]
			if !ok {
				w = &copyWatch{}
				watched[c.record] = w
			}
			sum, due, err := checkCopy(c, w)
			if err != nil {
				w.failed(c.Path, err, retry, logf)
				continue
			}
			// A box file gone for a whole poll, not just replaced, is gone for good.
			if c.Linked && w.absent > 1 {
				if err := unlink(l, c); err != nil {
					w.failed(c.Path, err, retry, logf)
					continue
				}
				logf("write-back: %s is gone, so %s no longer follows it", c.Path, c.Origin)
				continue
			}
			if !due {
				continue
			}
			w.sending = true
			inFlight++
			sending.Add(1)
			go func(c pastedCopy, sig fileSig) {
				defer sending.Done()
				rec, err := sendCopy(ctx, l, laptop, c, sum, logf)
				select {
				case done <- sent{path: c.Path, record: c.record, sig: sig, rec: rec, err: err}:
				case <-ctx.Done():
				}
			}(c, w.seen)
		}
		// A listing with errors may lack copies that are still there.
		if err != nil {
			continue
		}
		for record, w := range watched {
			if !present[record] && !w.sending {
				delete(watched, record)
			}
		}
	}
}

// checkCopy reports whether c settled on content the laptop lacks, of hash
// sum; a copy that holds what the laptop has is dealt with.
func checkCopy(c pastedCopy, w *copyWatch) (string, bool, error) {
	if w.sending {
		return "", false, nil
	}
	sig, ok, err := sigOf(c.Path)
	if err != nil {
		return "", false, err
	}
	if !ok {
		w.absent++
		return "", false, nil
	}
	w.absent = 0
	if sig != w.seen {
		w.seen = sig
		return "", false, nil
	}
	rec := c.rec()
	if sig == w.handled && rec == w.handledRec || time.Now().Before(w.retryAt) {
		return "", false, nil
	}
	sum, err := fileSum(c.Path)
	if err != nil {
		return "", false, err
	}
	if sum == c.Synced || sum == c.Refused {
		w.handled, w.handledRec, w.failures = sig, rec, 0
		return "", false, nil
	}
	return sum, true, nil
}

// sendCopy sends c, of content hash sum, back to the laptop and gives the
// record that leaves.
func sendCopy(ctx context.Context, l boxlayout.Layout, laptop Laptop, c pastedCopy, sum string, logf func(string, ...any)) (copyRecord, error) {
	reqCtx, cancel := context.WithTimeout(ctx, writeBackTimeout)
	defer cancel()
	rec := c.rec()
	res, err := laptop.WriteBack(reqCtx, linkproto.WriteBackRequest{Session: c.Session, Origin: c.Origin, BoxPath: c.Path})
	switch {
	case errors.Is(err, linkproto.ErrRefused):
		// Asking again would be refused again; the next change asks anew.
		logf("write-back of %s: %v; not asking again until it changes or is pasted again", c.Path, err)
		rec.Refused = sum
		return rec, markRefused(l, c, sum)
	case err != nil:
		return rec, err
	}
	logf("write-back of %s: %s, %s", c.Path, res.Outcome, res.Path)
	rec.Synced, rec.Refused = res.SHA256, ""
	return rec, markSynced(l, c, res.SHA256)
}

// awaitFlock takes an exclusive flock on path, trying again at each tick
// until it gets it or ctx ends. Closing the file releases it.
func awaitFlock(ctx context.Context, path string, tick <-chan time.Time) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(fmt.Errorf("locking %s: %w", path, err), f.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), f.Close())
		case <-tick:
		}
	}
}
