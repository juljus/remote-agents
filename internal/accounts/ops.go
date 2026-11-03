package accounts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"remote-agents/internal/atomicfile"
	"remote-agents/internal/claudeauth"
)

// RefreshAhead is how long before its refresh token expires an idle login is refreshed.
const RefreshAhead = 7 * 24 * time.Hour

// Usage lists every account with its plan usage, refreshing the access
// tokens that need it first. The live login is a row even when no entry
// names it. Inactive accounts refresh under the vault's lock only, so Claude
// Code's refresh lock is held just for the live login.
func (v Vault) Usage(ctx context.Context) (Listing, error) {
	type pending struct {
		row   Row
		login *claudeauth.Login
	}
	var rows []pending
	err := v.locked(func(s *state) error {
		var activeLogin *claudeauth.Login
		active := ""
		if err := v.withLive(ctx, s, func(l *live) error {
			if l.unnamed() {
				rows = append(rows, pending{row: Row{Active: true, Email: l.email()}, login: l.login})
			}
			active, activeLogin = l.active, l.login
			return nil
		}); err != nil {
			return err
		}
		for _, name := range s.names() {
			r := pending{row: Row{Name: name, Email: s.entries[name].Account.Email(), Active: name == active}}
			if name == active {
				r.login = activeLogin
			} else if login, err := v.fresh(ctx, s, name); err != nil {
				r.row.Error = err.Error()
			} else {
				r.login = &login
			}
			rows = append(rows, r)
		}
		return nil
	})
	if err != nil {
		return Listing{}, err
	}
	var wg sync.WaitGroup
	for i := range rows {
		if rows[i].login == nil {
			continue
		}
		wg.Go(func() {
			u, err := v.Store.Usage(ctx, *rows[i].login)
			if err != nil {
				rows[i].row.Error = err.Error()
				return
			}
			rows[i].row.FiveHour, rows[i].row.SevenDay = window(u.FiveHour), window(u.SevenDay)
		})
	}
	wg.Wait()
	out := Listing{Accounts: make([]Row, len(rows))}
	for i, r := range rows {
		out.Accounts[i] = r.row
	}
	return out, nil
}

func window(w *claudeauth.Window) *Window {
	if w == nil {
		return nil
	}
	out := &Window{Utilization: w.Utilization}
	if !w.ResetsAt.IsZero() {
		t := w.ResetsAt
		out.ResetsAt = &t
	}
	return out
}

// Use switches every Claude Code on the box to the named account, saving
// the live login to its own entry first. It refuses to drop a live login no
// entry names, and to switch to an account the server signed out.
func (v Vault) Use(ctx context.Context, name string) (switched bool, err error) {
	if err := ValidName(name); err != nil {
		return false, err
	}
	err = v.locked(func(s *state) error {
		if _, ok := s.entries[name]; !ok {
			return fmt.Errorf("no account %s", name)
		}
		return v.withLive(ctx, s, func(l *live) error {
			if l.active == name {
				return nil
			}
			if l.unnamed() {
				return unnamedError(l)
			}
			login, err := v.fresh(ctx, s, name)
			if err != nil {
				return err
			}
			if err := l.tx.Replace(login, s.entries[name].Account); err != nil {
				return err
			}
			switched = true
			return nil
		})
	})
	return switched, err
}

func unnamedError(l *live) error {
	who := "an account no saved one names"
	if email := l.email(); email != "" {
		who = email
	}
	return fmt.Errorf("the box is logged in to %s; keep that login with `rclaude account save <name>` first", who)
}

// Save keeps the live login under name.
func (v Vault) Save(ctx context.Context, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return v.locked(func(s *state) error {
		return v.withLive(ctx, s, func(l *live) error {
			switch {
			case l.login == nil:
				return errors.New("the box has no login to save; log in with `rclaude account add <name>`")
			case l.active == name:
				return nil
			case l.active != "":
				return fmt.Errorf("the box's login is saved as %s already", l.active)
			case l.email() == "":
				return errors.New("the account record in Claude Code's config names another account than the box's login; start a session, then save again")
			}
			if _, taken := s.entries[name]; taken {
				return fmt.Errorf("account %s is another login", name)
			}
			return v.writeEntry(s, name, entry{Login: *l.login, Account: *l.account})
		})
	})
}

