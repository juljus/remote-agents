package accounts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"remote-agents/internal/codexauth"
)

// CodexVault is the named Codex logins of one box install, in Dir.
type CodexVault struct {
	Dir   string
	Store codexauth.Store
	// Running lists the Codex processes that use the box's Codex home.
	Running func() (CodexRunning, error)
}

// CodexRunning are the Codex processes that use the box's Codex home.
type CodexRunning struct {
	PIDs []int
	// Sessions are the box's sessions among them, which rcodex can resume.
	Sessions []string
}

// codexRefreshAhead is how long before its access token expires KeepFresh
// refreshes an idle login: the timer runs daily, so each login rotates as
// often as a Codex that used it would rotate it.
const codexRefreshAhead = 24 * time.Hour

type codexState = state[codexauth.Login]

func (v CodexVault) files() files { return files{dir: v.Dir} }

func (v CodexVault) locked(fn func(*codexState) error) error {
	return locked(v.files(), codexauth.ParseLogin, fn)
}

func (v CodexVault) writeEntry(s *codexState, name string, l codexauth.Login) error {
	return writeEntry(v.files(), s, name, l)
}

// codexNamed is the entry that signs in as id, or "".
func codexNamed(s *codexState, id codexauth.Identity) string {
	for _, name := range s.names() {
		if s.entries[name].Identity() == id {
			return name
		}
	}
	return ""
}

// codexLive is the box's Codex login as one command read it. Codex keeps
// whom it signs in as in the login itself, so it is never in doubt.
type codexLive struct {
	// read is nil when Codex is signed out or signed in without ChatGPT.
	read *codexauth.Login
	// other: auth.json holds another kind of login, such as an API key.
	other bool
	// active is the entry that signs in as the login, or "": the only one
	// that may hold a copy of it, as no two entries sign in as one account.
	active string
}

// live reads the box's login and saves it to the entry that signs in as it,
// so the entry holds the latest refresh token once Codex has rotated it.
func (v CodexVault) live(ctx context.Context, s *codexState) (*codexLive, error) {
	read, err := v.Store.ReadLive(ctx)
	if errors.Is(err, codexauth.ErrNotChatGPT) {
		return &codexLive{other: true}, nil
	}
	if err != nil {
		return nil, err
	}
	l := &codexLive{read: read}
	if read == nil {
		return l, nil
	}
	l.active = codexNamed(s, read.Identity())
	if l.active == "" {
		return l, nil
	}
	same, err := sameJSON(s.entries[l.active], *read)
	if err != nil || same {
		return l, err
	}
	if err := v.writeEntry(s, l.active, *read); err != nil {
		return nil, fmt.Errorf("saving the live login to account %s: %w", l.active, err)
	}
	return l, nil
}

var errCodexNotChatGPT = errors.New("the box's Codex is not signed in with ChatGPT, and a switch would drop its login; sign it out with `codex logout` first")

// Usage lists every account with its plan usage, the live login a row even
// when no entry names it. Only inactive accounts are refreshed first.
func (v CodexVault) Usage(ctx context.Context) (Listing, error) {
	var rows []pendingRow[codexauth.Login]
	err := v.locked(func(s *codexState) error {
		l, err := v.live(ctx, s)
		if err != nil {
			return err
		}
		now := v.Store.Now()
		liveRow := func(r Row, login codexauth.Login) pendingRow[codexauth.Login] {
			if login.AccessExpiresBy(now) {
				r.Error = "not refreshed: Codex refreshes it on its next request"
				return pendingRow[codexauth.Login]{row: r}
			}
			return pendingRow[codexauth.Login]{row: r, login: &login}
		}
		switch {
		case l.other:
			rows = append(rows, pendingRow[codexauth.Login]{row: Row{Active: true, Error: errCodexNotChatGPT.Error()}})
		case l.read != nil && l.active == "":
			rows = append(rows, liveRow(Row{Active: true, Email: l.read.Email()}, *l.read))
		}
		for _, name := range s.names() {
			e := s.entries[name]
			r := Row{Name: name, Email: e.Email(), Active: name == l.active}
			if name == l.active {
				rows = append(rows, liveRow(r, e))
				continue
			}
			login, err := v.fresh(ctx, s, name)
			if err != nil {
				r.Error = err.Error()
				rows = append(rows, pendingRow[codexauth.Login]{row: r})
				continue
			}
			rows = append(rows, pendingRow[codexauth.Login]{row: r, login: &login})
		}
		return nil
	})
	if err != nil {
		return Listing{}, err
	}
	return readUsage(rows, func(login codexauth.Login) (*Window, error) {
		u, err := v.Store.Usage(ctx, login)
		if err != nil || u.Weekly == nil {
			return nil, err
		}
		return &Window{Utilization: u.Weekly.Utilization, ResetsAt: &u.Weekly.ResetsAt}, nil
	}), nil
}

