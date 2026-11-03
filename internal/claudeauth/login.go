package claudeauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"remote-agents/internal/jsonobject"
)

// Login is one claudeAiOauth object of .credentials.json, kept whole so the
// fields this package does not read survive a rewrite. It never prints its tokens.
type Login struct {
	fields jsonobject.Object
	tokens tokens
}

// tokens is the part of a Login this package reads, as Claude Code writes it.
type tokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	// ExpiresAt and RefreshTokenExpiresAt are Unix milliseconds; the latter may be unknown.
	ExpiresAt             int64    `json:"expiresAt"`
	RefreshTokenExpiresAt *int64   `json:"refreshTokenExpiresAt"`
	Scopes                []string `json:"scopes"`
	ClientID              *string  `json:"clientId"`
	SubscriptionType      *string  `json:"subscriptionType"`
}

func parseLogin(raw []byte) (Login, error) {
	o, err := jsonobject.Parse(raw)
	if err != nil {
		return Login{}, fmt.Errorf("claudeAiOauth: %w", err)
	}
	for _, key := range []string{"accessToken", "refreshToken", "expiresAt"} {
		if v, ok := o.Get(key); !ok || string(v) == "null" {
			return Login{}, fmt.Errorf("claudeAiOauth lacks %s", key)
		}
	}
	var t tokens
	if err := json.Unmarshal(raw, &t); err != nil {
		return Login{}, fmt.Errorf("claudeAiOauth: %w", err)
	}
	return Login{fields: o, tokens: t}, nil
}

// SignedOut is a login Claude Code cleared after the server refused its refresh token.
func (l Login) SignedOut() bool { return l.tokens.RefreshToken == "" }

// SameTokens reports whether l and other carry the same token pair.
func (l Login) SameTokens(other Login) bool {
	return l.tokens.AccessToken == other.tokens.AccessToken && l.tokens.RefreshToken == other.tokens.RefreshToken
}

// AccessExpired reports whether the access token has expired, so the server no longer takes it.
func (l Login) AccessExpired(now time.Time) bool { return now.UnixMilli() >= l.tokens.ExpiresAt }

// NewerThan reports whether l was issued after other, judged by their access token expiries.
func (l Login) NewerThan(other Login) bool { return l.tokens.ExpiresAt > other.tokens.ExpiresAt }

// SameRefreshToken reports whether l and other hold the same refresh token,
// so they are one login that nobody refreshed since.
func (l Login) SameRefreshToken(other Login) bool {
	return l.tokens.RefreshToken == other.tokens.RefreshToken
}

// accessExpired is Claude Code's test: the access token expires within five minutes.
func (l Login) accessExpired(now time.Time) bool {
	return now.UnixMilli()+5*60*1000 >= l.tokens.ExpiresAt
}

// RefreshExpiresAt is when the refresh token expires, when the server said.
func (l Login) RefreshExpiresAt() (time.Time, bool) {
	if l.tokens.RefreshTokenExpiresAt == nil {
		return time.Time{}, false
	}
	return time.UnixMilli(*l.tokens.RefreshTokenExpiresAt), true
}

func (l Login) MarshalJSON() ([]byte, error) { return l.fields.Marshal("") }

func (l *Login) UnmarshalJSON(data []byte) error {
	parsed, err := parseLogin(data)
	if err != nil {
		return err
	}
	*l = parsed
	return nil
}

// String keeps tokens out of logs and error messages.
func (Login) String() string   { return "claudeauth.Login{redacted}" }
func (Login) GoString() string { return "claudeauth.Login{redacted}" }

// Account is one oauthAccount object of Claude Code's global config, kept whole.
type Account struct {
	fields jsonobject.Object
	view   accountView
}

type accountView struct {
	AccountUUID      string `json:"accountUuid"`
	OrganizationUUID string `json:"organizationUuid"`
	EmailAddress     string `json:"emailAddress"`
}

// Identity names what one login signs in as: a user in one organization.
type Identity struct {
	AccountUUID      string
	OrganizationUUID string
}

func parseAccount(raw []byte) (Account, error) {
	o, err := jsonobject.Parse(raw)
	if err != nil {
		return Account{}, fmt.Errorf("oauthAccount: %w", err)
	}
	var v accountView
	if err := json.Unmarshal(raw, &v); err != nil {
		return Account{}, fmt.Errorf("oauthAccount: %w", err)
	}
	if v.AccountUUID == "" || v.OrganizationUUID == "" {
		return Account{}, errors.New("oauthAccount lacks accountUuid or organizationUuid")
	}
	return Account{fields: o, view: v}, nil
}

func (a Account) Identity() Identity {
	return Identity{AccountUUID: a.view.AccountUUID, OrganizationUUID: a.view.OrganizationUUID}
}

func (a Account) Email() string { return a.view.EmailAddress }

func (a Account) MarshalJSON() ([]byte, error) { return a.fields.Marshal("") }

func (a *Account) UnmarshalJSON(data []byte) error {
	parsed, err := parseAccount(data)
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}
