// Package accounts keeps several Claude logins on the box under names and
// switches the box between them. The live login stays where Claude Code
// keeps it; the vault holds every other account's login, and a copy of the
// live one that each command brings up to date.
package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"remote-agents/internal/atomicfile"
	"remote-agents/internal/claudeauth"
)

// Names start with a letter, so a picker can tell them from row numbers.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidName keeps names usable as file names and in remote command lines.
func ValidName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid account name %q: want a lowercase letter, then up to 31 lowercase letters, digits and dashes", name)
	}
	return nil
}

// Vault is the named Claude logins of one box install, in Dir.
type Vault struct {
	Dir   string
	Store claudeauth.Store
}

// entry is one saved account: its login and the oauthAccount Claude Code shows for it.
type entry struct {
	Login   claudeauth.Login   `json:"claudeAiOauth"`
	Account claudeauth.Account `json:"oauthAccount"`
}

const entryMode = 0o600

func (v Vault) entryPath(name string) string { return filepath.Join(v.Dir, name+".json") }
func (v Vault) lockPath() string             { return filepath.Join(v.Dir, ".lock") }

// ensureDir creates the vault dir, or narrows an existing one, to mode 0700.
func (v Vault) ensureDir() error {
	if err := os.MkdirAll(v.Dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(v.Dir, 0o700)
}

// state is the vault's entries, read under its lock.
type state struct {
	entries map[string]entry
}

func (s *state) names() []string {
	names := make([]string, 0, len(s.entries))
	for n := range s.entries {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func (s *state) named(id claudeauth.Identity) string {
	for name, e := range s.entries {
		if e.Account.Identity() == id {
			return name
		}
	}
	return ""
}

// locked runs fn holding the vault's lock, which orders every use of an
// entry's refresh token.
func (v Vault) locked(fn func(*state) error) error {
	if err := v.ensureDir(); err != nil {
		return err
	}
	f, err := os.OpenFile(v.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking the account vault: %w", err)
	}
	s, err := v.read()
	if err != nil {
		return err
	}
	return fn(s)
}

func (v Vault) read() (*state, error) {
	s := &state{entries: map[string]entry{}}
	dirents, err := os.ReadDir(v.Dir)
	if err != nil {
		return nil, err
	}
	for _, d := range dirents {
		name, ok := strings.CutSuffix(d.Name(), ".json")
		// The lock and temporary files have names no account can have.
		if !ok || ValidName(name) != nil {
			continue
		}
		data, err := os.ReadFile(v.entryPath(name))
		if err != nil {
			return nil, err
		}
		var e entry
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("account %s: %w", name, err)
		}
		s.entries[name] = e
	}
	return s, nil
}

func (v Vault) writeEntry(s *state, name string, e entry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(v.entryPath(name), data, entryMode); err != nil {
		return err
	}
	s.entries[name] = e
	return nil
}

// live is the box's login, resolved under Claude Code's refresh lock.
type live struct {
	tx claudeauth.LiveTx
	// login is nil when the box has no login the server honours.
	login *claudeauth.Login
	// id is whom login signs in as, by the server's word.
	id *claudeauth.Identity
	// account is the global config's oauthAccount, which may name someone else.
	account *claudeauth.Account
	// active is the entry id names, or "".
	active string
}

// email labels the live login, from an account record that matches it.
func (l *live) email() string {
	if l.account == nil || l.id == nil || l.account.Identity() != *l.id {
		return ""
	}
	return l.account.Email()
}

// unnamed reports a live login no entry names.
func (l *live) unnamed() bool { return l.login != nil && l.active == "" }

// withLive runs fn holding Claude Code's refresh lock, with the live login
// resolved and its entry brought up to date. The server says whom the login
// signs in as: oauthAccount is Claude Code's record of it, which a running
// session may rewrite from a stale profile read and a crash may leave behind.
func (v Vault) withLive(ctx context.Context, s *state, fn func(*live) error) error {
	return v.Store.WithLive(ctx, func(tx claudeauth.LiveTx) error {
		read, err := tx.Read()
		if err != nil {
			return err
		}
		l := &live{tx: tx, account: read.Account}
		if read.Login != nil && !read.Login.SignedOut() {
			login, err := tx.Fresh(*read.Login)
			switch {
			case errors.Is(err, claudeauth.ErrRefreshRefused):
			case err != nil:
				return fmt.Errorf("refreshing the box's login: %w", err)
			default:
				id, err := v.Store.Identity(ctx, login)
				if err != nil {
					return fmt.Errorf("asking whom the box's login signs in as: %w", err)
				}
				l.login, l.id, l.active = &login, &id, s.named(id)
			}
		}
		if err := v.saveLive(s, l); err != nil {
			return err
		}
		return fn(l)
	})
}

// saveLive copies the live login into the entry it signs in as, so the entry
// holds the latest refresh token once Claude Code has rotated it, and puts
// that entry's account record back when the global config names another.
func (v Vault) saveLive(s *state, l *live) error {
	if l.active == "" {
		return nil
	}
	saved := s.entries[l.active]
	if l.account == nil || l.account.Identity() != *l.id {
		if err := l.tx.ReplaceAccount(saved.Account); err != nil {
			return fmt.Errorf("restoring the box's account record: %w", err)
		}
		l.account = &saved.Account
	}
	e := entry{Login: *l.login, Account: *l.account}
	same, err := sameEntry(saved, e)
	if err != nil || same {
		return err
	}
	if err := v.writeEntry(s, l.active, e); err != nil {
		return fmt.Errorf("saving the live login to account %s: %w", l.active, err)
	}
	return nil
}

func sameEntry(a, b entry) (bool, error) {
	x, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	y, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return string(x) == string(y), nil
}

// fresh is an inactive entry's login with a usable access token, refreshed
// and saved first if needed; the caller holds the vault's lock.
func (v Vault) fresh(ctx context.Context, s *state, name string) (claudeauth.Login, error) {
	e := s.entries[name]
	login, err := v.Store.Fresh(ctx, e.Login, func(next claudeauth.Login) error {
		return v.writeEntry(s, name, entry{Login: next, Account: e.Account})
	})
	return login, signedOut(name, err)
}

// signedOut names the way back for an account the server signed out.
func signedOut(name string, err error) error {
	if errors.Is(err, claudeauth.ErrRefreshRefused) {
		return fmt.Errorf("signed out: log in again with `rclaude account add %s`", name)
	}
	return err
}
