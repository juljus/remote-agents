package claudeauth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"
)

// lockWait bounds the wait for a lock that a Claude Code process or another
// remote-agents command holds; Claude Code holds its refresh lock for at
// most one token request.
const lockWait = 45 * time.Second

// ErrLoginChanged means the live login is no longer the one read: a Claude
// Code process refreshed it or signed in again meanwhile.
var ErrLoginChanged = errors.New("the box's login changed meanwhile; run the command again")

// Store is Claude Code's login on this box and the services a login talks
// to. It never refreshes the live login: Claude Code does that itself.
type Store struct {
	Paths     Paths
	Endpoints Endpoints
	Client    *http.Client
	Now       func() time.Time
}

// ReadLive reads the live login under the lock Claude Code takes for each
// write of its credentials.
func (s Store) ReadLive(ctx context.Context) (Live, error) {
	if err := os.MkdirAll(s.Paths.SecureDir, 0o700); err != nil {
		return Live{}, err
	}
	var live Live
	err := withStorageLock(ctx, s.Paths, func() (err error) {
		live, err = readLive(s.Paths)
		return err
	})
	return live, err
}

// LiveTx replaces the live login while its Store holds Claude Code's refresh lock.
type LiveTx struct {
	ctx   context.Context
	store Store
	read  *Login
}

// WithLive runs fn holding Claude Code's refresh lock, so that no Claude Code
// process is midway through refreshing the login, once the live login is
// still read's; else it returns ErrLoginChanged.
func (s Store) WithLive(ctx context.Context, read Live, fn func(LiveTx) error) error {
	// Like Claude Code, create the dir the lock lives in.
	if err := os.MkdirAll(s.Paths.SecureDir, 0o700); err != nil {
		return err
	}
	locks, err := s.Paths.refreshLocks()
	if err != nil {
		return err
	}
	lockCtx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	return withLocks(lockCtx, locks[:], refreshLockTiming, func() error {
		now, err := s.ReadLive(ctx)
		if err != nil {
			return err
		}
		if !sameLogin(read.Login, now.Login) {
			return ErrLoginChanged
		}
		return fn(LiveTx{ctx: ctx, store: s, read: read.Login})
	})
}

// sameLogin reports whether a and b are the same login, or both none.
func sameLogin(a, b *Login) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.SameTokens(*b)
}

// Replace makes l and a the login of every Claude Code on the box: each
// running one reads the new credentials before its next request. Once the
// credentials changed, the account follows even if ctx ends.
func (tx LiveTx) Replace(l Login, a Account) error {
	if err := writeLogin(tx.ctx, tx.store.Paths, tx.read, l); err != nil {
		return err
	}
	return writeAccount(context.WithoutCancel(tx.ctx), tx.store.Paths, a)
}

// ReplaceAccount sets only the global config's oauthAccount.
func (tx LiveTx) ReplaceAccount(a Account) error { return writeAccount(tx.ctx, tx.store.Paths, a) }

// Fresh is a saved login l with an access token Claude Code would still use.
// When it refreshes l, save receives the new login before Fresh returns; the
// caller holds whatever lock keeps anyone else from refreshing l meanwhile,
// and l is never the live login.
func (s Store) Fresh(ctx context.Context, l Login, save func(Login) error) (Login, error) {
	if !l.accessExpired(s.Now()) {
		return l, nil
	}
	return refresh(ctx, s.Client, s.Endpoints, l, s.Now, save)
}

// Refresh trades a saved login's refresh token for new tokens whether or not
// its access token still works, saving them as Fresh does.
func (s Store) Refresh(ctx context.Context, l Login, save func(Login) error) (Login, error) {
	return refresh(ctx, s.Client, s.Endpoints, l, s.Now, save)
}

// Identity is whom l's access token signs in as, by the server's word.
func (s Store) Identity(ctx context.Context, l Login) (Identity, error) {
	return fetchIdentity(ctx, s.Client, s.Endpoints, l)
}

// Usage reads how much of its plan limits l's account has used.
func (s Store) Usage(ctx context.Context, l Login) (Usage, error) {
	return fetchUsage(ctx, s.Client, s.Endpoints, l)
}
