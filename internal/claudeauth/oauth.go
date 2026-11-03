package claudeauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Endpoints are the Anthropic services a login talks to.
type Endpoints struct {
	// TokenURL is Claude Code's OAuth token endpoint.
	TokenURL string
	// APIBase serves /api/oauth/usage.
	APIBase string
}

// DefaultEndpoints and ClientID are Claude Code's own (its TOKEN_URL, BASE_API_URL and CLIENT_ID).
var DefaultEndpoints = Endpoints{TokenURL: "https://platform.claude.com/v1/oauth/token", APIBase: "https://api.anthropic.com"}

const ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// Claude Code's timeouts for a token refresh and a usage read.
const (
	refreshTimeout = 30 * time.Second
	usageTimeout   = 10 * time.Second
)

// oauthBeta is the anthropic-beta value Claude Code sends with OAuth tokens.
const oauthBeta = "oauth-2025-04-20"

// ErrRefreshRefused means the server refused the refresh token: the login
// is gone and only a new sign-in brings the account back.
var ErrRefreshRefused = errors.New("the server refused the refresh token; sign in to this account again")

type tokenReply struct {
	AccessToken           *string  `json:"access_token"`
	RefreshToken          *string  `json:"refresh_token"`
	ExpiresIn             *float64 `json:"expires_in"`
	RefreshTokenExpiresIn *float64 `json:"refresh_token_expires_in"`
	Scope                 *string  `json:"scope"`
}

// refresh trades l's refresh token for new tokens as Claude Code does. The
// server retires the old refresh token once it answers, so save receives
// every login the server issued before refresh returns, even an incomplete one.
func refresh(ctx context.Context, client *http.Client, ep Endpoints, l Login, now func() time.Time, save func(Login) error) (Login, error) {
	t := l.tokens
	if t.RefreshToken == "" {
		return Login{}, ErrRefreshRefused
	}
	clientID := ClientID
	if t.ClientID != nil {
		clientID = *t.ClientID
	}
	body, err := json.Marshal(map[string]string{
		"grant_type": "refresh_token", "refresh_token": t.RefreshToken,
		"client_id": clientID, "scope": strings.Join(t.Scopes, " "),
	})
	if err != nil {
		return Login{}, err
	}
	// Cancelled midway, a refresh could lose the token the server just rotated.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.TokenURL, bytes.NewReader(body))
	if err != nil {
		return Login{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Login{}, fmt.Errorf("refreshing the login: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Login{}, fmt.Errorf("refreshing the login: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Login{}, refreshError(resp.StatusCode, data)
	}
	issuedAt := now()
	var reply tokenReply
	if err := json.Unmarshal(data, &reply); err != nil {
		return Login{}, fmt.Errorf("token endpoint reply is not the JSON object it was: %w", err)
	}
	next, complete, err := l.refreshed(reply, issuedAt)
	if err != nil {
		return Login{}, err
	}
	if err := save(next); err != nil {
		return Login{}, fmt.Errorf("saving the refreshed login: %w", err)
	}
	if !complete {
		return Login{}, errors.New("token endpoint reply lacks access_token or expires_in; the rotated refresh token was saved")
	}
	return next, nil
}

// refreshed is l with reply's tokens, as Claude Code merges them: an absent
// refresh token or refresh expiry keeps the old one, an absent scope means
// the scopes asked for. complete is false when the reply had no usable access token.
func (l Login) refreshed(reply tokenReply, issuedAt time.Time) (next Login, complete bool, err error) {
	complete = reply.AccessToken != nil && *reply.AccessToken != "" && reply.ExpiresIn != nil && *reply.ExpiresIn > 0
	if !complete && (reply.RefreshToken == nil || *reply.RefreshToken == l.tokens.RefreshToken) {
		return Login{}, false, errors.New("token endpoint reply lacks access_token or expires_in")
	}
	type update struct {
		key   string
		value any
	}
	// Without an access token Claude Code refreshes the login before using it.
	updates := []update{{"accessToken", ""}, {"expiresAt", 0}}
	if complete {
		updates = []update{{"accessToken", *reply.AccessToken}, {"expiresAt", issuedAt.UnixMilli() + int64(*reply.ExpiresIn*1000)}}
	}
	if reply.RefreshToken != nil {
		updates = append(updates, update{"refreshToken", *reply.RefreshToken})
	}
	if reply.RefreshTokenExpiresIn != nil {
		updates = append(updates, update{"refreshTokenExpiresAt", issuedAt.UnixMilli() + int64(*reply.RefreshTokenExpiresIn*1000)})
	}
	if reply.Scope != nil {
		updates = append(updates, update{"scopes", strings.Fields(*reply.Scope)})
	}
	o := l.fields
	for _, u := range updates {
		raw, err := json.Marshal(u.value)
		if err != nil {
			return Login{}, false, err
		}
		o = o.with(u.key, raw)
	}
	raw, err := o.marshal("")
	if err != nil {
		return Login{}, false, err
	}
	next, err = parseLogin(raw)
	return next, complete, err
}

// refreshError names the OAuth error code, never the body, which may echo the request.
func refreshError(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error == "invalid_grant" {
		return ErrRefreshRefused
	}
	if e.Error != "" {
		return fmt.Errorf("token endpoint answered %d (%s)", status, e.Error)
	}
	return fmt.Errorf("token endpoint answered %d", status)
}

// Window is one usage limit window.
type Window struct {
	// Utilization is the percentage of the window's limit used.
	Utilization float64
	// ResetsAt is zero when the server names no reset.
	ResetsAt time.Time
}

// Usage is how much of its plan limits an account has used. A nil window is
// one the server reports as not running, as before the first request in it.
type Usage struct {
	FiveHour *Window
	SevenDay *Window
}

// fetchUsage reads /api/oauth/usage, an endpoint Anthropic does not document:
// a reply without the fields read here is an error, never a default.
func fetchUsage(ctx context.Context, client *http.Client, ep Endpoints, l Login) (Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(ep.APIBase, "/")+"/api/oauth/usage", nil)
	if err != nil {
		return Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+l.tokens.AccessToken)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Usage{}, fmt.Errorf("reading usage: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Usage{}, fmt.Errorf("reading usage: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Usage{}, fmt.Errorf("usage endpoint answered %d", resp.StatusCode)
	}
	return parseUsage(data)
}

type rawWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

func parseUsage(data []byte) (Usage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return Usage{}, fmt.Errorf("usage reply changed shape: %w", err)
	}
	window := func(key string) (*Window, error) {
		raw, ok := top[key]
		if !ok {
			return nil, fmt.Errorf("usage reply changed shape: no %s", key)
		}
		if string(raw) == "null" {
			return nil, nil
		}
		var w rawWindow
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, fmt.Errorf("usage reply changed shape: %s: %w", key, err)
		}
		if w.Utilization == nil {
			return nil, fmt.Errorf("usage reply changed shape: %s has no utilization", key)
		}
		out := &Window{Utilization: *w.Utilization}
		if w.ResetsAt != nil {
			t, err := time.Parse(time.RFC3339Nano, *w.ResetsAt)
			if err != nil {
				return nil, fmt.Errorf("usage reply changed shape: %s resets_at: %w", key, err)
			}
			out.ResetsAt = t
		}
		return out, nil
	}
	five, err := window("five_hour")
	if err != nil {
		return Usage{}, err
	}
	seven, err := window("seven_day")
	if err != nil {
		return Usage{}, err
	}
	return Usage{FiveHour: five, SevenDay: seven}, nil
}
