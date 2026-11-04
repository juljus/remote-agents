package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"remote-agents/internal/atomicfile"
	"remote-agents/internal/claudeauth"
)

// ClaudeVault is the named Claude logins of one box install, in Dir.
type ClaudeVault struct {
	Dir   string
	Store claudeauth.Store
}

// claudeEntry is one saved account: its login and the oauthAccount Claude Code shows for it.
type claudeEntry struct {
	Login   claudeauth.Login   `json:"claudeAiOauth"`
	Account claudeauth.Account `json:"oauthAccount"`
}

// claudeState is a Claude vault's entries.
type claudeState = state[claudeEntry]

func (v ClaudeVault) files() files         { return files{dir: v.Dir} }
func (v ClaudeVault) switchedPath() string { return filepath.Join(v.Dir, ".live.json") }

// claudeNamed is the entry that signs in as id, or "".
func claudeNamed(s *claudeState, id claudeauth.Identity) string {
	for name, e := range s.entries {
		if e.Account.Identity() == id {
			return name
		}
	}
	return ""
}

// locked runs fn holding the vault's lock.
func (v ClaudeVault) locked(fn func(*claudeState) error) error {
	return locked(v.files(), parseClaudeEntry, fn)
}

// parseClaudeEntry reads an entry file, which must hold the login and its
// account record and nothing else.
func parseClaudeEntry(data []byte) (claudeEntry, error) {
	var raw struct {
		Login   *claudeauth.Login   `json:"claudeAiOauth"`
		Account *claudeauth.Account `json:"oauthAccount"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return claudeEntry{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return claudeEntry{}, errors.New("data after the JSON object")
	}
	if raw.Login == nil || raw.Account == nil {
		return claudeEntry{}, errors.New("lacks claudeAiOauth or oauthAccount")
	}
	return claudeEntry{Login: *raw.Login, Account: *raw.Account}, nil
}

func (v ClaudeVault) writeEntry(s *claudeState, name string, e claudeEntry) error {
	return writeEntry(v.files(), s, name, e)
}

// liveState is how much the vault knows about whose the live login is. It
// never refreshes the live login to find out: Claude Code refreshes it.
type liveState int

const (
	// liveNone: the box has no login the server could still honour.
	liveNone liveState = iota
	// liveVerified: the profile endpoint named the login's account.
	liveVerified
	// liveProven: unverified, but it holds an entry's refresh token, so it is
	// that entry's login and nobody refreshed it since.
	liveProven
	// liveGuessed: unverified; Claude Code's account record and the vault's
	// last switch agree on an entry.
	liveGuessed
	// liveUnknown: unverified, and nothing says whose it is.
	liveUnknown
)

// live is the box's login as one command read it.
type live struct {
	read  claudeauth.Live
	state liveState
	// id is whom the login signs in as, when liveVerified.
	id *claudeauth.Identity
	// active is the entry the login is, or is taken to be; "" when no entry is.
	active string
	// unverified says why the server could not name the login's account.
	unverified string
	// copies are the entries that hold the login's refresh token.
	copies []string
}

// possiblyLive reports whether name's saved login may be a copy of the
// live one, which Claude Code may have refreshed since: refreshing that copy
// would present a refresh token the server already rotated.
func (l *live) possiblyLive(name string) bool {
	switch l.state {
	case liveNone:
		return false
	case liveUnknown:
		return true
	}
	return name == l.active || slices.Contains(l.copies, name)
}

// email labels the live login, from an account record that matches it.
func (l *live) email() string {
	if l.read.Account == nil || l.id == nil || l.read.Account.Identity() != *l.id {
		return ""
	}
	return l.read.Account.Email()
}

// unnamed reports a verified live login no entry names.
func (l *live) unnamed() bool { return l.state == liveVerified && l.active == "" }

// live reads the box's login and works out whose it is. A verified login is
// saved to its entry, and Claude Code's account record is put back when it
// names someone else: a running session may write it from a stale profile
// read, and a crash may leave it behind a switch.
func (v ClaudeVault) live(ctx context.Context, s *claudeState) (*live, error) {
	read, err := v.Store.ReadLive(ctx)
	if err != nil {
		return nil, err
	}
	l := &live{read: read}
	if read.Login == nil || read.Login.SignedOut() {
		return l, nil
	}
	for _, name := range s.names() {
		if s.entries[name].Login.SameRefreshToken(*read.Login) {
			l.copies = append(l.copies, name)
		}
	}
	if read.Login.AccessExpired(v.Store.Now()) {
		l.unverified = "its access token expired"
	} else {
		id, err := v.Store.Identity(ctx, *read.Login)
		switch {
		case errors.Is(err, claudeauth.ErrTokenRefused):
			l.unverified = "the server refused its access token"
		case err != nil:
			return nil, fmt.Errorf("asking whom the box's login signs in as: %w", err)
		default:
			l.state, l.id, l.active = liveVerified, &id, claudeNamed(s, id)
			return l, v.saveLive(ctx, s, l)
		}
	}
	if len(l.copies) > 0 {
		l.state, l.active = liveProven, l.copies[0]
		return l, nil
	}
	l.state = liveUnknown
	if read.Account == nil {
		return l, nil
	}
	recorded := claudeNamed(s, read.Account.Identity())
	last, err := v.lastSwitched()
	if err != nil {
		return nil, err
	}
	if recorded != "" && recorded == last {
		l.state, l.active = liveGuessed, recorded
	}
	return l, nil
}

// saveLive copies a verified live login into the entry it signs in as, so
// the entry holds the latest refresh token once Claude Code has rotated it,
// and puts that entry's account record back when the global config names another.
func (v ClaudeVault) saveLive(ctx context.Context, s *claudeState, l *live) error {
	if l.active == "" {
		return v.markSwitched("")
	}
	saved := s.entries[l.active]
	account := saved.Account
	if l.read.Account != nil && l.read.Account.Identity() == *l.id {
		account = *l.read.Account
	} else if err := v.Store.WithLive(ctx, l.read, func(tx claudeauth.LiveTx) error { return tx.ReplaceAccount(saved.Account) }); err != nil {
		return fmt.Errorf("restoring the box's account record: %w", err)
	}
	e := claudeEntry{Login: *l.read.Login, Account: account}
	same, err := sameJSON(saved, e)
	if err != nil {
		return err
	}
	if !same {
		if err := v.writeEntry(s, l.active, e); err != nil {
			return fmt.Errorf("saving the live login to account %s: %w", l.active, err)
		}
	}
	return v.markSwitched(l.active)
}

// lastSwitched is the entry the vault last saw live, or "".
func (v ClaudeVault) lastSwitched() (string, error) {
	data, err := os.ReadFile(v.switchedPath())
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var m switched
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("%s: %w", v.switchedPath(), err)
	}
	return m.Name, nil
}

func (v ClaudeVault) markSwitched(name string) error {
	last, err := v.lastSwitched()
	if err != nil || last == name {
		return err
	}
	data, err := json.Marshal(switched{Name: name})
	if err != nil {
		return err
	}
	return atomicfile.Write(v.switchedPath(), data, entryMode)
}

// switched records the entry the vault last saw live.
type switched struct {
	Name string `json:"name"`
}

// fresh is an inactive entry's login with a usable access token, refreshed
// and saved first if needed; the caller holds the vault's lock and has made
// sure the entry is not possibly live.
func (v ClaudeVault) fresh(ctx context.Context, s *claudeState, name string) (claudeauth.Login, error) {
	e := s.entries[name]
	login, err := v.Store.Fresh(ctx, e.Login, func(next claudeauth.Login) error {
		return v.writeEntry(s, name, claudeEntry{Login: next, Account: e.Account})
	})
	return login, signedOut(name, err)
}

// signedOut names the way back for an account the server signed out.
func signedOut(name string, err error) error {
	if errors.Is(err, claudeauth.ErrRefreshRefused) {
		return fmt.Errorf("signed out: add it again: press a in `rclaude account`, or run `rclaude account add %s`", name)
	}
	return err
}
