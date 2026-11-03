// Package codexfake stands in for OpenAI's token and usage endpoints in
// tests, with logins shaped as Codex keeps them. Each refresh token
// fake-refresh-<tag>-<n> works once, as a rotating server's does, and a
// login of tag signs in as account acct-<tag>, user user-<tag>.
package codexfake

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// AccessLifetime is how long the access tokens the server issues last, as OpenAI's do.
const AccessLifetime = 240 * time.Hour

// Login is an auth.json for tag's login n, whose access token expires at expires.
func Login(tag string, n int, expires time.Time) string {
	return fmt.Sprintf(`{
  "auth_mode": "chatgpt",
  "OPENAI_API_KEY": null,
  "tokens": {
    "id_token": %q,
    "access_token": %q,
    "refresh_token": %q,
    "account_id": "acct-%s"
  },
  "last_refresh": "2026-11-01T02:58:24.839938586Z"
}`, IDToken(tag), AccessToken(tag, n, expires), RefreshToken(tag, n), tag)
}

func RefreshToken(tag string, n int) string { return fmt.Sprintf("fake-refresh-%s-%d", tag, n) }

// IDToken is an unsigned id token for tag, with the claims Codex reads.
func IDToken(tag string) string {
	return jwt(map[string]any{
		"email":                       tag + "@example.com",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-" + tag, "chatgpt_user_id": "user-" + tag},
	})
}

// AccessToken is an unsigned access token for tag's login n.
func AccessToken(tag string, n int, expires time.Time) string {
	return jwt(map[string]any{"exp": expires.Unix(), "tok": fmt.Sprintf("fake-access-%s-%d", tag, n)})
}

func jwt(claims map[string]any) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// Server is the fake; it remembers every refresh token sent to it.
type Server struct {
	*httptest.Server
	Now func() time.Time

	mu        sync.Mutex
	used      map[string]bool
	next      int
	onRefresh func()
	presented []string
}

var (
	refreshPattern = regexp.MustCompile(`^fake-refresh-([a-z]+)-\d+$`)
	accessPattern  = regexp.MustCompile(`^fake-access-([a-z]+)-\d+$`)
)

func NewServer(t *testing.T, now func() time.Time) *Server {
	s := &Server{Now: now, used: map[string]bool{}, next: 100}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", s.token)
	mux.HandleFunc("GET /backend-api/wham/usage", s.usage)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// Spend uses up a refresh token, as another copy of its login refreshing would.
func (s *Server) Spend(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used[token] = true
}

// OnRefresh has the next refresh run fn before the server answers it.
func (s *Server) OnRefresh(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRefresh = fn
}

// PresentedAny is those of tokens that were sent to the server.
func (s *Server) PresentedAny(tokens []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, rt := range s.presented {
		if slices.Contains(tokens, rt) {
			out = append(out, rt)
		}
	}
	return out
}

func reply(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		panic(err)
	}
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		reply(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	s.mu.Lock()
	hook := s.onRefresh
	s.onRefresh = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token := req["refresh_token"]
	s.presented = append(s.presented, token)
	m := refreshPattern.FindStringSubmatch(token)
	if req["grant_type"] != "refresh_token" || m == nil {
		reply(w, 400, map[string]string{"error": "invalid_grant"})
		return
	}
	if s.used[token] {
		reply(w, 401, map[string]any{"error": map[string]string{"code": "refresh_token_reused"}})
		return
	}
	s.used[token] = true
	s.next++
	tag := m[1]
	reply(w, 200, map[string]string{
		"id_token": IDToken(tag), "access_token": AccessToken(tag, s.next, s.Now().Add(AccessLifetime)), "refresh_token": RefreshToken(tag, s.next),
	})
}

// usage reports 10% of the weekly window used per letter of the tag.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	var claims struct {
		Tok string `json:"tok"`
	}
	if ok {
		ok = decode(auth, &claims) == nil
	}
	m := accessPattern.FindStringSubmatch(claims.Tok)
	if !ok || m == nil || r.Header.Get("ChatGPT-Account-Id") != "acct-"+m[1] {
		reply(w, 401, map[string]string{})
		return
	}
	reply(w, 200, map[string]any{"rate_limit": map[string]any{
		"primary_window": map[string]any{"used_percent": 10 * len(m[1]), "limit_window_seconds": 604800, "reset_after_seconds": 60, "reset_at": 1793000000},
	}})
}

func decode(token string, v any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, v)
}
