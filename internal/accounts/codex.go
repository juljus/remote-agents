package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"remote-agents/internal/atomicfile"
	"remote-agents/internal/codexauth"
)

// CodexVault is the named Codex logins of one box install, in Dir. A switch
// reaches only the Codex processes that start after it: a running Codex
// keeps the login it read until it is restarted.
type CodexVault struct {
	Dir   string
	Store codexauth.Store
	// Running lists the threads that running Codex processes hold.
	Running func() ([]string, error)
}

// codexRefreshAhead is how long before its access token expires KeepFresh
// refreshes an idle login: the timer runs daily, so each login rotates as
// often as a Codex that used it would rotate it.
const codexRefreshAhead = 24 * time.Hour

// codexBusyWindow is how close to its expiry the live access token may be
// for a switch. A Codex refreshes outside its lock and saves the new tokens
// only if auth.json still holds its login, so a switch that lands between
// the two loses them, and the saved copy holds a spent refresh token. A
// Codex refreshes when its access token is five minutes from expiry, which
// this window keeps switches away from; one that refreshes on a refused
// request, which a valid token rarely meets, can still race a switch.
const codexBusyWindow = 15 * time.Minute

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
	// kept are the running threads that started on each account the box
	// switched away from.
	kept codexKept
}

// live reads the box's login and saves it to the entry that signs in as it,
// so the entry holds the latest refresh token once Codex has rotated it.
func (v CodexVault) live(ctx context.Context, s *codexState) (*codexLive, error) {
	kept, err := v.stillKept()
	if err != nil {
		return nil, err
	}
	read, err := v.Store.ReadLive(ctx)
	if errors.Is(err, codexauth.ErrNotChatGPT) {
		return &codexLive{other: true, kept: kept}, nil
	}
	if err != nil {
		return nil, err
	}
	l := &codexLive{read: read, kept: kept}
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
			if threads := l.kept.threads(e.Identity()); len(threads) > 0 {
				if e.AccessExpiresBy(now) {
					r.Error = keptReason(threads)
					rows = append(rows, pendingRow[codexauth.Login]{row: r})
					continue
				}
				rows = append(rows, pendingRow[codexauth.Login]{row: r, login: &e})
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

// Use makes the named account the box's Codex login for every Codex that
// starts after it, keeping the live login in its own entry first. It
// refuses to drop a login it cannot keep, and to switch while a Codex may
// be refreshing the live login.
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
		case l.read.AccessExpiresBy(v.Store.Now()):
			return errors.New("the box's Codex login expired, and the next Codex request refreshes it, which a switch now could lose; send a Codex session a message, then switch, or, if that account was signed out, run `codex logout` on the box first")
		case l.read.AccessExpiresBy(v.Store.Now().Add(codexBusyWindow)):
			return errors.New("the box's Codex is about to refresh its login, which a switch now could lose; try again in a quarter of an hour")
		}
		if threads := l.kept.threads(s.entries[name].Identity()); len(threads) > 0 {
			return fmt.Errorf("%s with its login, and may have refreshed it; switching back would hand them the saved copy; exit them first: %s", keptSessions(threads, name), strings.Join(threads, ", "))
		}
		login, err := v.fresh(ctx, s, name)
		if err != nil {
			return err
		}
		if l.read != nil {
			if err := v.keep(l, l.read.Identity()); err != nil {
				return err
			}
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
// never touches the live login, nor its entry, nor a login running Codex
// processes still use, and reports each account it could not refresh.
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
			if threads := l.kept.threads(s.entries[name].Identity()); len(threads) > 0 {
				fmt.Fprintf(log, "account %s: not refreshed while %s\n", name, keptSessions(threads, ""))
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

// codexKeptFile records, for each account the box switched away from, the
// threads that were running then. Each of those Codex processes keeps the
// account's login in memory and refreshes it there, without saving the new
// tokens anywhere the vault sees: while one runs, the vault must not
// refresh that login, nor hand those processes its saved copy back.
const codexKeptFile = ".kept.json"

type codexKept []keptAccount

type keptAccount struct {
	AccountID string   `json:"account_id"`
	UserID    string   `json:"user_id"`
	Threads   []string `json:"threads"`
}

func (k codexKept) threads(id codexauth.Identity) []string {
	for _, a := range k {
		if a.AccountID == id.AccountID && a.UserID == id.UserID {
			return a.Threads
		}
	}
	return nil
}

func (v CodexVault) keptPath() string { return filepath.Join(v.Dir, codexKeptFile) }

// stillKept reads the record and drops from it the threads that have
// ended; the caller holds the vault's lock.
func (v CodexVault) stillKept() (codexKept, error) {
	data, err := os.ReadFile(v.keptPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recorded codexKept
	if err := json.Unmarshal(data, &recorded); err != nil {
		return nil, fmt.Errorf("reading %s: %w", v.keptPath(), err)
	}
	running, err := v.Running()
	if err != nil {
		return nil, fmt.Errorf("listing the running Codex sessions: %w", err)
	}
	var kept codexKept
	for _, a := range recorded {
		a.Threads = slices.DeleteFunc(a.Threads, func(id string) bool { return !slices.Contains(running, id) })
		if len(a.Threads) > 0 {
			kept = append(kept, a)
		}
	}
	return kept, v.writeKept(kept)
}

// keep records the running threads that no other account keeps as id's,
// before the box switches away from it.
func (v CodexVault) keep(l *codexLive, id codexauth.Identity) error {
	running, err := v.Running()
	if err != nil {
		return fmt.Errorf("listing the running Codex sessions, which keep the login a switch replaces: %w", err)
	}
	var others []string
	for _, a := range l.kept {
		if a.AccountID != id.AccountID || a.UserID != id.UserID {
			others = append(others, a.Threads...)
		}
	}
	threads := slices.DeleteFunc(slices.Clone(running), func(t string) bool { return slices.Contains(others, t) })
	if len(threads) == 0 {
		return nil
	}
	kept := slices.DeleteFunc(slices.Clone(l.kept), func(a keptAccount) bool { return a.AccountID == id.AccountID && a.UserID == id.UserID })
	kept = append(kept, keptAccount{AccountID: id.AccountID, UserID: id.UserID, Threads: threads})
	return v.writeKept(kept)
}

func (v CodexVault) writeKept(kept codexKept) error {
	if len(kept) == 0 {
		err := os.Remove(v.keptPath())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	data, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	return atomicfile.Write(v.keptPath(), data, entryMode)
}

// keptSessions says how many running sessions started on account name.
func keptSessions(threads []string, name string) string {
	on := "it"
	if name != "" {
		on = name
	}
	if len(threads) == 1 {
		return "1 Codex session that started on " + on + " runs"
	}
	return fmt.Sprintf("%d Codex sessions that started on %s run", len(threads), on)
}

func keptReason(threads []string) string {
	return "not refreshed while " + keptSessions(threads, "") + "; they use it until they exit"
}
