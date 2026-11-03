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

func (tx LiveTx) Read() (Live, error) { return readLive(tx.store.Paths) }

// Replace makes l and a the login of every Claude Code on the box: each
// running one reads the new credentials before its next request.
func (tx LiveTx) Replace(l Login, a Account) error {
	if err := writeLogin(tx.store.Paths, l); err != nil {
		return err
	}
	return tx.ReplaceAccount(a)
}

// ReplaceAccount sets only the global config's oauthAccount.
func (tx LiveTx) ReplaceAccount(a Account) error {
	ctx, cancel := context.WithTimeout(tx.ctx, lockWait)
	defer cancel()
	return writeAccount(ctx, tx.store.Paths, a)
}

// Fresh is the live login l with an access token Claude Code would still
// use, refreshing and saving it first if needed, as Claude Code would.
func (tx LiveTx) Fresh(l Login) (Login, error) {
	return tx.store.Fresh(tx.ctx, l, func(next Login) error { return writeLogin(tx.store.Paths, next) })
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

// Usage reads how much of its plan limits l's account has used.
func (s Store) Usage(ctx context.Context, l Login) (Usage, error) {
	return fetchUsage(ctx, s.Client, s.Endpoints, l)
}