// Use makes the named account the box's Codex login, keeping the live login
// in its own entry first. A running Codex keeps the login it read, and
// refreshes it without saving the new tokens anywhere the vault sees, so Use
// refuses while any Codex runs: once Codex runs nowhere, no copy of a login
// is in use but the live one. It also refuses to drop a login it cannot keep.
func (v CodexVault) Use(ctx context.Context, name string) (switched bool, err error) {
	if err := ValidName(name); err != nil {
		return false, err
	}
	err = v.locked(func(s *codexState) error {
		if _, ok := s.entries[name]; !ok {
			return fmt.Errorf("no account %s", name)
		}
		l, err := v.live(ctx, s)
		if err != nil {
			return err
		}
		switch {
		case l.other:
			return errCodexNotChatGPT
		case l.read == nil:
		case l.active == name:
			return nil
		case l.active == "":
			return fmt.Errorf("the box's Codex is signed in to %s; keep that login with `rcodex account save <name>` first", l.read.Email())
		}
		running, err := v.Running()
		if err != nil {
			return fmt.Errorf("listing the running Codex processes: %w", err)
		}
		if len(running.PIDs) > 0 {
			return codexRunningError(running)
		}
		login, err := v.fresh(ctx, s, name)
		if err != nil {
			return err
		}
		return v.Store.WithLive(ctx, l.read, func(tx codexauth.LiveTx) error {
			if err := tx.Replace(login); err != nil {
				return err
			}
			switched = true
			return nil
		})
	})
	return switched, err
}

// codexRunningError says which Codex processes keep a switch from happening,
// and how to move the box's sessions among them over.
func codexRunningError(r CodexRunning) error {
	pids := make([]string, len(r.PIDs))
	for i, pid := range r.PIDs {
		pids[i] = strconv.Itoa(pid)
	}
	msg := fmt.Sprintf("Codex runs on the box (pid %s) with the login a switch would replace, which it would go on using and refreshing where the box cannot keep it; exit every Codex, switch, then start them again", strings.Join(pids, ", "))
	if len(r.Sessions) > 0 {
		msg += "; resume each of the box's sessions with rcodex resume <id>: " + strings.Join(r.Sessions, ", ")
	}
	return errors.New(msg)
}

// Save keeps the live login, which no entry names, under name.
func (v CodexVault) Save(ctx context.Context, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return v.locked(func(s *codexState) error {
		l, err := v.live(ctx, s)
		if err != nil {
			return err
		}
		switch {
		case l.other:
			return errCodexNotChatGPT
		case l.read == nil:
			return errors.New("the box's Codex has no login to save; log in with `rcodex account add <name>`")
		case l.active == name:
			return nil
		case l.active != "":
			return fmt.Errorf("the box's login is saved as %s already", l.active)
		}
		if _, taken := s.entries[name]; taken {
			return fmt.Errorf("account %s is another login", name)
		}
		return v.writeEntry(s, name, *l.read)
	})
}

// Rename gives an account another name.
func (v CodexVault) Rename(_ context.Context, old, new string) error {
	return v.locked(func(s *codexState) error { return renameEntry(v.files(), s, old, new) })
}

