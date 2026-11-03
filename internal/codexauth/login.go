// Package codexauth reads, switches and refreshes the ChatGPT login Codex
// keeps in $CODEX_HOME/auth.json, the way Codex 0.166.0 does.
package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"remote-agents/internal/jsonobject"
)

// ErrNotChatGPT means auth.json holds another kind of login, such as an API
// key, which accounts never replace.
var ErrNotChatGPT = errors.New("auth.json holds no ChatGPT login")

// Login is one ChatGPT login, the whole of an auth.json, kept whole so a
// switch writes back every field Codex keeps.
type Login struct {
	fields    jsonobject.Object
	tokens    tokens
	identity  Identity
	email     string
	accessExp time.Time
}

type tokens struct {
	IDToken      string      `json:"id_token"`
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	AccountID    *string     `json:"account_id"`
	Provenance   *provenance `json:"oauth_provenance"`
}

// provenance is the OAuth authority that issued a login's refresh token.
type provenance struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
}

// Identity is whom a login signs in as: Codex tells two logins apart by the
// account, and an account's users by their user id.
type Identity struct {
	AccountID string
	UserID    string
}

func ParseLogin(data []byte) (Login, error) {
	o, err := jsonobject.Parse(data)
	if err != nil {
		return Login{}, err
	}
	var head struct {
		AuthMode *string         `json:"auth_mode"`
		APIKey   *string         `json:"OPENAI_API_KEY"`
		Tokens   json.RawMessage `json:"tokens"`
		// LastRefresh is read only to refuse one Codex could not read.
		LastRefresh *time.Time `json:"last_refresh"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return Login{}, err
	}
	if (head.AuthMode != nil && *head.AuthMode != "chatgpt") || head.APIKey != nil || len(head.Tokens) == 0 || string(head.Tokens) == "null" {
		return Login{}, ErrNotChatGPT
	}
	l := Login{fields: o}
	if err := l.setTokens(head.Tokens); err != nil {
		return Login{}, err
	}
	return l, nil
}

// setTokens reads raw as l's tokens and what their JWTs claim.
func (l *Login) setTokens(raw json.RawMessage) error {
	var t tokens
	if err := json.Unmarshal(raw, &t); err != nil {
		return fmt.Errorf("tokens: %w", err)
	}
	if t.AccessToken == "" || t.RefreshToken == "" || t.AccountID == nil || *t.AccountID == "" {
		return errors.New("tokens lack an access token, a refresh token or an account id")
	}
	var id struct {
		Email   string `json:"email"`
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
		Auth struct {
			ChatGPTUserID string `json:"chatgpt_user_id"`
			UserID        string `json:"user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := decodeClaims(t.IDToken, &id); err != nil {
		return fmt.Errorf("id token: %w", err)
	}
	var access struct {
		Exp *int64 `json:"exp"`
	}
	if err := decodeClaims(t.AccessToken, &access); err != nil {
		return fmt.Errorf("access token: %w", err)
	}
	if access.Exp == nil {
		return errors.New("access token names no expiry")
	}
	// Codex reads the user and the email the same way (login/src/token_data.rs).
	user := id.Auth.ChatGPTUserID
	if user == "" {
		user = id.Auth.UserID
	}
	if user == "" {
		return errors.New("id token names no user")
	}
	email := id.Email
	if email == "" {
		email = id.Profile.Email
	}
	l.tokens, l.identity, l.email, l.accessExp = t, Identity{AccountID: *t.AccountID, UserID: user}, email, time.Unix(*access.Exp, 0)
	return nil
}

// decodeClaims reads a JWT's claims without checking its signature: the
// token comes from the server that signed it, by way of Codex's own file.
func decodeClaims(token string, v any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("not a JWT")
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return errors.New("its claims are not JSON")
	}
	return nil
}

func (l Login) Identity() Identity { return l.identity }

// Email is the address the id token names, or "".
func (l Login) Email() string { return l.email }

// AccessExpiresBy reports whether the access token has expired by t.
func (l Login) AccessExpiresBy(t time.Time) bool { return !l.accessExp.After(t) }

func (l Login) MarshalJSON() ([]byte, error) { return l.fields.Marshal("") }

func (l *Login) UnmarshalJSON(data []byte) error {
	parsed, err := ParseLogin(data)
	if err != nil {
		return err
	}
	*l = parsed
	return nil
}

// String keeps tokens out of logs and error messages.
func (Login) String() string   { return "codexauth.Login{redacted}" }
func (Login) GoString() string { return "codexauth.Login{redacted}" }
