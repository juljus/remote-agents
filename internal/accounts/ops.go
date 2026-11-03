package accounts

import (
	"context"
	"encoding/json"
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
// tokens that need it first. The live login is a row even when no entry names it.
func (v Vault) Usage(ctx context.Context) (Listing, error) {
	type pending struct {
		row   Row
		login *claudeauth.Login
	}
	var rows []pending
	err := v.locked(ctx, func(s *state) error {
		if s.unnamedLive() {
			login, err := s.tx.Fresh(*s.live.Login)
			r := pending{row: Row{Active: true, Email: liveEmail(s.live)}}
			if err != nil {
				r.row.Error = err.Error()
			} else {
				r.login = &login
			}
			rows = append(rows, r)
		}
		for _, name := range s.names() {
			e := s.entries[name]
			r := pending{row: Row{Name: name, Email: e.Account.Email(), Active: name == s.active}}
			login, err := v.fresh(s, name)
			if err != nil {
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

// fresh is the named account's login with a usable access token: the live
// login when it is active, refreshed under Claude Code's lock, else its
// entry's, refreshed under the vault's.
func (v Vault) fresh(s *state, name string) (claudeauth.Login, error) {
	e := s.entries[name]
	if name == s.active && s.live.Login != nil && !s.live.Login.SignedOut() {
		login, err := s.tx.Fresh(*s.live.Login)
		if err != nil {
			return claudeauth.Login{}, err
		}
		s.live.Login = &login
		return login, v.saveLive(s)
	}
	login, err := v.Store.Fresh(s.tx.Context(), e.Login, func(next claudeauth.Login) error {
		saved := entry{Login: next, Account: e.Account}
		if err := v.writeEntry(name, saved); err != nil {
			return err
		}
		s.entries[name] = saved
		return nil
	})
	if errors.Is(err, claudeauth.ErrRefreshRefused) {
		return claudeauth.Login{}, fmt.Errorf("signed out: log in again with `rclaude account add %s`", name)
	}
	return login, err
}

func liveEmail(live claudeauth.Live) string {
	if live.Account == nil {
		return ""
	}
	return live.Account.Email()
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
// entry names.
func (v Vault) Use(ctx context.Context, name string) (switched bool, err error) {
	if err := ValidName(name); err != nil {
		return false, err
	}
	err = v.locked(ctx, func(s *state) error {
		if _, ok := s.entries[name]; !ok {
			return fmt.Errorf("no account %s", name)
		}
		if name == s.active {
			return nil
		}
		if s.unnamedLive() {
			return unnamedError(s.live)
		}
		// A login the server no longer honours would sign every session out.
		login, err := v.fresh(s, name)
		if err != nil {
			return err
		}
		target := entry{Login: login, Account: s.entries[name].Account}
		if err := v.writeJournal(journal{From: s.active, To: name}); err != nil {
			return err
		}
		if err := s.tx.Replace(target.Login, target.Account); err != nil {
			return err
		}
		switched = true
		return atomicfile.Remove(v.journalPath())
	})
	return switched, err
}

func unnamedError(live claudeauth.Live) error {
	who := "an account the box does not know"
	if email := liveEmail(live); email != "" {
		who = email
	}
	return fmt.Errorf("the box is logged in to %s under no name; keep that login with `rclaude account save <name>` first", who)
}

func (v Vault) writeJournal(j journal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return atomicfile.Write(v.journalPath(), data, entryMode)
}

// Save keeps the live login under name.
func (v Vault) Save(ctx context.Context, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return v.locked(ctx, func(s *state) error {
		if s.live.Login == nil || s.live.Login.SignedOut() || s.live.Account == nil {
			return errors.New("the box has no login to save; log in with `rclaude account add <name>`")
		}
		if s.active != "" {
			if s.active == name {
				return nil
			}
			return fmt.Errorf("the box's login is already saved as %s", s.active)
		}
		if _, taken := s.entries[name]; taken {
			return fmt.Errorf("account %s is another login", name)
		}
		return v.writeEntry(name, entry{Login: *s.live.Login, Account: *s.live.Account})
	})
}

// Remove forgets an account other than the live one.
func (v Vault) Remove(ctx context.Context, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return v.locked(ctx, func(s *state) error {
		if _, ok := s.entries[name]; !ok {
			return fmt.Errorf("no account %s", name)
		}
		if name == s.active {
			return fmt.Errorf("account %s is the box's login; switch to another one first", name)
		}
		return atomicfile.Remove(v.entryPath(name))
	})
}

// Add saves under name the login that signIn makes in an empty Claude Code
// config dir. Naming an existing account, it replaces that account's login,
// which must sign in as the same account.
func (v Vault) Add(ctx context.Context, name string, signIn func(configDir string) error) error {
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
	defer os.RemoveAll(dir)
	if err := signIn(dir); err != nil {
		return err
	}
	fresh := claudeauth.Store{Paths: claudeauth.Paths{ConfigDir: dir, SecureDir: dir, GlobalConfig: filepath.Join(dir, ".claude.json")}}
	var made claudeauth.Live
	if err := fresh.WithLive(ctx, func(tx claudeauth.LiveTx) error {
		made, err = tx.Read()
		return err
	}); err != nil {
		return err
	}
	if made.Login == nil || made.Login.SignedOut() || made.Account == nil {
		return errors.New("the sign-in left no Claude login behind")
	}
	return v.locked(ctx, func(s *state) error {
		id := made.Account.Identity()
		for other, e := range s.entries {
			if e.Account.Identity() == id && other != name {
				return fmt.Errorf("that login is account %s already", other)
			}
		}
		if e, ok := s.entries[name]; ok && e.Account.Identity() != id {
			return fmt.Errorf("account %s is %s, not %s", name, e.Account.Email(), made.Account.Email())
		}
		// The active account's entry follows the live login, so a new one stays unused.
		if name == s.active {
			return nil
		}
		return v.writeEntry(name, entry{Login: *made.Login, Account: *made.Account})
	})
}

// KeepFresh refreshes every saved login whose refresh token would expire
// within RefreshAhead, so an account nobody uses stays signed in. It reports each
// account it could not refresh and carries on with the others.
func (v Vault) KeepFresh(ctx context.Context, log io.Writer) error {
	var failed []error
	err := v.locked(ctx, func(s *state) error {
		now := v.Store.Now()
		due := func(l claudeauth.Login) bool {
			at, known := l.RefreshExpiresAt()
			return !known || at.Sub(now) < RefreshAhead
		}
		// Only a live login the vault names is the vault's to keep.
		if s.active != "" && s.live.Login != nil && !s.live.Login.SignedOut() && due(*s.live.Login) {
			login, err := s.tx.Refresh(*s.live.Login)
			if err != nil {
				failed = append(failed, fmt.Errorf("live login: %w", err))
			} else {
				s.live.Login = &login
				if err := v.saveLive(s); err != nil {
					return err
				}
				fmt.Fprintln(log, "refreshed the live login")
			}
		}
		for _, name := range s.names() {
			e := s.entries[name]
			if name == s.active || !due(e.Login) {
				continue
			}
			_, err := v.Store.Refresh(s.tx.Context(), e.Login, func(next claudeauth.Login) error {
				return v.writeEntry(name, entry{Login: next, Account: e.Account})
			})
			if err != nil {
				failed = append(failed, fmt.Errorf("account %s: %w", name, err))
				continue
			}
			fmt.Fprintf(log, "refreshed account %s\n", name)
		}
		return nil
	})
	return errors.Join(err, errors.Join(failed...))
}
