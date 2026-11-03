package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"remote-agents/internal/jsonobject"
)

// Endpoints are the OpenAI services a login talks to.
type Endpoints struct {
	// TokenOverride, when set, takes every refresh, as Codex's
	// CODEX_REFRESH_TOKEN_URL_OVERRIDE does; otherwise a login refreshes
	// with the authority that issued it, or Codex's default one.
	TokenOverride string
	// APIBase is the ChatGPT backend, which serves /wham/usage.
	APIBase string
}

// DefaultEndpoints and ClientID are Codex's own (its REFRESH_TOKEN_URL,
// chatgpt_base_url and CLIENT_ID).
var DefaultEndpoints = Endpoints{APIBase: "https://chatgpt.com/backend-api"}

const (
	defaultTokenURL = "https://auth.openai.com/oauth/token"
	ClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// Codex refreshes a login whose access token expires within five minutes.
const accessRefreshWindow = 5 * time.Minute

// Timeouts for a token refresh and a usage read.
const (
	refreshTimeout = 30 * time.Second
	readTimeout    = 10 * time.Second
)

// Reason is why a login refreshes, which Codex reports to the server.
type Reason string

const (
	ReasonProactive Reason = "proactive_expiry"
	ReasonScheduled Reason = "scheduled"
)

// ErrTokenRefused means the server no longer takes an access token.
var ErrTokenRefused = errors.New("the server refused the access token")

// ErrRefreshRefused means the server refused the refresh token: the login
// is gone and only a new sign-in brings the account back.
var ErrRefreshRefused = errors.New("the server refused the refresh token; sign in to this account again")

// Fresh is l with an access token Codex would not refresh yet, refreshed and
// saved first if needed. l must be a saved login no Codex runs on.
func (s Store) Fresh(ctx context.Context, l Login, save func(Login) error) (Login, error) {
	if !l.AccessExpiresBy(s.Now().Add(accessRefreshWindow)) {
		return l, nil
	}
	return s.Refresh(ctx, l, ReasonProactive, save)
}

// Refresh trades l's refresh token for new tokens, as Codex does, and has
// save keep them before it returns: the server has already rotated the old
// refresh token. The request runs to its end even if ctx is cancelled.
func (s Store) Refresh(ctx context.Context, l Login, reason Reason, save func(Login) error) (Login, error) {
	endpoint, clientID := defaultTokenURL, ClientID
	if p := l.tokens.Provenance; p != nil {
		endpoint, clientID = strings.TrimSuffix(p.Issuer, "/")+"/oauth/token", p.ClientID
	}
	if s.Endpoints.TokenOverride != "" {
		endpoint = s.Endpoints.TokenOverride
	}
	body, err := json.Marshal(map[string]string{
		"client_id": clientID, "grant_type": "refresh_token", "refresh_token": l.tokens.RefreshToken, "refresh_reason": string(reason),
	})
	if err != nil {
		return Login{}, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Login{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return Login{}, fmt.Errorf("refreshing: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Login{}, fmt.Errorf("refreshing: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Login{}, refreshError(resp.StatusCode, data)
	}
	var reply struct {
		IDToken      *string `json:"id_token"`
		AccessToken  *string `json:"access_token"`
		RefreshToken *string `json:"refresh_token"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return Login{}, errors.New("token endpoint reply is not JSON")
	}
	next, err := l.refreshed(reply.IDToken, reply.AccessToken, reply.RefreshToken, endpoint, s.Now())
	if err != nil {
		return Login{}, fmt.Errorf("token endpoint reply: %w", err)
	}
	if err := save(next); err != nil {
		return Login{}, fmt.Errorf("saving the refreshed login: %w", err)
	}
	return next, nil
}

// refreshed is l with the tokens a refresh returned, as Codex persists them:
// the authority that issued the login stays only when it took the refresh.
func (l Login) refreshed(idToken, accessToken, refreshToken *string, endpoint string, now time.Time) (Login, error) {
	raw, ok := l.fields.Get("tokens")
	if !ok {
		return Login{}, errors.New("the login lost its tokens")
	}
	t, err := jsonobject.Parse(raw)
	if err != nil {
		return Login{}, err
	}
	for _, set := range []struct {
		key   string
		value *string
	}{{"id_token", idToken}, {"access_token", accessToken}, {"refresh_token", refreshToken}} {
		if set.value == nil {
			continue
		}
		v, err := json.Marshal(*set.value)
		if err != nil {
			return Login{}, err
		}
		t = t.With(set.key, v)
	}
	if p := l.tokens.Provenance; p != nil && endpoint != strings.TrimSuffix(p.Issuer, "/")+"/oauth/token" {
		t = t.Without("oauth_provenance")
	}
	tokens, err := t.Marshal("")
	if err != nil {
		return Login{}, err
	}
	stamp, err := json.Marshal(now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return Login{}, err
	}
	data, err := l.fields.With("tokens", tokens).With("last_refresh", stamp).Marshal("")
	if err != nil {
		return Login{}, err
	}
	return ParseLogin(data)
}

// refreshError says why the token endpoint refused, classified as Codex
// classifies it; the body is never quoted, as it may echo the refresh token.
func refreshError(status int, body []byte) error {
	code := errorCode(body)
	switch {
	case status == http.StatusUnauthorized,
		status == http.StatusBadRequest && strings.EqualFold(code, "invalid_grant"),
		slices.Contains([]string{"refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated"}, strings.ToLower(code)):
		return ErrRefreshRefused
	case plainCode.MatchString(code):
		return fmt.Errorf("token endpoint answered %d (%s)", status, code)
	}
	return fmt.Errorf("token endpoint answered %d", status)
}

// plainCode is an error code safe to show: a word, which no token is.
var plainCode = regexp.MustCompile(`^[a-z_]{1,40}$`)

// errorCode is the error code of a token endpoint reply: error, error.code
// or code, as Codex reads it; "" when there is none.
func errorCode(body []byte) string {
	var reply map[string]json.RawMessage
	if json.Unmarshal(body, &reply) != nil {
		return ""
	}
	var code string
	if json.Unmarshal(reply["error"], &code) == nil && code != "" {
		return code
	}
	var nested struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(reply["error"], &nested) == nil && nested.Code != "" {
		return nested.Code
	}
	if json.Unmarshal(reply["code"], &code) == nil {
		return code
	}
	return ""
}

// Window is one usage limit window.
type Window struct {
	// Utilization is the percentage of the window's limit used.
	Utilization float64
	ResetsAt    time.Time
}

// Usage is a login's weekly usage; Weekly is nil for a plan without a
// weekly limit. A plan's 5-hour window, which some have, goes unread.
type Usage struct {
	Weekly *Window
}

// The window lengths the usage endpoint reports, in seconds.
const (
	fiveHourWindow = 5 * 60 * 60
	sevenDayWindow = 7 * 24 * 60 * 60
)

// Usage reads /wham/usage, the endpoint Codex's /status reads. OpenAI does
// not document it: a reply without the fields read here is an error.
func (s Store) Usage(ctx context.Context, l Login) (Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(s.Endpoints.APIBase, "/")+"/wham/usage", nil)
	if err != nil {
		return Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+l.tokens.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", l.identity.AccountID)
	req.Header.Set("User-Agent", "codex-cli")
	resp, err := s.Client.Do(req)
	if err != nil {
		return Usage{}, fmt.Errorf("reading the usage: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Usage{}, fmt.Errorf("reading the usage: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return Usage{}, fmt.Errorf("usage endpoint answered 401: %w", ErrTokenRefused)
	}
	if resp.StatusCode != http.StatusOK {
		return Usage{}, fmt.Errorf("usage endpoint answered %d", resp.StatusCode)
	}
	return parseUsage(data)
}

type usageWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAt       *int64   `json:"reset_at"`
}

func parseUsage(data []byte) (Usage, error) {
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(data, &reply); err != nil {
		return Usage{}, fmt.Errorf("usage reply: %w", err)
	}
	raw, ok := reply["rate_limit"]
	if !ok {
		return Usage{}, errors.New("usage reply has no rate_limit")
	}
	var limit *struct {
		Primary   *usageWindow `json:"primary_window"`
		Secondary *usageWindow `json:"secondary_window"`
	}
	if err := json.Unmarshal(raw, &limit); err != nil {
		return Usage{}, fmt.Errorf("usage reply: %w", err)
	}
	var u Usage
	if limit == nil {
		return u, nil
	}
	for _, w := range []*usageWindow{limit.Primary, limit.Secondary} {
		if w == nil {
			continue
		}
		if w.UsedPercent == nil || w.WindowSeconds == nil || w.ResetAt == nil {
			return Usage{}, errors.New("usage reply has a window without used_percent, limit_window_seconds or reset_at")
		}
		switch *w.WindowSeconds {
		case fiveHourWindow:
			continue
		case sevenDayWindow:
		default:
			return Usage{}, fmt.Errorf("usage reply has a %d-second window", *w.WindowSeconds)
		}
		if u.Weekly != nil {
			return Usage{}, fmt.Errorf("usage reply has two %d-second windows", *w.WindowSeconds)
		}
		u.Weekly = &Window{Utilization: *w.UsedPercent, ResetsAt: time.Unix(*w.ResetAt, 0)}
	}
	return u, nil
}
