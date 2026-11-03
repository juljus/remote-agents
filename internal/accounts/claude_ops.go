package accounts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"remote-agents/internal/claudeauth"
)

// RefreshAhead is how long before its refresh token expires an idle login is refreshed.
const RefreshAhead = 7 * 24 * time.Hour

// unverifiedHint says how an unverified box login gets verified again.
const unverifiedHint = "a running session refreshes it on its next request"

// Usage lists every account with its plan usage. The live login is a row
// even when no entry names it. Only inactive accounts are refreshed first:
// the live login's usage reads with its own access token or not at all.
func (v ClaudeVault) Usage(ctx context.Context) (Listing, error) {
	type pending = pendingRow[claudeauth.Login]
	var rows []pending
	err := v.locked(func(s *claudeState) error {
		l, err := v.live(ctx, s)
		if err != nil {
			return err
		}
		liveRow := func(r Row) pending {
			if l.state == liveVerified {
				return pending{row: r, login: l.read.Login}
			}
			r.Error = fmt.Sprintf("the box's login is unverified: %s; %s", l.unverified, unverifiedHint)
			return pending{row: r}
		}
		if l.unnamed() || l.state == liveUnknown {
			rows = append(rows, liveRow(Row{Active: true, Email: l.email()}))
		}
		for _, name := range s.names() {
			e := s.entries[name]
			r := Row{Name: name, Email: e.Account.Email(), Active: l.state != liveUnknown && name == l.active}
			switch {
			case r.Active:
				rows = append(rows, liveRow(r))
			case l.possiblyLive(name) && e.Login.AccessExpired(v.Store.Now()):
				r.Error = "not refreshed while the box's login is unverified"
				rows = append(rows, pending{row: r})
			case l.possiblyLive(name):
				login := e.Login
				rows = append(rows, pending{row: r, login: &login})
			default:
				login, err := v.fresh(ctx, s, name)
				if err != nil {
					r.Error = err.Error()
					rows = append(rows, pending{row: r})
					continue
				}
				rows = append(rows, pending{row: r, login: &login})
			}
		}
		return nil
	})
	if err != nil {
		return Listing{}, err
	}
	return readUsage(rows, func(login claudeauth.Login) (*Window, *Window, error) {
		u, err := v.Store.Usage(ctx, login)
		if err != nil {
			return nil, nil, err
		}
		return claudeWindow(u.FiveHour), claudeWindow(u.SevenDay), nil
	}), nil
}

func claudeWindow(w *claudeauth.Window) *Window {
	if w == nil {
		return nil
	}
	return newWindow(w.Utilization, w.ResetsAt)
}

