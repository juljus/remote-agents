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
	home := t.TempDir()
	got, err := DefaultPaths(home)
	if want := (Paths{ConfigDir: home + "/.claude", SecureDir: home + "/.claude", GlobalConfig: home + "/.claude.json"}); err != nil || got != want {
		t.Fatalf("default %+v, %v", got, err)
	}
	if got.EnvDir() != "" {
		t.Fatalf("default env dir %q", got.EnvDir())
	}
	got, err = PathsIn("/c")
	if want := (Paths{ConfigDir: "/c", SecureDir: "/c", GlobalConfig: "/c/.claude.json"}); err != nil || got != want {
		t.Fatalf("in /c %+v, %v", got, err)
	}
	if got.EnvDir() != "/c" {
		t.Fatalf("env dir %q, want /c", got.EnvDir())
	}
	if err := os.MkdirAll(home+"/.claude", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.claude/.config.json", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DefaultPaths(home); err == nil || !strings.Contains(err.Error(), "legacy") {
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

// withLive runs fn as a command does: on the live login it read just before.
func withLive(t *testing.T, s Store, fn func(LiveTx) error) error {
	t.Helper()
	live, err := s.ReadLive(context.Background())
	if err != nil {
		return err
	}
	return s.WithLive(context.Background(), live, fn)
}

func TestWithLiveAndReplaceRefuseALoginThatChangedSinceRead(t *testing.T) {
	s := testStore(t, Endpoints{})
	writeLive(t, s, loginJSON("a", t0), accountJSON("a"))
	read, err := s.ReadLive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A session refreshed the login after the command read it.
	writeLive(t, s, loginJSON("a2", t0), accountJSON("a"))
	ran := false
	if err := s.WithLive(context.Background(), read, func(LiveTx) error { ran = true; return nil }); !errors.Is(err, ErrLoginChanged) || ran {
		t.Fatalf("WithLive = %v, ran %v", err, ran)
	}
	// A sign-in in a session lands while the command holds the refresh lock:
	// Claude Code's sign-in takes only the credentials write lock.
	read, err = s.ReadLive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = s.WithLive(context.Background(), read, func(tx LiveTx) error {
		writeLive(t, s, loginJSON("c", t0), accountJSON("c"))
		return tx.Replace(mustLogin(t, loginJSON("b", t0)), mustAccount(t, accountJSON("b")))
	})
	if !errors.Is(err, ErrLoginChanged) {
		t.Fatalf("Replace = %v", err)
	}
	if data, _ := os.ReadFile(s.Paths.Credentials()); !strings.Contains(string(data), "fake-access-c") {
		t.Fatalf("the sign-in was overwritten:\n%s", data)
	}
	// A box with no login stays without one until the switch writes it.
	if err := os.Remove(s.Paths.Credentials()); err != nil {
		t.Fatal(err)
	}
	if err := withLive(t, s, func(tx LiveTx) error {
		return tx.Replace(mustLogin(t, loginJSON("b", t0)), mustAccount(t, accountJSON("b")))
	}); err != nil {
		t.Fatal(err)
	}
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
	live, err := s.ReadLive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if live.Login == nil || live.Account == nil || live.Account.Identity() != (Identity{"acc-a", "org-a"}) {
		t.Fatalf("read %+v", live)
	}
	err = s.WithLive(context.Background(), live, func(tx LiveTx) error { return tx.Replace(b, ab) })
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
		done <- withLive(t, s, func(LiveTx) error { entered.Store(true); return nil })
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
	want := map[string]string{"grant_type": "refresh_token", "refresh_token": "fake-refresh-1", "client_id": ClientID, "scope": "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload user:plugins"}
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
	// The old access token stays: an empty one would read as signed out to Claude Code.
	if len(saved) != 1 || saved[0].tokens.RefreshToken != "fake-refresh-2" || saved[0].tokens.AccessToken != "fake-access-1" || !saved[0].accessExpired(t0) {
		t.Fatalf("saved %d logins", len(saved))
	}
}

func TestRefreshAsksForTheScopesClaudeCodeAsksFor(t *testing.T) {
	cases := []struct {
		login string
		want  []string
	}{
		// A claude.ai login: Claude Code's scopes plus the project scopes it holds, then its own on invalid_scope.
		{`{"accessToken":"a","refreshToken":"r","expiresAt":0,"scopes":["user:inference","user:projects:read","org:x"]}`,
			[]string{"user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload user:plugins user:projects:read", "user:inference user:projects:read org:x"}},
		// One with a client id of its own asks for what it holds, or the defaults when it holds none.
		{`{"accessToken":"a","refreshToken":"r","expiresAt":0,"scopes":["user:inference"],"clientId":"c"}`, []string{"user:inference"}},
		{`{"accessToken":"a","refreshToken":"r","expiresAt":0,"scopes":[],"clientId":"c"}`,
			[]string{"user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload user:plugins"}},
		// Without the inference scope Claude Code does not ask again.
		{`{"accessToken":"a","refreshToken":"r","expiresAt":0,"scopes":[],"subscriptionType":"max"}`,
			[]string{"user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload user:plugins"}},
	}
	for _, c := range cases {
		ts := newTokenServer(t, func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_scope"}`)
		})
		s := testStore(t, Endpoints{TokenURL: ts.URL})
		_, err := s.Refresh(context.Background(), mustLogin(t, c.login), func(Login) error { return nil })
		if !errors.Is(err, errInvalidScope) {
			t.Fatalf("err = %v", err)
		}
		var got []string
		for _, r := range ts.requests {
			got = append(got, r["scope"])
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s asked for\n%q\nwant\n%q", c.login, got, c.want)
		}
	}
}

