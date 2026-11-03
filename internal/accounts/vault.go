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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"remote-agents/internal/atomicfile"
	"remote-agents/internal/claudeauth"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidName keeps names usable as file names and in remote command lines.
func ValidName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid account name %q: want up to 32 lowercase letters, digits and dashes", name)
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

// journal records a switch in progress, so the next command can finish one
// a crash cut short. From is "" when the box had no login to keep.
type journal struct {
	From string `json:"from"`
	To   string `json:"to"`
}

const entryMode = 0o600

func (v Vault) entryPath(name string) string { return filepath.Join(v.Dir, name+".json") }
func (v Vault) journalPath() string          { return filepath.Join(v.Dir, "switching.json") }
func (v Vault) lockPath() string             { return filepath.Join(v.Dir, ".lock") }

// ensureDir creates the vault dir, or narrows an existing one, to mode 0700.
func (v Vault) ensureDir() error {
	if err := os.MkdirAll(v.Dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(v.Dir, 0o700)
}

// state is the vault and the live login, read under both locks.
type state struct {
	tx      claudeauth.LiveTx
	live    claudeauth.Live
	entries map[string]entry
	// active names the entry the live login signs in as, or is "".
	active string
}

// names lists the entries in order.
func (s *state) names() []string {
	names := make([]string, 0, len(s.entries))
	for n := range s.entries {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// unnamedLive reports a usable live login that no entry names.
func (s *state) unnamedLive() bool {
	return s.active == "" && s.live.Login != nil && !s.live.Login.SignedOut()
}

// locked runs fn holding the vault's lock and Claude Code's refresh lock,
// after finishing an interrupted switch and saving the live login to its entry.
func (v Vault) locked(ctx context.Context, fn func(*state) error) error {
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
	return v.Store.WithLive(ctx, func(tx claudeauth.LiveTx) error {
		if err := v.finishSwitch(tx); err != nil {
			return fmt.Errorf("finishing an interrupted switch: %w", err)
		}
		s, err := v.read(tx)
		if err != nil {
			return err
		}
		if err := v.saveLive(s); err != nil {
			return err
		}
		return fn(s)
	})
}

func (v Vault) read(tx claudeauth.LiveTx) (*state, error) {
	live, err := tx.Read()
	if err != nil {
		return nil, err
	}
	s := &state{tx: tx, live: live, entries: map[string]entry{}}
	dirents, err := os.ReadDir(v.Dir)
	if err != nil {
		return nil, err
	}
	for _, d := range dirents {
		name, ok := strings.CutSuffix(d.Name(), ".json")
		if !ok || d.Name() == filepath.Base(v.journalPath()) || ValidName(name) != nil {
			continue
		}
		e, err := v.readEntry(name)
		if err != nil {
			return nil, err
		}
		s.entries[name] = e
		if live.Account != nil && e.Account.Identity() == live.Account.Identity() {
			s.active = name
		}
	}
	return s, nil
}

func (v Vault) readEntry(name string) (entry, error) {
	data, err := os.ReadFile(v.entryPath(name))
	if err != nil {
		return entry{}, err
	}
	var e entry
	if err := json.Unmarshal(data, &e); err != nil {
		return entry{}, fmt.Errorf("account %s: %w", name, err)
	}
	return e, nil
}

func (v Vault) writeEntry(name string, e entry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return atomicfile.Write(v.entryPath(name), data, entryMode)
}

// saveLive copies the live login into the entry it signs in as, so the entry
// holds the latest refresh token once Claude Code has rotated it. A login
// Claude Code cleared never replaces a saved one.
func (v Vault) saveLive(s *state) error {
	if s.active == "" || s.live.Login == nil || s.live.Login.SignedOut() {
		return nil
	}
	saved := s.entries[s.active]
	if saved.Login.SameTokens(*s.live.Login) && sameJSON(saved.Account, *s.live.Account) {
		return nil
	}
	e := entry{Login: *s.live.Login, Account: *s.live.Account}
	if err := v.writeEntry(s.active, e); err != nil {
		return fmt.Errorf("saving the live login to account %s: %w", s.active, err)
	}
	s.entries[s.active] = e
	return nil
}

func sameJSON(a, b json.Marshaler) bool {
	x, errX := a.MarshalJSON()
	y, errY := b.MarshalJSON()
	return errX == nil && errY == nil && string(x) == string(y)
}

// finishSwitch completes a switch the journal records. The target's entry is
// never changed by a switch, so it still holds what the switch was writing.
func (v Vault) finishSwitch(tx claudeauth.LiveTx) error {
	data, err := os.ReadFile(v.journalPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		return fmt.Errorf("%s: %w", v.journalPath(), err)
	}
	to, err := v.readEntry(j.To)
	if err != nil {
		return err
	}
	live, err := tx.Read()
	if err != nil {
		return err
	}
	// The credentials still hold what the switch replaced, or already the
	// target's login, which Claude Code may have refreshed since.
	pending := live.Login == nil || live.Login.SignedOut()
	if j.From != "" && live.Login != nil {
		from, err := v.readEntry(j.From)
		if err != nil {
			return err
		}
		pending = live.Login.SameTokens(from.Login)
	}
	if pending {
		err = tx.Replace(to.Login, to.Account)
	} else {
		err = tx.ReplaceAccount(to.Account)
	}
	if err != nil {
		return err
	}
	return atomicfile.Remove(v.journalPath())
}