// Remove forgets an account other than the live one.
func (v Vault) Remove(ctx context.Context, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return v.locked(func(s *state) error {
		if _, ok := s.entries[name]; !ok {
			return fmt.Errorf("no account %s", name)
		}
		return v.withLive(ctx, s, func(l *live) error {
			if l.active == name {
				return fmt.Errorf("account %s is the box's login; switch to another one first", name)
			}
			delete(s.entries, name)
			return atomicfile.Remove(v.entryPath(name))
		})
	})
}

// ErrAddedLive means the login added is of the account the box uses now,
// whose entry follows the live login, so the new login went unused.
var ErrAddedLive = errors.New("that account is the box's login already")

// Add saves under name the login that signIn makes in an empty Claude Code
// config dir. Naming an existing account, it replaces that account's login,
// which must sign in as the same account.
func (v Vault) Add(ctx context.Context, name string, signIn func(configDir string) error) (err error) {
	if err := ValidName(name); err != nil {
		return err
	}
	if err := v.ensureDir(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(v.Dir, ".login-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	if err := signIn(dir); err != nil {
		return err
	}
	made, id, err := v.signedIn(ctx, dir)
	if err != nil {
		return err
	}
	return v.locked(func(s *state) error {
		return v.withLive(ctx, s, func(l *live) error {
			if other := s.named(id); other != "" && other != name {
				return fmt.Errorf("that login is account %s already", other)
			}
			if e, ok := s.entries[name]; ok && e.Account.Identity() != id {
				return fmt.Errorf("account %s is %s, not %s", name, e.Account.Email(), made.Account.Email())
			}
			if l.active == name {
				return ErrAddedLive
			}
			return v.writeEntry(s, name, entry{Login: *made.Login, Account: *made.Account})
		})
	})
}

// signedIn reads the login a sign-in left in dir and checks with the server
// that its account record names the account it signs in as.
func (v Vault) signedIn(ctx context.Context, dir string) (claudeauth.Live, claudeauth.Identity, error) {
	store := v.Store
	store.Paths = claudeauth.Paths{ConfigDir: dir, SecureDir: dir, GlobalConfig: filepath.Join(dir, ".claude.json")}
	var made claudeauth.Live
	if err := store.WithLive(ctx, func(tx claudeauth.LiveTx) (err error) {
		made, err = tx.Read()
		return err
	}); err != nil {
		return claudeauth.Live{}, claudeauth.Identity{}, err
	}
	if made.Login == nil || made.Login.SignedOut() || made.Account == nil {
		return claudeauth.Live{}, claudeauth.Identity{}, errors.New("the sign-in left no Claude login behind")
	}
	id, err := store.Identity(ctx, *made.Login)
	if err != nil {
		return claudeauth.Live{}, claudeauth.Identity{}, fmt.Errorf("asking whom the new login signs in as: %w", err)
	}
	if id != made.Account.Identity() {
		return claudeauth.Live{}, claudeauth.Identity{}, errors.New("the sign-in's account record names another account than its login")
	}
	return made, id, nil
}

// KeepFresh refreshes every saved login whose refresh token would expire
// within RefreshAhead, so an account nobody uses stays signed in. It reports
// each account it could not refresh and carries on with the others.
func (v Vault) KeepFresh(ctx context.Context, log io.Writer) error {
	var failed []error
	err := v.locked(func(s *state) error {
		now := v.Store.Now()
		due := func(l claudeauth.Login) bool {
			at, known := l.RefreshExpiresAt()
			return !known || at.Sub(now) < RefreshAhead
		}
		active := ""
		if err := v.withLive(ctx, s, func(l *live) error {
			active = l.active
			// Only a live login the vault names is the vault's to keep.
			if active == "" || !due(*l.login) {
				return nil
			}
			login, err := l.tx.Refresh(*l.login)
			if err != nil {
				failed = append(failed, fmt.Errorf("account %s, the box's login: %w", active, signedOut(active, err)))
				return nil
			}
			l.login = &login
			fmt.Fprintf(log, "refreshed account %s, the box's login\n", active)
			return v.saveLive(s, l)
		}); err != nil {
			return err
		}
		for _, name := range s.names() {
			e := s.entries[name]
			if name == active || !due(e.Login) {
				continue
			}
			_, err := v.Store.Refresh(ctx, e.Login, func(next claudeauth.Login) error {
				return v.writeEntry(s, name, entry{Login: next, Account: e.Account})
			})
			if err != nil {
				failed = append(failed, fmt.Errorf("account %s: %w", name, signedOut(name, err)))
				continue
			}
			fmt.Fprintf(log, "refreshed account %s\n", name)
		}
		return nil
	})
	return errors.Join(err, errors.Join(failed...))
}