func TestIdentityReadsTheProfileStrictly(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/profile" || r.Header.Get("Authorization") != "Bearer fake-access-1" || r.Header.Get("Cache-Control") != "no-cache" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	s := testStore(t, Endpoints{APIBase: srv.URL})
	body = `{"account":{"uuid":"acc-a","email":"a@example.com"},"organization":{"uuid":"org-a"},"application":{}}`
	id, err := s.Identity(context.Background(), mustLogin(t, loginJSON("1", t0)))
	if err != nil || id != (Identity{"acc-a", "org-a"}) {
		t.Fatalf("identity %+v, %v", id, err)
	}
	for _, bad := range []string{`{"account":{"uuid":"acc-a"}}`, `{"account":{},"organization":{"uuid":"o"}}`, `[]`} {
		body = bad
		if _, err := s.Identity(context.Background(), mustLogin(t, loginJSON("1", t0))); err == nil || !strings.Contains(err.Error(), "changed shape") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := s.Identity(context.Background(), mustLogin(t, loginJSON("2", t0))); !errors.Is(err, ErrTokenRefused) {
		t.Fatalf("refused token: %v", err)
	}
}

func TestReplaceWaitsForClaudeCodesCredentialsWrite(t *testing.T) {
	s := testStore(t, Endpoints{})
	writeLive(t, s, loginJSON("a", t0), accountJSON("a"))
	// A Claude Code process is saving an MCP token.
	if err := os.Mkdir(s.Paths.storageWriteLock(), 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- withLive(t, s, func(tx LiveTx) error {
			return tx.Replace(mustLogin(t, loginJSON("b", t0)), mustAccount(t, accountJSON("b")))
		})
	}()
	time.Sleep(300 * time.Millisecond)
	if data, _ := os.ReadFile(s.Paths.Credentials()); strings.Contains(string(data), "fake-access-b") {
		t.Fatal("wrote while Claude Code held its write lock")
	}
	if err := os.Remove(s.Paths.storageWriteLock()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(s.Paths.Credentials()); !strings.Contains(string(data), "fake-access-b") {
		t.Fatal("credentials not replaced")
	}
}

func TestReplaceWritesThroughASymlinkedConfigAndRefusesSymlinkedCredentials(t *testing.T) {
	s := testStore(t, Endpoints{})
	writeLive(t, s, loginJSON("a", t0), accountJSON("a"))
	real := s.Paths.GlobalConfig + ".real"
	if err := os.Rename(s.Paths.GlobalConfig, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, s.Paths.GlobalConfig); err != nil {
		t.Fatal(err)
	}
	err := withLive(t, s, func(tx LiveTx) error { return tx.ReplaceAccount(mustAccount(t, accountJSON("b"))) })
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(s.Paths.GlobalConfig); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v", err)
	}
	if data, _ := os.ReadFile(real); !strings.Contains(string(data), "acc-b") {
		t.Fatalf("config not written through the link:\n%s", data)
	}
	creds := s.Paths.Credentials() + ".real"
	if err := os.Rename(s.Paths.Credentials(), creds); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(creds, s.Paths.Credentials()); err != nil {
		t.Fatal(err)
	}
	err = withLive(t, s, func(tx LiveTx) error {
		return tx.Replace(mustLogin(t, loginJSON("b", t0)), mustAccount(t, accountJSON("b")))
	})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshErrorsNameTheCauseButNoToken(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"error":"invalid_grant","error_description":"Refresh token fake-refresh-1 not found"}`, ErrRefreshRefused.Error()},
		{401, `{"error":"invalid_grant"}`, ErrRefreshRefused.Error()},
		{500, `{"error":"invalid_grant"}`, "token endpoint answered 500 (invalid_grant)"},
		{429, `{"error":"rate_limited"}`, "token endpoint answered 429 (rate_limited)"},
		{500, `oops fake-refresh-1`, "token endpoint answered 500"},
		{200, `{"access_token":"x"}`, "lacks access_token or expires_in"},
		{400, `{"error":"invalid_scope"}`, errInvalidScope.Error()},
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

func TestUsageReadsTheWeeklyWindowStrictly(t *testing.T) {
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
	body = `{"five_hour":null,"seven_day":{"utilization":12.5,"resets_at":"2026-11-03T17:00:00.5+00:00"},"limits":[]}`
	u, err := s.Usage(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	if u.Weekly == nil || u.Weekly.Utilization != 12.5 || u.Weekly.ResetsAt == nil || !u.Weekly.ResetsAt.Equal(time.Date(2026, 11, 3, 17, 0, 0, 5e8, time.UTC)) {
		t.Fatalf("usage %+v", u)
	}
	// A window that has not started names no reset.
	body = `{"seven_day":{"utilization":0,"resets_at":null}}`
	if u, err := s.Usage(context.Background(), l); err != nil || u.Weekly == nil || u.Weekly.ResetsAt != nil {
		t.Fatalf("not started: %+v, %v", u, err)
	}
	body = `{"seven_day":null}`
	if u, err := s.Usage(context.Background(), l); err != nil || u.Weekly != nil {
		t.Fatalf("no weekly limit: %+v, %v", u, err)
	}
	for _, bad := range []string{`{"five_hour":null}`, `{"seven_day":{"resets_at":null}}`, `{"seven_day":{"utilization":"x"}}`, `{"seven_day":{"utilization":1,"resets_at":"soon"}}`, `[]`} {
		body = bad
		if _, err := s.Usage(context.Background(), l); err == nil || !strings.Contains(err.Error(), "changed shape") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
	if _, err := s.Usage(context.Background(), mustLogin(t, loginJSON("2", t0))); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("refused token: %v", err)
	}
}

func TestWritesNeedClaudeCodesConfig(t *testing.T) {
	s := testStore(t, Endpoints{})
	err := withLive(t, s, func(tx LiveTx) error { return tx.ReplaceAccount(mustAccount(t, accountJSON("a"))) })
	if err == nil || !strings.Contains(err.Error(), "start Claude Code once") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Dir(s.Paths.GlobalConfig) + "/.claude.json"); !os.IsNotExist(err) {
		t.Fatalf("config created: %v", err)
	}
	// A switch then changes neither file, rather than the credentials alone.
	writeLive(t, s, loginJSON("a", t0), accountJSON("a"))
	if err := os.Remove(s.Paths.GlobalConfig); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.Paths.Credentials())
	if err != nil {
		t.Fatal(err)
	}
	err = withLive(t, s, func(tx LiveTx) error {
		return tx.Replace(mustLogin(t, loginJSON("b", t0)), mustAccount(t, accountJSON("b")))
	})
	if err == nil || !strings.Contains(err.Error(), "start Claude Code once") {
		t.Fatalf("Replace = %v", err)
	}
	if after, err := os.ReadFile(s.Paths.Credentials()); err != nil || string(after) != string(before) {
		t.Fatalf("credentials changed: %v\n%s", err, after)
	}
}
