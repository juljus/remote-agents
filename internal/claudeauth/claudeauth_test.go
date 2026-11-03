package claudeauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func loginJSON(tag string, expiresAt time.Time) string {
	return fmt.Sprintf(`{"accessToken":"fake-access-%[1]s","refreshToken":"fake-refresh-%[1]s","expiresAt":%[2]d,"refreshTokenExpiresAt":%[3]d,"scopes":["user:inference","user:profile"],"subscriptionType":"max","rateLimitTier":"tier"}`,
		tag, expiresAt.UnixMilli(), expiresAt.Add(30*24*time.Hour).UnixMilli())
}

func mustLogin(t *testing.T, raw string) Login {
	t.Helper()
	l, err := parseLogin([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func accountJSON(tag string) string {
	return fmt.Sprintf(`{"accountUuid":"acc-%[1]s","emailAddress":"%[1]s@example.com","organizationUuid":"org-%[1]s","displayName":"%[1]s"}`, tag)
}

func mustAccount(t *testing.T, raw string) Account {
	t.Helper()
	a, err := parseAccount([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPathsFollowClaudeCode(t *testing.T) {
	env := func(vars map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	}
	home := t.TempDir()
	cases := []struct {
		vars map[string]string
		want Paths
	}{
		{nil, Paths{ConfigDir: home + "/.claude", SecureDir: home + "/.claude", GlobalConfig: home + "/.claude.json"}},
		{map[string]string{"CLAUDE_CONFIG_DIR": "/c"}, Paths{ConfigDir: "/c", SecureDir: "/c", GlobalConfig: "/c/.claude.json"}},
		{map[string]string{"CLAUDE_CONFIG_DIR": "/c", "CLAUDE_SECURESTORAGE_CONFIG_DIR": "/s"}, Paths{ConfigDir: "/c", SecureDir: "/s", GlobalConfig: "/c/.claude.json"}},
		{map[string]string{"CLAUDE_CONFIG_DIR": "/c", "CLAUDE_SECURESTORAGE_CONFIG_DIR": ""}, Paths{ConfigDir: "/c", SecureDir: home + "/.claude", GlobalConfig: "/c/.claude.json"}},
	}
	for _, c := range cases {
		got, err := PathsFromEnv(env(c.vars), home)
		if err != nil || got != c.want {
			t.Errorf("%v: got %+v, %v; want %+v", c.vars, got, err, c.want)
		}
	}
	if err := os.MkdirAll(home+"/.claude", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.claude/.config.json", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PathsFromEnv(env(nil), home); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("legacy config: %v", err)
	}
}

func TestLoginNeverPrintsItsTokens(t *testing.T) {
	l := mustLogin(t, loginJSON("x", t0))
	for _, s := range []string{fmt.Sprint(l), fmt.Sprintf("%v %+v %#v %s", l, l, l, l), fmt.Sprint(&l)} {
		if strings.Contains(s, "fake-") {
			t.Fatalf("printed tokens: %s", s)
		}
	}
	if _, err := parseLogin([]byte(`{"accessToken":"a","expiresAt":1}`)); err == nil {
		t.Fatal("login without refresh token parsed")
	}
}

func testStore(t *testing.T, ep Endpoints) Store {
	t.Helper()
	dir := t.TempDir()
	p := Paths{ConfigDir: dir + "/.claude", SecureDir: dir + "/.claude", GlobalConfig: dir + "/.claude.json"}
	return Store{Paths: p, Endpoints: ep, Client: http.DefaultClient, Now: func() time.Time { return t0 }}
}

const otherCredentials = `{"mcpOAuth":{"srv|1":{"serverName":"srv","accessToken":""}},"claudeAiOauth":%s}`

func writeLive(t *testing.T, s Store, login, account string) {
	t.Helper()
	if err := os.MkdirAll(s.Paths.SecureDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Paths.Credentials(), fmt.Appendf(nil, otherCredentials, login), 0o600); err != nil {
		t.Fatal(err)
	}
	config := "{\n  \"numStartups\": 3,\n  \"oauthAccount\": " + account + ",\n  \"theme\": \"dark\"\n}"
	if err := os.WriteFile(s.Paths.GlobalConfig, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceSwapsOnlyTheLoginAndTheAccount(t *testing.T) {
	s := testStore(t, Endpoints{})
	writeLive(t, s, loginJSON("a", t0.Add(time.Hour)), accountJSON("a"))
	b, ab := mustLogin(t, loginJSON("b", t0.Add(time.Hour))), mustAccount(t, accountJSON("b"))
	err := s.WithLive(context.Background(), func(tx LiveTx) error {
		live, err := tx.Read()
		if err != nil {
			return err
		}
		if live.Login == nil || live.Account == nil || live.Account.Identity() != (Identity{"acc-a", "org-a"}) {
			return fmt.Errorf("read %+v", live)
		}
		return tx.Replace(b, ab)
	})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := os.ReadFile(s.Paths.Credentials())
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(otherCredentials, loginJSON("b", t0.Add(time.Hour))); string(creds) != want {
		t.Fatalf("credentials:\n%s\nwant\n%s", creds, want)
	}
	config, err := os.ReadFile(s.Paths.GlobalConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(config), "{\n  \"numStartups\": 3,\n  \"oauthAccount\": {\n    \"accountUuid\": \"acc-b\"") || !strings.HasSuffix(string(config), "\"theme\": \"dark\"\n}") {
		t.Fatalf("config:\n%s", config)
	}
	for path, mode := range map[string]os.FileMode{s.Paths.Credentials(): 0o600, s.Paths.GlobalConfig: 0o644} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s mode %v, %v", path, info.Mode(), err)
		}
	}
	if _, err := os.Stat(s.Paths.GlobalConfig + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("config lock left: %v", err)
	}
}

func TestWithLiveWaitsForClaudeCodesRefresh(t *testing.T) {
	s := testStore(t, Endpoints{})
	writeLive(t, s, loginJSON("a", t0), accountJSON("a"))
	locks, err := s.Paths.refreshLocks()
	if err != nil {
		t.Fatal(err)
	}
	// A Claude Code process is refreshing: it holds the legacy lock too.
	if err := os.Mkdir(locks[1], 0o700); err != nil {
		t.Fatal(err)
	}
	var entered atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- s.WithLive(context.Background(), func(LiveTx) error { entered.Store(true); return nil })
	}()
	time.Sleep(300 * time.Millisecond)
	if entered.Load() {
		t.Fatal("entered while Claude Code held its lock")
	}
	if err := os.Remove(locks[1]); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || !entered.Load() {
		t.Fatalf("WithLive = %v, entered %v", err, entered.Load())
	}
	for _, l := range locks {
		if _, err := os.Stat(l); !os.IsNotExist(err) {
			t.Fatalf("lock %s left: %v", l, err)
		}
	}
}

type tokenServer struct {
	*httptest.Server
	requests []map[string]string
	reply    func(w http.ResponseWriter)
}

func newTokenServer(t *testing.T, reply func(w http.ResponseWriter)) *tokenServer {
	ts := &tokenServer{reply: reply}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || json.Unmarshal(body, &req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ts.requests = append(ts.requests, req)
		ts.reply(w)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestFreshRefreshesAnExpiringLoginAsClaudeCodeDoes(t *testing.T) {
	ts := newTokenServer(t, func(w http.ResponseWriter) {
		fmt.Fprint(w, `{"token_type":"Bearer","access_token":"fake-access-2","refresh_token":"fake-refresh-2","expires_in":28800,"refresh_token_expires_in":2592000,"scope":"user:inference user:profile","account":{"uuid":"acc-a"}}`)
	})
	s := testStore(t, Endpoints{TokenURL: ts.URL})
	fresh := mustLogin(t, loginJSON("1", t0.Add(10*time.Minute)))
	var saved []Login
	save := func(l Login) error { saved = append(saved, l); return nil }
	if got, err := s.Fresh(context.Background(), fresh, save); err != nil || !got.SameTokens(fresh) || len(saved) != 0 {
		t.Fatalf("a fresh login was refreshed: %v", err)
	}
	expiring := mustLogin(t, loginJSON("1", t0.Add(4*time.Minute)))
	got, err := s.Fresh(context.Background(), expiring, save)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"grant_type": "refresh_token", "refresh_token": "fake-refresh-1", "client_id": ClientID, "scope": "user:inference user:profile"}
	if len(ts.requests) != 1 || fmt.Sprint(ts.requests[0]) != fmt.Sprint(want) {
		t.Fatalf("requests %v", ts.requests)
	}
	if len(saved) != 1 || !saved[0].SameTokens(got) {
		t.Fatal("refreshed login not saved")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantRaw := fmt.Sprintf(`{"accessToken":"fake-access-2","refreshToken":"fake-refresh-2","expiresAt":%d,"refreshTokenExpiresAt":%d,"scopes":["user:inference","user:profile"],"subscriptionType":"max","rateLimitTier":"tier"}`,
		t0.Add(8*time.Hour).UnixMilli(), t0.Add(30*24*time.Hour).UnixMilli())
	if string(raw) != wantRaw {
		t.Fatalf("refreshed login:\n%s\nwant\n%s", raw, wantRaw)
	}
}

func TestRefreshKeepsWhatTheReplyLeavesOut(t *testing.T) {
	ts := newTokenServer(t, func(w http.ResponseWriter) { fmt.Fprint(w, `{"access_token":"fake-access-2","expires_in":60}`) })
	s := testStore(t, Endpoints{TokenURL: ts.URL})
	l := mustLogin(t, `{"accessToken":"fake-access-1","refreshToken":"fake-refresh-1","expiresAt":0,"refreshTokenExpiresAt":5,"scopes":["s"],"clientId":"other-client"}`)
	got, err := s.Refresh(context.Background(), l, func(Login) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ts.requests[0]["client_id"] != "other-client" {
		t.Fatalf("client id %q", ts.requests[0]["client_id"])
	}
	raw, _ := json.Marshal(got)
	if want := fmt.Sprintf(`{"accessToken":"fake-access-2","refreshToken":"fake-refresh-1","expiresAt":%d,"refreshTokenExpiresAt":5,"scopes":["s"],"clientId":"other-client"}`, t0.Add(time.Minute).UnixMilli()); string(raw) != want {
		t.Fatalf("got %s", raw)
	}
}

func TestRefreshSavesARotatedRefreshTokenEvenFromAnIncompleteReply(t *testing.T) {
	ts := newTokenServer(t, func(w http.ResponseWriter) { fmt.Fprint(w, `{"refresh_token":"fake-refresh-2"}`) })
	s := testStore(t, Endpoints{TokenURL: ts.URL})
	var saved []Login
	_, err := s.Refresh(context.Background(), mustLogin(t, loginJSON("1", t0)), func(l Login) error { saved = append(saved, l); return nil })
	if err == nil || !strings.Contains(err.Error(), "rotated refresh token was saved") {
		t.Fatalf("err = %v", err)
	}
	if len(saved) != 1 || saved[0].tokens.RefreshToken != "fake-refresh-2" || saved[0].tokens.AccessToken != "" || !saved[0].accessExpired(t0) {
		t.Fatalf("saved %d logins", len(saved))
	}
}

func TestRefreshErrorsNameTheCauseButNoToken(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"error":"invalid_grant","error_description":"Refresh token fake-refresh-1 not found"}`, ErrRefreshRefused.Error()},
		{429, `{"error":"rate_limited"}`, "token endpoint answered 429 (rate_limited)"},
		{500, `oops fake-refresh-1`, "token endpoint answered 500"},
		{200, `{"access_token":"x"}`, "lacks access_token or expires_in"},
		{200, `[]`, "not the JSON object"},
	}
	for _, c := range cases {
		ts := newTokenServer(t, func(w http.ResponseWriter) { w.WriteHeader(c.status); fmt.Fprint(w, c.body) })
		s := testStore(t, Endpoints{TokenURL: ts.URL})
		saved := false
		_, err := s.Refresh(context.Background(), mustLogin(t, loginJSON("1", t0)), func(Login) error { saved = true; return nil })
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "fake-") || saved {
			t.Errorf("%d %s: err = %v, saved %v", c.status, c.body, err, saved)
		}
	}
	signedOut := mustLogin(t, `{"accessToken":"","refreshToken":"","expiresAt":0}`)
	if _, err := testStore(t, Endpoints{}).Refresh(context.Background(), signedOut, nil); !errors.Is(err, ErrRefreshRefused) || !signedOut.SignedOut() {
		t.Fatalf("signed-out login: %v", err)
	}
}

func TestUsageReadsBothWindowsStrictly(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" || r.Header.Get("Authorization") != "Bearer fake-access-1" || r.Header.Get("anthropic-beta") != oauthBeta {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	s := testStore(t, Endpoints{APIBase: srv.URL + "/"})
	l := mustLogin(t, loginJSON("1", t0))
	body = `{"five_hour":{"utilization":12.5,"resets_at":"2026-11-03T17:00:00.5+00:00"},"seven_day":null,"limits":[]}`
	u, err := s.Usage(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	if u.FiveHour == nil || u.FiveHour.Utilization != 12.5 || !u.FiveHour.ResetsAt.Equal(time.Date(2026, 11, 3, 17, 0, 0, 5e8, time.UTC)) || u.SevenDay != nil {
		t.Fatalf("usage %+v", u)
	}
	for _, bad := range []string{`{"five_hour":null}`, `{"five_hour":{"resets_at":null},"seven_day":null}`, `{"five_hour":{"utilization":"x"},"seven_day":null}`, `{"five_hour":{"utilization":1,"resets_at":"soon"},"seven_day":null}`, `[]`} {
		body = bad
		if _, err := s.Usage(context.Background(), l); err == nil || !strings.Contains(err.Error(), "changed shape") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
	if _, err := s.Usage(context.Background(), mustLogin(t, loginJSON("2", t0))); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("refused token: %v", err)
	}
}

func TestWriteAccountNeedsClaudeCodesConfig(t *testing.T) {
	s := testStore(t, Endpoints{})
	err := s.WithLive(context.Background(), func(tx LiveTx) error { return tx.ReplaceAccount(mustAccount(t, accountJSON("a"))) })
	if err == nil || !strings.Contains(err.Error(), "start Claude Code once") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Dir(s.Paths.GlobalConfig) + "/.claude.json"); !os.IsNotExist(err) {
		t.Fatalf("config created: %v", err)
	}
}
