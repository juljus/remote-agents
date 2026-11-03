package claudeauth

import (
	"context"
	"net/http"
	"os"
	"time"
)

// lockWait bounds the wait for a lock that a Claude Code process or another
// remote-agents command holds; Claude Code holds its refresh lock for at
// most one token request.
const lockWait = 45 * time.Second

// Store is Claude Code's login on this box and the services a login talks to.
type Store struct {
	Paths     Paths
	Endpoints Endpoints
	Client    *http.Client
	Now       func() time.Time
}

// LiveTx reads and replaces the live login while its Store holds Claude Code's refresh lock.
type LiveTx struct {
	ctx   context.Context
	store Store
}

// WithLive runs fn holding Claude Code's refresh lock, so that no Claude Code
// process refreshes or saves the login meanwhile.
func (s Store) WithLive(ctx context.Context, fn func(LiveTx) error) error {
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
		return fn(LiveTx{ctx: ctx, store: s})
	})
}

// save writes a refreshed live login even once ctx ends: the server has rotated it.
func (tx LiveTx) save(l Login) error {
	return writeLogin(context.WithoutCancel(tx.ctx), tx.store.Paths, l)
}

// Context is the context WithLive was called with.
func (tx LiveTx) Context() context.Context { return tx.ctx }

func (tx LiveTx) Read() (Live, error) { return readLive(tx.store.Paths) }

// Replace makes l and a the login of every Claude Code on the box: each
// running one reads the new credentials before its next request. Once the
// credentials changed, the account follows even if ctx ends.
func (tx LiveTx) Replace(l Login, a Account) error {
	if err := writeLogin(tx.ctx, tx.store.Paths, l); err != nil {
		return err
	}
	return writeAccount(context.WithoutCancel(tx.ctx), tx.store.Paths, a)
}

// ReplaceAccount sets only the global config's oauthAccount.
func (tx LiveTx) ReplaceAccount(a Account) error { return writeAccount(tx.ctx, tx.store.Paths, a) }

// Fresh is the live login l with an access token Claude Code would still
// use, refreshing and saving it first if needed, as Claude Code would.
func (tx LiveTx) Fresh(l Login) (Login, error) {
	return tx.store.Fresh(tx.ctx, l, tx.save)
}

// Refresh trades the live login l's refresh token for new tokens and saves them.
func (tx LiveTx) Refresh(l Login) (Login, error) {
	return tx.store.Refresh(tx.ctx, l, tx.save)
}

// Fresh is l with an access token Claude Code would still use. When it
// refreshes l, save receives the new login before Fresh returns; the caller
// holds whatever lock keeps anyone else from refreshing l meanwhile.
func (s Store) Fresh(ctx context.Context, l Login, save func(Login) error) (Login, error) {
	if !l.accessExpired(s.Now()) {
		return l, nil
	}
	return refresh(ctx, s.Client, s.Endpoints, l, s.Now, save)
}

// Refresh trades l's refresh token for new tokens whether or not its access
// token still works, saving them as Fresh does.
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