// Remove forgets an account that is not the live one.
func (v CodexVault) Remove(ctx context.Context, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	return v.locked(func(s *codexState) error {
		if _, ok := s.entries[name]; !ok {
			return fmt.Errorf("no account %s", name)
		}
		l, err := v.live(ctx, s)
		if err != nil {
			return err
		}
		if name == l.active {
			return fmt.Errorf("account %s is the box's login; switch to another one first", name)
		}
		return removeEntry(v.files(), s, name)
	})
}

// Add saves under name the login that signIn makes in an empty Codex home.
// Naming an existing account, it replaces that account's login, which must
// sign in as the same account. When the box is logged in to the account
// already, its login is kept instead and the new one goes unused.
func (v CodexVault) Add(ctx context.Context, name string, signIn func(home string) error) (added Added, err error) {
	if err := ValidName(name); err != nil {
		return 0, err
	}
	var made codexauth.Login
	err = v.files().scratch(func(dir string) error {
		if err := signIn(dir); err != nil {
			return err
		}
		store := v.Store
		store.Paths = codexauth.Paths{Home: dir}
		login, err := store.ReadLive(ctx)
		if errors.Is(err, codexauth.ErrNotChatGPT) {
			return errors.New("the sign-in left Codex not signed in with ChatGPT")
		}
		if err != nil {
			return err
		}
		if login == nil {
			return errors.New("the sign-in left no Codex login behind")
		}
		made = *login
		return nil
	})
	if err != nil {
		return 0, err
	}
	id := made.Identity()
	err = v.locked(func(s *codexState) error {
		if err := v.files().removeStaleScratch(); err != nil {
			return err
		}
		l, err := v.live(ctx, s)
		if err != nil {
			return err
		}
		if other := codexNamed(s, id); other != "" && other != name {
			return fmt.Errorf("that login is account %s already", other)
		}
		e, ok := s.entries[name]
		if ok && e.Identity() != id {
			return fmt.Errorf("account %s is %s, not %s", name, e.Email(), made.Email())
		}
		switch {
		case l.read != nil && l.read.Identity() == id && l.active == name:
			added = AddedAlready
			return nil
		case l.read != nil && l.read.Identity() == id:
			added = AddedLive
			return v.writeEntry(s, name, *l.read)
		}
		added = AddedNew
		return v.writeEntry(s, name, made)
	})
	return added, err
}

// KeepFresh refreshes every inactive saved login whose access token expires
// within codexRefreshAhead, so an account nobody uses stays signed in. It
// never touches the live login, nor its entry, and reports each
// account it could not refresh.
func (v CodexVault) KeepFresh(ctx context.Context, log io.Writer) error {
	var failed []error
	err := v.locked(func(s *codexState) error {
		if len(s.entries) == 0 {
			return nil
		}
		l, err := v.live(ctx, s)
		if err != nil {
			return err
		}
		due := v.Store.Now().Add(codexRefreshAhead)
		for _, name := range s.names() {
			if name == l.active || !s.entries[name].AccessExpiresBy(due) {
				continue
			}
			_, err := v.Store.Refresh(ctx, s.entries[name], codexauth.ReasonScheduled, func(next codexauth.Login) error {
				return v.writeEntry(s, name, next)
			})
			if err != nil {
				failed = append(failed, fmt.Errorf("account %s: %w", name, codexSignedOut(name, err)))
				continue
			}
			fmt.Fprintf(log, "refreshed account %s\n", name)
		}
		return nil
	})
	return errors.Join(err, errors.Join(failed...))
}

// fresh is an inactive entry's login with a usable access token, refreshed
// and saved first if needed; the caller holds the vault's lock and has made
// sure the entry is not the live login's.
func (v CodexVault) fresh(ctx context.Context, s *codexState, name string) (codexauth.Login, error) {
	login, err := v.Store.Fresh(ctx, s.entries[name], func(next codexauth.Login) error {
		return v.writeEntry(s, name, next)
	})
	return login, codexSignedOut(name, err)
}

// codexSignedOut names the way back for an account the server signed out.
func codexSignedOut(name string, err error) error {
	if errors.Is(err, codexauth.ErrRefreshRefused) {
		return fmt.Errorf("signed out: log in again with `rcodex account add %s`", name)
	}
	return err
}
