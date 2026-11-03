package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// jwt is an unsigned token with claims, shaped as OpenAI's are.
func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func idToken(t *testing.T, tag string) string {
	return jwt(t, map[string]any{
		"email":                       tag + "@example.com",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-" + tag, "chatgpt_user_id": "user-" + tag, "chatgpt_plan_type": "pro"},
	})
}

func accessToken(t *testing.T, tag string, n int, expires time.Time) string {
	return jwt(t, map[string]any{"exp": expires.Unix(), "tok": fmt.Sprintf("fake-access-%s-%d", tag, n)})
}

// authJSON is an auth.json as Codex 0.166.0 writes it for a ChatGPT login.
func authJSON(t *testing.T, tag string, n int, expires time.Time, extraTokens string) string {
	return fmt.Sprintf(`{
  "auth_mode": "chatgpt",
  "OPENAI_API_KEY": null,
  "tokens": {
    "id_token": %q,
    "access_token": %q,
    "refresh_token": "fake-refresh-%s-%d",
    "account_id": "acct-%s"%s
  },
  "last_refresh": "2026-11-01T02:58:24.839938586Z"
}`, idToken(t, tag), accessToken(t, tag, n, expires), tag, n, tag, extraTokens)
}

func mustLogin(t *testing.T, data string) Login {
	t.Helper()
	l, err := ParseLogin([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func testStore(t *testing.T, ep Endpoints) Store {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".codex")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return Store{Paths: Paths{Home: home}, Endpoints: ep, Client: http.DefaultClient, Now: func() time.Time { return t0 }}
}

func writeLive(t *testing.T, s Store, data string) {
	t.Helper()
	if err := os.WriteFile(s.Paths.AuthFile(), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseLoginReadsAChatGPTLogin(t *testing.T) {
	l := mustLogin(t, authJSON(t, "work", 1, t0.Add(time.Hour), ""))
	if l.Identity() != (Identity{AccountID: "acct-work", UserID: "user-work"}) || l.Email() != "work@example.com" {
		t.Fatalf("identity %+v %s", l.Identity(), l.Email())
	}
	if l.AccessExpiresBy(t0.Add(59*time.Minute)) || !l.AccessExpiresBy(t0.Add(time.Hour)) {
		t.Fatal("access expiry")
	}
	if l.RefreshedBefore(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || !l.RefreshedBefore(t0) {
		t.Fatal("last refresh")
	}
	if s := fmt.Sprint(l) + fmt.Sprintf("%#v", l); strings.Contains(s, "fake-") {
		t.Fatalf("printed a token: %s", s)
	}
	// The profile claim names the email when the top level does not.
	profile := jwt(t, map[string]any{
		"https://api.openai.com/profile": map[string]any{"email": "p@example.com"},
		"https://api.openai.com/auth":    map[string]any{"user_id": "user-p"},
	})
	l = mustLogin(t, fmt.Sprintf(`{"tokens":{"id_token":%q,"access_token":%q,"refresh_token":"r","account_id":"acct-p"}}`, profile, accessToken(t, "p", 1, t0)))
	if l.Identity() != (Identity{AccountID: "acct-p", UserID: "user-p"}) || l.Email() != "p@example.com" || !l.RefreshedBefore(t0) {
		t.Fatalf("profile claims %+v %s", l.Identity(), l.Email())
	}
}

func TestParseLoginRefusesWhatIsNotAChatGPTLogin(t *testing.T) {
	good := authJSON(t, "work", 1, t0, "")
	for name, data := range map[string]string{
		"api key":       `{"OPENAI_API_KEY":"sk-fake"}`,
		"api key mode":  strings.Replace(good, `"auth_mode": "chatgpt"`, `"auth_mode": "apikey"`, 1),
		"key and login": strings.Replace(good, `"OPENAI_API_KEY": null`, `"OPENAI_API_KEY": "sk-fake"`, 1),
	} {
		if _, err := ParseLogin([]byte(data)); !errors.Is(err, ErrNotChatGPT) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, data := range map[string]string{
		"no refresh token": strings.Replace(good, `"refresh_token": "fake-refresh-work-1"`, `"refresh_token": ""`, 1),
		"no account id":    strings.Replace(good, `"account_id": "acct-work"`, `"account_id": null`, 1),
		"bad id token":     strings.Replace(good, idToken(t, "work"), "x.y", 1),
		"no access expiry": strings.Replace(good, accessToken(t, "work", 1, t0), jwt(t, map[string]any{}), 1),
		"trailing data":    good + "{}",
		"bad last refresh": strings.Replace(good, "2026-11-01T02:58:24.839938586Z", "yesterday", 1),
	} {
		if _, err := ParseLogin([]byte(data)); err == nil || errors.Is(err, ErrNotChatGPT) || strings.Contains(err.Error(), "fake-") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSameLoginFollowsRotation(t *testing.T) {
	a := mustLogin(t, authJSON(t, "work", 1, t0, ""))
	if !a.SameLogin(mustLogin(t, authJSON(t, "work", 1, t0.Add(time.Hour), ""))) || a.SameLogin(mustLogin(t, authJSON(t, "work", 2, t0, ""))) {
		t.Fatal("by refresh token")
	}
	owned := func(n int, owner string) Login {
		return mustLogin(t, authJSON(t, "work", n, t0, fmt.Sprintf(`,
    "login_owner_id": %q`, owner)))
	}
	if !owned(1, "o1").SameLogin(owned(2, "o1")) || owned(1, "o1").SameLogin(owned(2, "o2")) {
		t.Fatal("by login owner")
	}
}

func TestReplaceWritesTheWholeLoginAndRefusesAChangedOne(t *testing.T) {
	s := testStore(t, Endpoints{})
	if live, err := s.ReadLive(context.Background()); err != nil || live != nil {
		t.Fatalf("no auth.json: %v, %v", live, err)
	}
	writeLive(t, s, authJSON(t, "work", 1, t0, ""))
	read, err := s.ReadLive(context.Background())
	if err != nil || read == nil || read.Identity().AccountID != "acct-work" {
		t.Fatalf("read %v, %v", read, err)
	}
	home := authJSON(t, "home", 1, t0, `,
    "oauth_provenance": {"issuer": "https://auth.openai.com", "client_id": "app_x"}`)
	if err := s.WithLive(context.Background(), read, func(tx LiveTx) error { return tx.Replace(mustLogin(t, home)) }); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Paths.AuthFile())
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal([]byte(home), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil || fmt.Sprint(got) != fmt.Sprint(want) || !strings.HasPrefix(string(data), "{\n  \"auth_mode\": \"chatgpt\",\n  \"OPENAI_API_KEY\": null,") {
		t.Fatalf("auth.json:\n%s", data)
	}
	if info, err := os.Stat(s.Paths.AuthFile()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v", err)
	}
	// A Codex refreshed the login after it was read.
	writeLive(t, s, authJSON(t, "home", 2, t0, ""))
	ran := false
	err = s.WithLive(context.Background(), read, func(LiveTx) error { ran = true; return nil })
	if !errors.Is(err, ErrLoginChanged) || ran {
		t.Fatalf("WithLive = %v, ran %v", err, ran)
	}
	// An API key login is read as such, never replaced unawares.
	writeLive(t, s, `{"OPENAI_API_KEY":"sk-fake"}`)
	if _, err := s.ReadLive(context.Background()); !errors.Is(err, ErrNotChatGPT) {
		t.Fatalf("api key: %v", err)
	}
}

func TestLiveAccessWaitsForCodexsAuthLock(t *testing.T) {
	s := testStore(t, Endpoints{})
	writeLive(t, s, authJSON(t, "work", 1, t0, ""))
	lock, err := os.OpenFile(filepath.Join(s.Paths.Home, "auth.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	fd := int(lock.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var done atomic.Bool
	released := make(chan struct{})
	defer func() { <-released }()
	go func() {
		defer close(released)
		time.Sleep(300 * time.Millisecond)
		done.Store(true)
		if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
			t.Error(err)
		}
	}()
	if _, err := s.ReadLive(context.Background()); err != nil || !done.Load() {
		t.Fatalf("read while Codex held its lock: %v", err)
	}
}

type refreshServer struct {
	*httptest.Server
	bodies []map[string]string
}

func newRefreshServer(t *testing.T, reply func(w http.ResponseWriter)) *refreshServer {
	rs := &refreshServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad request", http.StatusTeapot)
			return
		}
		rs.bodies = append(rs.bodies, body)
		reply(w)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func TestRefreshSendsWhatCodexSendsAndSavesTheReply(t *testing.T) {
	rs := newRefreshServer(t, func(w http.ResponseWriter) {
		fmt.Fprintf(w, `{"id_token":%q,"access_token":%q,"refresh_token":"fake-refresh-work-2","extra":1}`, idToken(t, "work"), accessToken(t, "work", 2, t0.Add(240*time.Hour)))
	})
	s := testStore(t, Endpoints{TokenOverride: rs.URL})
	old := mustLogin(t, authJSON(t, "work", 1, t0, `,
    "login_owner_id": "owner-1"`))
	ctx, cancel := context.WithCancel(context.Background())
	var saved *Login
	next, err := s.Refresh(ctx, old, ReasonScheduled, func(l Login) error {
		cancel()
		saved = &l
		return nil
	})
	if err != nil || saved == nil {
		t.Fatalf("Refresh = %v, saved %v", err, saved != nil)
	}
	want := map[string]string{"client_id": ClientID, "grant_type": "refresh_token", "refresh_token": "fake-refresh-work-1", "refresh_reason": "scheduled"}
	if fmt.Sprint(rs.bodies) != fmt.Sprint([]map[string]string{want}) {
		t.Fatalf("sent %v", rs.bodies)
	}
	if !next.SameLogin(old) || next.tokens.RefreshToken != "fake-refresh-work-2" || next.AccessExpiresBy(t0.Add(239*time.Hour)) || next.RefreshedBefore(t0) {
		t.Fatalf("refreshed %+v", next.tokens.RefreshToken)
	}
	data, err := json.Marshal(next)
	if err != nil || !strings.Contains(string(data), `"login_owner_id":"owner-1"`) || !strings.Contains(string(data), `"last_refresh":"2026-11-03T12:00:00Z"`) {
		t.Fatalf("saved %s, %v", data, err)
	}
}

func TestRefreshUsesTheLoginsOwnAuthority(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		asked = append(asked, r.URL.Path+" "+body["client_id"])
		fmt.Fprintf(w, `{"access_token":%q}`, accessToken(t, "work", 2, t0.Add(time.Hour)))
	}))
	defer srv.Close()
	provenance := fmt.Sprintf(`,
    "oauth_provenance": {"issuer": %q, "client_id": "app_own"}`, srv.URL+"/issuer")
	s := testStore(t, Endpoints{})
	next, err := s.Refresh(context.Background(), mustLogin(t, authJSON(t, "work", 1, t0, provenance)), ReasonProactive, func(Login) error { return nil })
	if err != nil || fmt.Sprint(asked) != "[/issuer/oauth/token app_own]" {
		t.Fatalf("asked %v, %v", asked, err)
	}
	// A reply without a refresh token keeps the old one, and the authority that issued it.
	if data, _ := json.Marshal(next); next.tokens.RefreshToken != "fake-refresh-work-1" || !strings.Contains(string(data), `"oauth_provenance"`) {
		t.Fatalf("next %s", data)
	}
	// A test endpoint is not the login's authority, which the login then forgets, as Codex does.
	s.Endpoints.TokenOverride = srv.URL + "/test"
	next, err = s.Refresh(context.Background(), next, ReasonProactive, func(Login) error { return nil })
	if data, _ := json.Marshal(next); err != nil || strings.Contains(string(data), `"oauth_provenance"`) || asked[1] != "/test app_own" {
		t.Fatalf("override: %v %v %s", err, asked, data)
	}
}

func TestRefreshErrorsSayWhetherTheLoginIsGone(t *testing.T) {
	cases := []struct {
		status  int
		body    string
		refused bool
	}{
		{401, `{}`, true},
		{400, `{"error":"invalid_grant","error_description":"fake-refresh-work-1 is gone"}`, true},
		{400, `{"error":{"code":"refresh_token_reused"}}`, true},
		{403, `{"code":"refresh_token_invalidated"}`, true},
		{400, `{"error":"refresh_token_expired"}`, true},
		{500, `{"error":"invalid_grant"}`, false},
		{400, `{"error":"fake-refresh-work-1"}`, false},
		{429, `oops fake-refresh-work-1`, false},
		{200, `not json fake-refresh-work-1`, false},
	}
	for _, c := range cases {
		srv := newRefreshServer(t, func(w http.ResponseWriter) { w.WriteHeader(c.status); fmt.Fprint(w, c.body) })
		s := testStore(t, Endpoints{TokenOverride: srv.URL})
		saved := false
		_, err := s.Refresh(context.Background(), mustLogin(t, authJSON(t, "work", 1, t0, "")), ReasonScheduled, func(Login) error { saved = true; return nil })
		if err == nil || errors.Is(err, ErrRefreshRefused) != c.refused || strings.Contains(err.Error(), "fake-") || saved {
			t.Errorf("%d %s: %v, saved %v", c.status, c.body, err, saved)
		}
	}
}

func TestFreshRefreshesOnlyNearExpiry(t *testing.T) {
	var calls atomic.Int32
	srv := newRefreshServer(t, func(w http.ResponseWriter) {
		calls.Add(1)
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"fake-refresh-work-2"}`, accessToken(t, "work", 2, t0.Add(240*time.Hour)))
	})
	s := testStore(t, Endpoints{TokenOverride: srv.URL})
	keep := func(Login) error { return nil }
	if _, err := s.Fresh(context.Background(), mustLogin(t, authJSON(t, "work", 1, t0.Add(6*time.Minute), "")), keep); err != nil || calls.Load() != 0 {
		t.Fatalf("not due: %v, %d calls", err, calls.Load())
	}
	l, err := s.Fresh(context.Background(), mustLogin(t, authJSON(t, "work", 1, t0.Add(5*time.Minute), "")), keep)
	if err != nil || calls.Load() != 1 || l.tokens.RefreshToken != "fake-refresh-work-2" || srv.bodies[0]["refresh_reason"] != "proactive_expiry" {
		t.Fatalf("due: %v, %d calls", err, calls.Load())
	}
}

func TestUsageReadsBothWindowsByTheirLength(t *testing.T) {
	var body string
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" || r.Header.Get("ChatGPT-Account-Id") != "acct-work" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	s := testStore(t, Endpoints{APIBase: srv.URL + "/backend-api"})
	l := mustLogin(t, authJSON(t, "work", 1, t0.Add(time.Hour), ""))
	status = 200
	body = `{"email":"work@example.com","rate_limit":{"allowed":true,"limit_reached":false,
	  "primary_window":{"used_percent":11,"limit_window_seconds":604800,"reset_after_seconds":5,"reset_at":1793000000},
	  "secondary_window":{"used_percent":40.5,"limit_window_seconds":18000,"reset_after_seconds":5,"reset_at":1792000000}},"credits":{}}`
	u, err := s.Usage(context.Background(), l)
	if err != nil || u.SevenDay == nil || u.SevenDay.Utilization != 11 || !u.SevenDay.ResetsAt.Equal(time.Unix(1793000000, 0)) || u.FiveHour == nil || u.FiveHour.Utilization != 40.5 {
		t.Fatalf("usage %+v, %v", u, err)
	}
	body = `{"rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":604800,"reset_after_seconds":5,"reset_at":1793000000},"secondary_window":null}}`
	if u, err := s.Usage(context.Background(), l); err != nil || u.FiveHour != nil || u.SevenDay == nil {
		t.Fatalf("one window: %+v, %v", u, err)
	}
	for _, bad := range []string{
		`{}`,
		`{"rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":3600,"reset_at":1}}}`,
		`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"reset_at":1}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":18000,"reset_at":1},"secondary_window":{"used_percent":3,"limit_window_seconds":18000,"reset_at":1}}}`,
	} {
		body = bad
		if _, err := s.Usage(context.Background(), l); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
	status, body = 401, `{}`
	if _, err := s.Usage(context.Background(), l); !errors.Is(err, ErrTokenRefused) {
		t.Fatalf("refused: %v", err)
	}
}