// Use switches every Claude Code on the box to the named account, keeping
// the live login in its own entry first. It refuses to drop a live login
// whose tokens it cannot keep, and to switch to an account the server signed out.
func (v ClaudeVault) Use(ctx context.Context, name string) (switched bool, err error) {
	if err := ValidName(name); err != nil {
		return false, err
	}
	err = v.locked(func(s *claudeState) error {
		if _, ok := s.entries[name]; !ok {
			return fmt.Errorf("no account %s", name)
		}
		l, err := v.live(ctx, s)
		if err != nil {
			return err
		}
		switch {
		case l.state == liveUnknown:
			return fmt.Errorf("the box's login is unverified (%s) and no saved account is known to be it, so switching could lose it; %s, then switch", l.unverified, unverifiedHint)
		case l.state != liveNone && l.active == name:
			return nil
		case l.possiblyLive(name):
			return copyError(name)
		case l.unnamed():
			return unnamedError(l)
		case l.state == liveGuessed && s.entries[l.active].Login.NewerThan(*l.read.Login):
			return fmt.Errorf("the box's login is unverified (%s) and account %s holds a newer login than it; %s, then switch", l.unverified, l.active, unverifiedHint)
		}
		login, err := v.fresh(ctx, s, name)
		if err != nil {
			return err
		}
		return v.Store.WithLive(ctx, l.read, func(tx claudeauth.LiveTx) error {
			// The login is still the one read, so it is the latest to keep.
			if l.state == liveGuessed {
				if err := v.writeEntry(s, l.active, claudeEntry{Login: *l.read.Login, Account: s.entries[l.active].Account}); err != nil {
					return err
				}
			}
			// Marked first: a switch that stops after the login write leaves
			// an account the mark does not name, so its login is never guessed.
			if err := v.markSwitched(name); err != nil {
				return err
			}
			if err := tx.Replace(login, s.entries[name].Account); err != nil {
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

// Save keeps the verified live login under name.
func (v ClaudeVault) Save(ctx context.Context, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return v.locked(func(s *claudeState) error {
		l, err := v.live(ctx, s)
		if err != nil {
			return err
		}
		switch {
		case l.state == liveNone:
			return errors.New("the box has no login to save; log in with `rclaude account add <name>`")
		case l.active == name:
			return nil
		case l.state != liveVerified:
			return fmt.Errorf("the box's login is unverified (%s); %s, then save", l.unverified, unverifiedHint)
		case l.active != "":
			return fmt.Errorf("the box's login is saved as %s already", l.active)
		case l.email() == "":
			return errors.New("the account record in Claude Code's config names another account than the box's login; start a session, then save again")
		}
		if _, taken := s.entries[name]; taken {
			return fmt.Errorf("account %s is another login", name)
		}
		return v.nameLive(s, l, name, *l.read.Account)
	})
}

// nameLive saves the verified live login, which no entry names, under name
// with account as its record.
func (v ClaudeVault) nameLive(s *claudeState, l *live, name string, account claudeauth.Account) error {
	if err := v.writeEntry(s, name, claudeEntry{Login: *l.read.Login, Account: account}); err != nil {
		return err
	}
	return v.markSwitched(name)
}

// Remove forgets an account that is not, and may not be, the live one.
func (v ClaudeVault) Remove(ctx context.Context, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return v.locked(func(s *claudeState) error {
		if _, ok := s.entries[name]; !ok {
			return fmt.Errorf("no account %s", name)
		}
		l, err := v.live(ctx, s)
		if err != nil {
			return err
		}
		if l.possiblyLive(name) {
			return fmt.Errorf("account %s is, or may be, the box's login; switch to another one first", name)
		}
		return removeEntry(v.files(), s, name)
	})
}

// Add saves under name the login that signIn makes in an empty Claude Code
// config dir. Naming an existing account, it replaces that account's login,
// which must sign in as the same account and must not be the box's login.
// When the box is logged in to the account already, its login is kept
// instead and the new one goes unused.
func (v ClaudeVault) Add(ctx context.Context, name string, signIn func(configDir string) error) (added Added, err error) {
	if err := ValidName(name); err != nil {
		return 0, err
	}
	var (
		made claudeauth.Live
		id   claudeauth.Identity
	)
	err = v.files().signIn(func(dir string) error {
		if err := signIn(dir); err != nil {
			return err
		}
		made, id, err = v.signedIn(ctx, dir)
		return err
	})
	if err != nil {
		return 0, err
	}
	err = v.locked(func(s *claudeState) error {
		if err := v.files().removeStaleSignIns(); err != nil {
			return err
		}
		l, err := v.live(ctx, s)
		if err != nil {
			return err
		}
		if other := claudeNamed(s, id); other != "" && other != name {
			return fmt.Errorf("that login is account %s already", other)
		}
		e, ok := s.entries[name]
		if ok && e.Account.Identity() != id {
			return fmt.Errorf("account %s is %s, not %s", name, e.Account.Email(), made.Account.Email())
		}
		switch {
		case l.state == liveVerified && *l.id == id && l.active == name:
			added = AddedAlready
			return nil
		case l.state == liveVerified && *l.id == id:
			added = AddedLive
			return v.nameLive(s, l, name, *made.Account)
		case ok && l.state == liveVerified && l.possiblyLive(name):
			return copyError(name)
		case ok && l.possiblyLive(name):
			return fmt.Errorf("account %s may be the box's login, which is unverified (%s); %s, or sign in again with /login inside a session", name, l.unverified, unverifiedHint)
		}
		added = AddedNew
		return v.writeEntry(s, name, claudeEntry{Login: *made.Login, Account: *made.Account})
	})
	return added, err
}

// signedIn reads the login a sign-in left in dir and checks with the server
// that its account record names the account it signs in as.
func (v ClaudeVault) signedIn(ctx context.Context, dir string) (claudeauth.Live, claudeauth.Identity, error) {
	paths, err := claudeauth.PathsIn(dir)
	if err != nil {
		return claudeauth.Live{}, claudeauth.Identity{}, err
	}
	store := v.Store
	store.Paths = paths
	made, err := store.ReadLive(ctx)
	if err != nil {
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

// KeepFresh refreshes every inactive saved login whose refresh token would
// expire within RefreshAhead, so an account nobody uses stays signed in. It
// never touches the live login, nor a saved login that may be a copy of it.
// It reports each account it could not refresh and carries on with the others.
func (v ClaudeVault) KeepFresh(ctx context.Context, log io.Writer) error {
	var failed []error
	err := v.locked(func(s *claudeState) error {
		if len(s.entries) == 0 {
			return nil
		}
		l, err := v.live(ctx, s)
		if err != nil {
			return err
		}
		// An error, so the timer shows that saved logins are left to expire.
		if l.state == liveUnknown {
			return fmt.Errorf("the box's login is unverified (%s) and no saved account is known to be it, so none was refreshed; %s", l.unverified, unverifiedHint)
		}
		now := v.Store.Now()
		for _, name := range s.names() {
			e := s.entries[name]
			at, known := e.Login.RefreshExpiresAt()
			if l.possiblyLive(name) || (known && at.Sub(now) >= RefreshAhead) {
				continue
			}
			_, err := v.Store.Refresh(ctx, e.Login, func(next claudeauth.Login) error {
				return v.writeEntry(s, name, claudeEntry{Login: next, Account: e.Account})
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
