package claudeauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
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

// Claude Code's timeouts for a token refresh and for its profile and usage reads.
const (
	refreshTimeout = 30 * time.Second
	readTimeout    = 10 * time.Second
)

// oauthBeta is the anthropic-beta value Claude Code sends with OAuth tokens.
const oauthBeta = "oauth-2025-04-20"

// ErrTokenRefused means the server no longer takes an access token.
var ErrTokenRefused = errors.New("the server refused the access token")

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

// Claude Code 2.1.317 asks a claude.ai login without a client id of its own
// for its current scopes, plus the project scopes the login holds; when the
// server refuses those it asks again for the scopes the login holds.
var (
	defaultScopes = []string{"user:profile", "user:inference", "user:sessions:claude_code", "user:mcp_servers", "user:file_upload", "user:plugins"}
	keptScopes    = []string{"user:projects:read", "user:projects:write"}
)

const inferenceScope = "user:inference"

// refreshScopes are the scopes Claude Code asks for first, and whether it
// asks again for the held ones when the server refuses them.
func (t tokens) refreshScopes() (scopes []string, retry bool) {
	if t.ClientID != nil || (!slices.Contains(t.Scopes, inferenceScope) && t.SubscriptionType == nil) {
		return t.heldScopes(), false
	}
	scopes = slices.Clone(defaultScopes)
	for _, s := range t.Scopes {
		if slices.Contains(keptScopes, s) && !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	return scopes, slices.Contains(t.Scopes, inferenceScope)
}

// heldScopes are the login's scopes; Claude Code asks for its defaults when it holds none.
func (t tokens) heldScopes() []string {
	if len(t.Scopes) == 0 {
		return slices.Clone(defaultScopes)
	}
	return t.Scopes
}

// errInvalidScope is the server refusing the scopes asked for, before it rotated anything.
var errInvalidScope = errors.New("token endpoint refused the scopes asked for")

// refresh trades l's refresh token for new tokens as Claude Code does. The
// server retires the old refresh token once it answers, so save receives
// every login the server issued before refresh returns, even an incomplete one.
func refresh(ctx context.Context, client *http.Client, ep Endpoints, l Login, now func() time.Time, save func(Login) error) (Login, error) {
	if l.tokens.RefreshToken == "" {
		return Login{}, ErrRefreshRefused
	}
	scopes, retry := l.tokens.refreshScopes()
	next, err := requestTokens(ctx, client, ep, l, scopes, now, save)
	if errors.Is(err, errInvalidScope) && retry {
		next, err = requestTokens(ctx, client, ep, l, l.tokens.heldScopes(), now, save)
	}
	return next, err
}

func requestTokens(ctx context.Context, client *http.Client, ep Endpoints, l Login, scopes []string, now func() time.Time, save func(Login) error) (Login, error) {
	clientID := ClientID
	if l.tokens.ClientID != nil {
		clientID = *l.tokens.ClientID
	}
	body, err := json.Marshal(map[string]string{
		"grant_type": "refresh_token", "refresh_token": l.tokens.RefreshToken,
		"client_id": clientID, "scope": strings.Join(scopes, " "),
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
	next, complete, err := l.refreshed(reply, scopes, issuedAt)
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

// refreshed is l with reply's tokens. As Claude Code merges them, an absent
// refresh token or refresh expiry keeps the old one; an absent scope means
// the scopes asked for, as OAuth defines it, where Claude Code would store
// none. complete is false when the reply had no usable access token.
func (l Login) refreshed(reply tokenReply, asked []string, issuedAt time.Time) (next Login, complete bool, err error) {
	complete = reply.AccessToken != nil && *reply.AccessToken != "" && reply.ExpiresIn != nil && *reply.ExpiresIn > 0
	if !complete && (reply.RefreshToken == nil || *reply.RefreshToken == l.tokens.RefreshToken) {
		return Login{}, false, errors.New("token endpoint reply lacks access_token or expires_in")
	}
	type update struct {
		key   string
		value any
	}
	// An expired login keeps its old access token: Claude Code refreshes it
	// before use, while an empty one would read as signed out.
	updates := []update{{"expiresAt", 0}, {"scopes", asked}}
	if complete {
		updates = []update{{"accessToken", *reply.AccessToken}, {"expiresAt", issuedAt.UnixMilli() + int64(*reply.ExpiresIn*1000)}, {"scopes", asked}}
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
	if json.Unmarshal(body, &e) != nil {
		return fmt.Errorf("token endpoint answered %d", status)
	}
	switch {
	case e.Error == "invalid_grant":
		return ErrRefreshRefused
	case status == http.StatusBadRequest && e.Error == "invalid_scope":
		return errInvalidScope
	case e.Error != "":
		return fmt.Errorf("token endpoint answered %d (%s)", status, e.Error)
	}
	return fmt.Errorf("token endpoint answered %d", status)
}

// fetchIdentity asks the server whom l's access token signs in as, through
// the profile endpoint Claude Code reads; it too is undocumented, so a reply
// without the ids read here is an error.
func fetchIdentity(ctx context.Context, client *http.Client, ep Endpoints, l Login) (Identity, error) {
	data, err := get(ctx, client, ep, "/api/oauth/profile", l, "profile", "Cache-Control", "no-cache")
	if err != nil {
		return Identity{}, err
	}
	var reply struct {
		Account *struct {
			UUID string `json:"uuid"`
		} `json:"account"`
		Organization *struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return Identity{}, fmt.Errorf("profile reply changed shape: %w", err)
	}
	if reply.Account == nil || reply.Account.UUID == "" || reply.Organization == nil || reply.Organization.UUID == "" {
		return Identity{}, errors.New("profile reply changed shape: no account or organization uuid")
	}
	return Identity{AccountUUID: reply.Account.UUID, OrganizationUUID: reply.Organization.UUID}, nil
}

// get reads an OAuth API path with l's access token and the one extra
// header Claude Code sends for it.
func get(ctx context.Context, client *http.Client, ep Endpoints, path string, l Login, what, header, value string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(ep.APIBase, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+l.tokens.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, value)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading the %s: %w", what, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading the %s: %w", what, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%s endpoint answered 401: %w", what, ErrTokenRefused)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s endpoint answered %d", what, resp.StatusCode)
	}
	return data, nil
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
	data, err := get(ctx, client, ep, "/api/oauth/usage", l, "usage", "anthropic-beta", oauthBeta)
	if err != nil {
		return Usage{}, err
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
