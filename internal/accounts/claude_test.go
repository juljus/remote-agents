package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"remote-agents/internal/claudeauth"
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// fakeAnthropic honours each refresh token once, as a rotating OAuth server
// does, and reports usage per access token.
type fakeAnthropic struct {
	mu           sync.Mutex
	valid        map[string]string // refresh token -> account tag
	usage        map[string]string // access token -> usage reply
	tags         map[string]string // access token -> account tag
	next         int
	refreshes    []string // tags refreshed, in order
	presented    []string // refresh tokens presented, in order
	profileReads int
	onRefresh    func()
}

func newFakeAnthropic(t *testing.T) (*fakeAnthropic, claudeauth.Endpoints) {
	f := &fakeAnthropic{valid: map[string]string{}, usage: map[string]string{}, tags: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// onRefresh runs outside mu: it may issue logins of its own.
		if f.onRefresh != nil {
			f.onRefresh()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.presented = append(f.presented, req["refresh_token"])
		tag, ok := f.valid[req["refresh_token"]]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		delete(f.valid, req["refresh_token"])
		f.next++
		f.refreshes = append(f.refreshes, tag)
		// The server numbers what it issues from 101, apart from the tests' own logins.
		access, refresh := f.issue(tag, 100+f.next)
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":28800,"refresh_token_expires_in":2592000,"scope":"user:inference user:profile"}`, access, refresh)
	})
	mux.HandleFunc("GET /api/oauth/profile", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.profileReads++
		tag, ok := f.tags[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"account":{"uuid":"acc-%[1]s","email":"%[1]s@example.com"},"organization":{"uuid":"org-%[1]s"}}`, tag)
	})
	mux.HandleFunc("GET /api/oauth/usage", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, ok := f.usage[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, claudeauth.Endpoints{TokenURL: srv.URL + "/token", APIBase: srv.URL}
}

// revoke makes the server refuse an access token before it expires.
func (f *fakeAnthropic) revoke(access string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tags, access)
	delete(f.usage, access)
}

// issue registers a token pair for tag; the caller holds mu.
func (f *fakeAnthropic) issue(tag string, n int) (access, refresh string) {
	access, refresh = fmt.Sprintf("fake-access-%s-%d", tag, n), fmt.Sprintf("fake-refresh-%s-%d", tag, n)
	f.valid[refresh] = tag
	f.tags[access] = tag
	f.usage[access] = fmt.Sprintf(`{"five_hour":{"utilization":%d,"resets_at":"2026-11-03T15:00:00+00:00"},"seven_day":null}`, len(tag)*10)
	return access, refresh
}

type fixture struct {
	t     *testing.T
	api   *fakeAnthropic
	paths claudeauth.Paths
	vault ClaudeVault
	// live holds every refresh token the box's login had: none may ever be presented.
	live []string
}

func newFixture(t *testing.T) *fixture {
	home := t.TempDir()
	api, ep := newFakeAnthropic(t)
	paths := claudeauth.Paths{ConfigDir: home + "/.claude", SecureDir: home + "/.claude", GlobalConfig: home + "/.claude.json"}
	store := claudeauth.Store{Paths: paths, Endpoints: ep, Client: http.DefaultClient, Now: func() time.Time { return t0 }}
	f := &fixture{t: t, api: api, paths: paths, vault: ClaudeVault{Dir: home + "/.config/remote-agents/accounts/claude", Store: store}}
	if err := os.MkdirAll(paths.SecureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.GlobalConfig, []byte("{\n  \"numStartups\": 1\n}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, rt := range f.api.presented {
			if slices.Contains(f.live, rt) {
				t.Errorf("refreshed the box's login %s", rt)
			}
		}
	})
	return f
}

// login is a login JSON for tag whose tokens the server honours; its access
// token expires after expiresIn and its refresh token after refreshIn.
func (f *fixture) login(tag string, n int, expiresIn, refreshIn time.Duration) string {
	f.api.mu.Lock()
	access, refresh := f.api.issue(tag, n)
	f.api.mu.Unlock()
	return fmt.Sprintf(`{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"refreshTokenExpiresAt":%d,"scopes":["user:inference","user:profile"],"subscriptionType":"max"}`,
		access, refresh, t0.Add(expiresIn).UnixMilli(), t0.Add(refreshIn).UnixMilli())
}

func account(tag string) string {
	return fmt.Sprintf(`{"accountUuid":"acc-%[1]s","emailAddress":"%[1]s@example.com","organizationUuid":"org-%[1]s"}`, tag)
}

func (f *fixture) setLive(login, acct string) {
	f.t.Helper()
	var l tokenView
	if err := json.Unmarshal([]byte(login), &l); err != nil {
		f.t.Fatal(err)
	}
	f.live = append(f.live, l.RefreshToken)
	creds := `{"mcpOAuth":{"srv":{"accessToken":""}},"claudeAiOauth":` + login + `}`
	if err := os.WriteFile(f.paths.Credentials(), []byte(creds), 0o600); err != nil {
		f.t.Fatal(err)
	}
	config := "{\n  \"numStartups\": 1,\n  \"oauthAccount\": " + acct + "\n}"
	if err := os.WriteFile(f.paths.GlobalConfig, []byte(config), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) seed(name, login, acct string) {
	f.t.Helper()
	if err := f.vault.files().ensureDir(); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.vault.files().entryPath(name), []byte(`{"claudeAiOauth":`+login+`,"oauthAccount":`+acct+`}`), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// markLast records name as the account the vault last switched to.
func (f *fixture) markLast(name string) {
	f.t.Helper()
	if err := f.vault.files().ensureDir(); err != nil {
		f.t.Fatal(err)
	}
	if err := f.vault.markSwitched(name); err != nil {
		f.t.Fatal(err)
	}
}

type tokenView struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

func (f *fixture) liveTokens() tokenView {
	f.t.Helper()
	var creds struct {
		MCP   json.RawMessage `json:"mcpOAuth"`
		OAuth tokenView       `json:"claudeAiOauth"`
	}
	data, err := os.ReadFile(f.paths.Credentials())
	if err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		f.t.Fatal(err)
	}
	if string(creds.MCP) != `{"srv":{"accessToken":""}}` {
		f.t.Fatalf("mcpOAuth changed: %s", creds.MCP)
	}
	return creds.OAuth
}

func (f *fixture) liveEmail() string {
	f.t.Helper()
	var config struct {
		Account struct {
			Email string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	data, err := os.ReadFile(f.paths.GlobalConfig)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		f.t.Fatal(err)
	}
	return config.Account.Email
}

func (f *fixture) entryTokens(name string) tokenView {
	f.t.Helper()
	var e struct {
		OAuth tokenView `json:"claudeAiOauth"`
	}
	data, err := os.ReadFile(f.vault.files().entryPath(name))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(data, &e); err != nil {
		f.t.Fatal(err)
	}
	return e.OAuth
}

var ctx = context.Background()

func TestUseSavesTheLiveLoginThenSwitchesTheBox(t *testing.T) {
	f := newFixture(t)
	f.seed("work", f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	// Claude Code rotated the work login since it was saved.
	f.setLive(f.login("work", 2, time.Hour, 20*24*time.Hour), account("work"))
	f.seed("home", f.login("home", 1, time.Hour, 20*24*time.Hour), account("home"))

	switched, err := f.vault.Use(ctx, "home")
	if err != nil || !switched {
		t.Fatalf("Use = %v, %v", switched, err)
	}
	if got := f.entryTokens("work"); got.RefreshToken != "fake-refresh-work-2" {
		t.Fatalf("work entry holds %s, not the live login", got.RefreshToken)
	}
	if got := f.liveTokens(); got.RefreshToken != "fake-refresh-home-1" || f.liveEmail() != "home@example.com" {
		t.Fatalf("live is %s for %s", got.RefreshToken, f.liveEmail())
	}
	if switched, err := f.vault.Use(ctx, "home"); err != nil || switched {
		t.Fatalf("Use of the active account = %v, %v", switched, err)
	}
	for _, p := range []string{f.vault.Dir, f.vault.files().entryPath("work")} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]os.FileMode{true: 0o700, false: 0o600}[info.IsDir()]; info.Mode().Perm() != want {
			t.Fatalf("%s mode %v", p, info.Mode())
		}
	}
}

func TestUseRefusesToDropAnUnnamedLogin(t *testing.T) {
	f := newFixture(t)
	f.setLive(f.login("stray", 1, time.Hour, 20*24*time.Hour), account("stray"))
	f.seed("home", f.login("home", 1, time.Hour, 20*24*time.Hour), account("home"))
	if _, err := f.vault.Use(ctx, "home"); err == nil || !strings.Contains(err.Error(), "logged in to stray@example.com; keep that login") {
		t.Fatalf("err = %v", err)
	}
	if f.liveTokens().RefreshToken != "fake-refresh-stray-1" {
		t.Fatal("live login replaced")
	}
	if err := f.vault.Save(ctx, "stray"); err != nil {
		t.Fatal(err)
	}
	if err := f.vault.Save(ctx, "stray"); err != nil {
		t.Fatalf("saving again: %v", err)
	}
	if err := f.vault.Save(ctx, "other"); err == nil || !strings.Contains(err.Error(), "saved as stray already") {
		t.Fatalf("second name: %v", err)
	}
	if _, err := f.vault.Use(ctx, "home"); err != nil {
		t.Fatal(err)
	}
}

func TestUseRefreshesAnExpiredTargetAndRefusesASignedOutOne(t *testing.T) {
	f := newFixture(t)
	f.seed("work", f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	f.setLive(f.login("work", 2, time.Hour, 20*24*time.Hour), account("work"))
	f.seed("home", f.login("home", 1, -time.Hour, 20*24*time.Hour), account("home"))
	f.seed("gone", `{"accessToken":"fake-access-gone","refreshToken":"fake-refresh-unknown","expiresAt":0}`, account("gone"))

	if _, err := f.vault.Use(ctx, "gone"); err == nil || !strings.Contains(err.Error(), "account add gone") {
		t.Fatalf("err = %v", err)
	}
	if f.liveTokens().RefreshToken != "fake-refresh-work-2" {
		t.Fatal("switched to a signed-out account")
	}
	if _, err := f.vault.Use(ctx, "home"); err != nil {
		t.Fatal(err)
	}
	live := f.liveTokens()
	if live.RefreshToken != "fake-refresh-home-101" || f.entryTokens("home") != live {
		t.Fatalf("live %+v, entry %+v", live, f.entryTokens("home"))
	}
}

func TestTheServerSaysWhichEntryTheLiveLoginIs(t *testing.T) {
	f := newFixture(t)
	f.seed("work", f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	f.seed("home", f.login("home", 1, time.Hour, 20*24*time.Hour), account("home"))
	// The box runs home's rotated login, but the account record says work: a
	// switch cut short, or a session writing back a stale profile read.
	f.setLive(f.login("home", 2, time.Hour, 20*24*time.Hour), account("work"))
	l, err := f.vault.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if l.Active() != "home" || f.liveEmail() != "home@example.com" {
		t.Fatalf("active %q, record %s", l.Active(), f.liveEmail())
	}
	if f.entryTokens("home").RefreshToken != "fake-refresh-home-2" || f.entryTokens("work").RefreshToken != "fake-refresh-work-1" {
		t.Fatalf("home %+v, work %+v", f.entryTokens("home"), f.entryTokens("work"))
	}
}

func TestInactiveAccountsRefreshWithoutClaudeCodesLock(t *testing.T) {
	f := newFixture(t)
	f.setLive(f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	f.seed("work", f.login("work", 0, time.Hour, 20*24*time.Hour), account("work"))
	f.seed("home", f.login("home", 1, -time.Hour, 20*24*time.Hour), account("home"))
	var held []bool
	f.api.onRefresh = func() {
		_, err := os.Stat(filepath.Join(f.paths.SecureDir, ".oauth_refresh.lock"))
		held = append(held, err == nil)
	}
	if _, err := f.vault.Usage(ctx); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(held) != "[false]" {
		t.Fatalf("Claude Code's lock held during the refreshes: %v", held)
	}
}

func TestASignedOutBoxLoginCanBeReplaced(t *testing.T) {
	f := newFixture(t)
	f.seed("work", f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	// Claude Code cleared the login after the server refused it, and kept its record.
	f.setLive(`{"accessToken":"","refreshToken":"","expiresAt":0}`, account("work"))
	signIn := func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":`+f.login("work", 5, time.Hour, 20*24*time.Hour)+`}`), 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount":`+account("work")+`}`), 0o600)
	}
	if err := addSaved(f, "work", signIn); err != nil {
		t.Fatal(err)
	}
	if switched, err := f.vault.Use(ctx, "work"); err != nil || !switched {
		t.Fatalf("Use = %v, %v", switched, err)
	}
	if f.liveTokens().RefreshToken != "fake-refresh-work-5" {
		t.Fatalf("live %+v", f.liveTokens())
	}
}

func TestUsageRefreshesOnlyInactiveAccounts(t *testing.T) {
	f := newFixture(t)
	f.setLive(f.login("work", 2, time.Hour, 20*24*time.Hour), account("work"))
	f.seed("work", f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	f.seed("home", f.login("home", 1, -time.Minute, 20*24*time.Hour), account("home"))
	f.seed("gone", `{"accessToken":"x","refreshToken":"fake-refresh-unknown","expiresAt":0}`, account("gone"))

	l, err := f.vault.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"accounts":[` +
		`{"name":"gone","email":"gone@example.com","active":false,"five_hour":null,"seven_day":null,"error":"signed out: log in again with ` + "`rclaude account add gone`" + `"},` +
		`{"name":"home","email":"home@example.com","active":false,"five_hour":{"utilization":40,"resets_at":"2026-11-03T15:00:00Z"},"seven_day":null},` +
		`{"name":"work","email":"work@example.com","active":true,"five_hour":{"utilization":40,"resets_at":"2026-11-03T15:00:00Z"},"seven_day":null}]}`
	if string(got) != want {
		t.Fatalf("listing\n%s\nwant\n%s", got, want)
	}
	if live := f.liveTokens(); live.RefreshToken != "fake-refresh-work-2" || f.entryTokens("work") != live {
		t.Fatalf("live %+v, work entry %+v", live, f.entryTokens("work"))
	}
	if f.entryTokens("home").RefreshToken != "fake-refresh-home-101" {
		t.Fatalf("home entry %+v", f.entryTokens("home"))
	}
	// Nothing expired now, so a second listing refreshes nothing.
	before := len(f.api.refreshes)
	if _, err := f.vault.Usage(ctx); err != nil || len(f.api.refreshes) != before {
		t.Fatalf("second listing refreshed %v: %v", f.api.refreshes[before:], err)
	}
}

func TestAnUnverifiedBoxLoginIsNeverRefreshed(t *testing.T) {
	cases := map[string]struct {
		// prepare leaves the box's login unverifiable and returns whom the vault should take it for.
		prepare  func(f *fixture) string
		wantRows string
	}{
		// The access token expired overnight; the record and the last switch agree.
		"guessed": {func(f *fixture) string {
			f.setLive(f.login("work", 2, -time.Hour, 20*24*time.Hour), account("work"))
			f.markLast("work")
			return "work"
		}, "home:40 work*:unverified"},
		// The server refused an unexpired token, which an entry still holds.
		"proven": {func(f *fixture) string {
			raw := f.login("work", 1, time.Hour, 20*24*time.Hour)
			f.seed("work", raw, account("work"))
			f.setLive(raw, account("work"))
			f.api.revoke("fake-access-work-1")
			return "work"
		}, "home:40 work*:unverified"},
		// Nothing agrees: no saved login may be refreshed, since any may be a copy of it.
		"unknown": {func(f *fixture) string {
			f.setLive(f.login("work", 2, -time.Hour, 20*24*time.Hour), account("work"))
			f.markLast("home")
			return ""
		}, "*:unverified home:not refreshed work:40"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			// Both are due for KeepFresh, which refreshes only those that cannot be the box's.
			f.seed("work", f.login("work", 1, time.Hour, 3*24*time.Hour), account("work"))
			f.seed("home", f.login("home", 1, -time.Minute, 3*24*time.Hour), account("home"))
			taken := c.prepare(f)
			l, err := f.vault.Usage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := rowSummary(l); got != c.wantRows {
				t.Fatalf("rows %q, want %q", got, c.wantRows)
			}
			if l.Active() != taken {
				t.Fatalf("active %q, want %q", l.Active(), taken)
			}
			var log bytes.Buffer
			err = f.vault.KeepFresh(ctx, &log)
			if unknown := taken == ""; unknown != (err != nil) {
				t.Fatalf("KeepFresh = %v", err)
			}
		})
	}
}

func TestAnEntryHoldingTheBoxsLoginIsNeverRefreshed(t *testing.T) {
	f := newFixture(t)
	// A guessed save went to the wrong entry, and the switch after it failed.
	box := f.login("box", 1, time.Hour, 3*24*time.Hour)
	f.seed("work", box, account("work"))
	f.setLive(box, account("box"))
	var log bytes.Buffer
	if err := f.vault.KeepFresh(ctx, &log); err != nil {
		t.Fatal(err)
	}
	if _, err := f.vault.Use(ctx, "work"); err == nil || !strings.Contains(err.Error(), "account work holds a copy of the box's login") {
		t.Fatalf("Use = %v", err)
	}
	if _, err := f.vault.Add(ctx, "work", signInAs("work", f.login("work", 2, time.Hour, 20*24*time.Hour))); err == nil || !strings.Contains(err.Error(), "account work holds a copy of the box's login") {
		t.Fatalf("Add = %v", err)
	}
}

func TestAnIncompleteEntryIsRefused(t *testing.T) {
	f := newFixture(t)
	f.setLive(f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	if err := f.vault.files().ensureDir(); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"claudeAiOauth":` + f.login("home", 1, time.Hour, 20*24*time.Hour) + `}`,
		`{"oauthAccount":` + account("home") + `,"claudeAiOauth":null}`,
		`{"claudeAiOauth":` + f.login("home", 2, time.Hour, 20*24*time.Hour) + `,"oauthAccount":` + account("home") + `,"x":1}`,
		`{"claudeAiOauth":` + f.login("home", 3, time.Hour, 20*24*time.Hour) + `,"oauthAccount":` + account("home") + `} {}`,
	} {
		if err := os.WriteFile(f.vault.files().entryPath("home"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.vault.Usage(ctx); err == nil || !strings.Contains(err.Error(), "account home") {
			t.Errorf("%.40s...: %v", body, err)
		}
	}
}

// rowSummary is each row as name[*]:<5-hour %> or :<first word of a reason it has none>.
func rowSummary(l Listing) string {
	var out []string
	for _, r := range l.Accounts {
		s := r.Name
		if r.Active {
			s += "*"
		}
		switch {
		case strings.HasPrefix(r.Error, "not refreshed"):
			s += ":not refreshed"
		case strings.Contains(r.Error, "unverified"):
			s += ":unverified"
		case r.Error != "":
			s += ":" + r.Error
		case r.FiveHour != nil:
			s += fmt.Sprintf(":%.0f", r.FiveHour.Utilization)
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func TestSwitchingAwayFromAnUnverifiedLogin(t *testing.T) {
	t.Run("guessed and not older than its entry: kept there, then switched", func(t *testing.T) {
		f := newFixture(t)
		f.seed("work", f.login("work", 1, -2*time.Hour, 20*24*time.Hour), account("work"))
		f.seed("home", f.login("home", 1, time.Hour, 20*24*time.Hour), account("home"))
		f.setLive(f.login("work", 2, -time.Hour, 20*24*time.Hour), account("work"))
		f.markLast("work")
		if switched, err := f.vault.Use(ctx, "home"); err != nil || !switched {
			t.Fatalf("Use = %v, %v", switched, err)
		}
		if f.entryTokens("work").RefreshToken != "fake-refresh-work-2" || f.liveTokens().RefreshToken != "fake-refresh-home-1" {
			t.Fatalf("work %+v, live %+v", f.entryTokens("work"), f.liveTokens())
		}
	})
	t.Run("guessed but older than its entry: refused", func(t *testing.T) {
		f := newFixture(t)
		f.seed("work", f.login("work", 3, -time.Minute, 20*24*time.Hour), account("work"))
		f.seed("home", f.login("home", 1, time.Hour, 20*24*time.Hour), account("home"))
		f.setLive(f.login("work", 2, -time.Hour, 20*24*time.Hour), account("work"))
		f.markLast("work")
		if _, err := f.vault.Use(ctx, "home"); err == nil || !strings.Contains(err.Error(), "account work holds a newer login") {
			t.Fatalf("err = %v", err)
		}
		if f.entryTokens("work").RefreshToken != "fake-refresh-work-3" || f.liveTokens().RefreshToken != "fake-refresh-work-2" {
			t.Fatalf("work %+v, live %+v", f.entryTokens("work"), f.liveTokens())
		}
	})
	t.Run("unknown: refused", func(t *testing.T) {
		f := newFixture(t)
		f.seed("home", f.login("home", 1, time.Hour, 20*24*time.Hour), account("home"))
		f.setLive(f.login("work", 2, -time.Hour, 20*24*time.Hour), account("work"))
		if _, err := f.vault.Use(ctx, "home"); err == nil || !strings.Contains(err.Error(), "no saved account is known to be it") {
			t.Fatalf("err = %v", err)
		}
		if err := f.vault.Remove(ctx, "home"); err == nil || !strings.Contains(err.Error(), "may be, the box's login") {
			t.Fatalf("remove: %v", err)
		}
	})
}

func TestASwitchThatWroteOnlyTheLoginIsNeverGuessed(t *testing.T) {
	f := newFixture(t)
	cfg := filepath.Dir(f.paths.SecureDir) + "/cfg"
	if err := os.Mkdir(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	f.paths.GlobalConfig = cfg + "/.claude.json"
	f.vault.Store.Paths = f.paths
	f.seed("work", f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	f.seed("home", f.login("home", 1, time.Hour, 20*24*time.Hour), account("home"))
	f.setLive(f.login("work", 2, time.Hour, 20*24*time.Hour), account("work"))
	f.markLast("work")
	// The account write fails after the login write: its lock cannot be taken.
	if err := os.Chmod(cfg, 0o555); err != nil {
		t.Fatal(err)
	}
	if _, err := f.vault.Use(ctx, "home"); err == nil {
		t.Fatal("Use succeeded")
	}
	if err := os.Chmod(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if f.liveTokens().RefreshToken != "fake-refresh-home-1" || f.liveEmail() != "work@example.com" {
		t.Fatalf("live %+v %s", f.liveTokens(), f.liveEmail())
	}
	// A session then rotates the login, and its access token is refused.
	f.setLive(f.login("home", 2, 2*time.Hour, 20*24*time.Hour), account("work"))
	f.api.revoke("fake-access-home-2")
	if _, err := f.vault.Use(ctx, "home"); err == nil || !strings.Contains(err.Error(), "no saved account is known to be it") {
		t.Fatalf("err = %v", err)
	}
	if f.entryTokens("work").RefreshToken != "fake-refresh-work-2" {
		t.Fatalf("work %+v", f.entryTokens("work"))
	}
}

func TestUseRefusesALoginThatChangedWhileItWorked(t *testing.T) {
	f := newFixture(t)
	f.seed("work", f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	f.setLive(f.login("work", 2, time.Hour, 20*24*time.Hour), account("work"))
	f.seed("home", f.login("home", 1, -time.Hour, 20*24*time.Hour), account("home"))
	// A session refreshes the box's login while the switch refreshes home.
	f.api.onRefresh = func() {
		f.api.onRefresh = nil
		f.setLive(f.login("work", 3, time.Hour, 20*24*time.Hour), account("work"))
	}
	if _, err := f.vault.Use(ctx, "home"); !errors.Is(err, claudeauth.ErrLoginChanged) {
		t.Fatalf("err = %v", err)
	}
	if f.liveTokens().RefreshToken != "fake-refresh-work-3" {
		t.Fatalf("live %+v", f.liveTokens())
	}
	// Run again, the switch keeps the session's newer login.
	if _, err := f.vault.Use(ctx, "home"); err != nil {
		t.Fatal(err)
	}
	if f.entryTokens("work").RefreshToken != "fake-refresh-work-3" {
		t.Fatalf("work %+v", f.entryTokens("work"))
	}
}

func TestUsageShowsAnUnnamedLiveLogin(t *testing.T) {
	f := newFixture(t)
	f.setLive(f.login("stray", 1, time.Hour, 20*24*time.Hour), account("stray"))
	l, err := f.vault.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Accounts) != 1 || l.Accounts[0].Name != "" || !l.Accounts[0].Active || l.Accounts[0].FiveHour == nil || l.Active() != "" || len(l.Named()) != 0 {
		t.Fatalf("listing %+v", l)
	}
}

func TestAddSavesANewLoginAndRefusesAMismatch(t *testing.T) {
	f := newFixture(t)
	f.setLive(f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	if err := f.vault.Save(ctx, "work"); err != nil {
		t.Fatal(err)
	}
	signIn := func(tag string, n int) func(string) error {
		return func(dir string) error {
			if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":`+f.login(tag, n, time.Hour, 20*24*time.Hour)+`}`), 0o600); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount":`+account(tag)+`}`), 0o600)
		}
	}
	if err := addSaved(f, "home", signIn("home", 1)); err != nil {
		t.Fatal(err)
	}
	if f.entryTokens("home").RefreshToken != "fake-refresh-home-1" {
		t.Fatalf("home entry %+v", f.entryTokens("home"))
	}
	// Signing in again replaces a login the server stopped honouring.
	if err := addSaved(f, "home", signIn("home", 2)); err != nil || f.entryTokens("home").RefreshToken != "fake-refresh-home-2" {
		t.Fatalf("re-adding: %v, %+v", err, f.entryTokens("home"))
	}
	if err := addSaved(f, "home", signIn("other", 1)); err == nil || !strings.Contains(err.Error(), "home is home@example.com, not other@example.com") {
		t.Fatalf("mismatch: %v", err)
	}
	if err := addSaved(f, "again", signIn("home", 3)); err == nil || !strings.Contains(err.Error(), "account home already") {
		t.Fatalf("duplicate: %v", err)
	}
	if err := addSaved(f, "x", func(string) error { return nil }); err == nil || !strings.Contains(err.Error(), "no Claude login") {
		t.Fatalf("empty sign-in: %v", err)
	}
	if err := addSaved(f, "x", func(string) error { return fmt.Errorf("cancelled") }); err == nil || err.Error() != "cancelled" {
		t.Fatalf("failed sign-in: %v", err)
	}
	if added, err := f.vault.Add(ctx, "work", signIn("work", 9)); err != nil || added != AddedAlready || f.liveTokens().RefreshToken != "fake-refresh-work-1" {
		t.Fatalf("adding the active account: %v, %v", added, err)
	}
	dirents, err := os.ReadDir(f.vault.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirents {
		if strings.HasPrefix(d.Name(), ".login-") {
			t.Fatalf("sign-in dir %s left", d.Name())
		}
	}
}

// addSaved adds an account whose new login must be saved.
func addSaved(f *fixture, name string, signIn func(string) error) error {
	added, err := f.vault.Add(ctx, name, signIn)
	if err == nil && added != AddedNew {
		return fmt.Errorf("added %v", added)
	}
	return err
}

// signInAs is a sign-in that leaves login and the account record of tag behind.
func signInAs(tag, login string) func(string) error {
	return func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":`+login+`}`), 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount":`+account(tag)+`}`), 0o600)
	}
}

func TestAddingTheBoxsOwnAccount(t *testing.T) {
	t.Run("unnamed: the box's login is saved", func(t *testing.T) {
		f := newFixture(t)
		f.setLive(f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
		if added, err := f.vault.Add(ctx, "work", signInAs("work", f.login("work", 9, time.Hour, 20*24*time.Hour))); err != nil || added != AddedLive {
			t.Fatalf("Add = %v, %v", added, err)
		}
		if f.entryTokens("work").RefreshToken != "fake-refresh-work-1" || f.liveTokens().RefreshToken != "fake-refresh-work-1" {
			t.Fatalf("work %+v, live %+v", f.entryTokens("work"), f.liveTokens())
		}
	})
	t.Run("an entry that may be the unverified box login: refused", func(t *testing.T) {
		f := newFixture(t)
		f.seed("work", f.login("work", 1, -time.Hour, 20*24*time.Hour), account("work"))
		f.setLive(f.login("work", 1, -time.Hour, 20*24*time.Hour), account("work"))
		if _, err := f.vault.Add(ctx, "work", signInAs("work", f.login("work", 9, time.Hour, 20*24*time.Hour))); err == nil || !strings.Contains(err.Error(), "account work may be the box's login") {
			t.Fatalf("proven: %v", err)
		}
		if f.entryTokens("work").RefreshToken != "fake-refresh-work-1" {
			t.Fatalf("work %+v", f.entryTokens("work"))
		}
	})
	t.Run("unknown box login: existing names refused, new ones saved", func(t *testing.T) {
		f := newFixture(t)
		f.seed("home", f.login("home", 1, time.Hour, 20*24*time.Hour), account("home"))
		f.setLive(f.login("other", 1, -time.Hour, 20*24*time.Hour), account("other"))
		if _, err := f.vault.Add(ctx, "home", signInAs("home", f.login("home", 2, time.Hour, 20*24*time.Hour))); err == nil || !strings.Contains(err.Error(), "account home may be the box's login") {
			t.Fatalf("unknown: %v", err)
		}
		if err := addSaved(f, "work", signInAs("work", f.login("work", 1, time.Hour, 20*24*time.Hour))); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAddRemovesSignInDirsACrashLeft(t *testing.T) {
	f := newFixture(t)
	if err := f.vault.files().ensureDir(); err != nil {
		t.Fatal(err)
	}
	old, recent := filepath.Join(f.vault.Dir, ".login-old"), filepath.Join(f.vault.Dir, ".login-recent")
	for _, dir := range []string{old, recent} {
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	twoHoursAgo := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, twoHoursAgo, twoHoursAgo); err != nil {
		t.Fatal(err)
	}
	if err := addSaved(f, "work", signInAs("work", f.login("work", 1, time.Hour, 20*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old sign-in dir: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("a sign-in in progress lost its dir: %v", err)
	}
}

func TestAddReportsALoginDirItCouldNotRemove(t *testing.T) {
	f := newFixture(t)
	f.setLive(f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	if err := f.vault.Save(ctx, "work"); err != nil {
		t.Fatal(err)
	}
	// The sign-in leaves a file the vault's cleanup can't remove, in a dir it can't enter.
	signIn := func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":`+f.login("work", 9, time.Hour, 20*24*time.Hour)+`}`), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount":`+account("work")+`}`), 0o600); err != nil {
			return err
		}
		stuck := filepath.Join(dir, "stuck")
		if err := os.MkdirAll(filepath.Join(stuck, "x"), 0o700); err != nil {
			return err
		}
		t.Cleanup(func() { os.Chmod(stuck, 0o700) })
		return os.Chmod(stuck, 0o500)
	}
	if _, err := f.vault.Add(ctx, "work", signIn); err == nil {
		t.Fatal("Add succeeded: a failed cleanup was hidden")
	}
}

func TestRemoveKeepsTheActiveAccount(t *testing.T) {
	f := newFixture(t)
	f.setLive(f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	f.seed("work", f.login("work", 0, time.Hour, 20*24*time.Hour), account("work"))
	f.seed("home", f.login("home", 1, time.Hour, 20*24*time.Hour), account("home"))
	if err := f.vault.Remove(ctx, "work"); err == nil || !strings.Contains(err.Error(), "switch to another one first") {
		t.Fatalf("err = %v", err)
	}
	if err := f.vault.Remove(ctx, "home"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.vault.files().entryPath("home")); !os.IsNotExist(err) {
		t.Fatalf("home entry left: %v", err)
	}
	if err := f.vault.Remove(ctx, "home"); err == nil {
		t.Fatal("removed a missing account")
	}
}

func TestKeepFreshRefreshesOnlyInactiveLoginsNearTheirExpiry(t *testing.T) {
	f := newFixture(t)
	f.setLive(f.login("work", 1, time.Hour, 3*24*time.Hour), account("work"))
	f.seed("work", f.login("work", 0, time.Hour, 3*24*time.Hour), account("work"))
	f.seed("idle", f.login("idle", 1, -time.Hour, 6*24*time.Hour), account("idle"))
	f.seed("recent", f.login("recent", 1, -time.Hour, 20*24*time.Hour), account("recent"))
	f.seed("gone", `{"accessToken":"x","refreshToken":"fake-refresh-unknown","expiresAt":0}`, account("gone"))
	var log bytes.Buffer
	err := f.vault.KeepFresh(ctx, &log)
	if err == nil || !strings.Contains(err.Error(), "account gone: signed out") {
		t.Fatalf("err = %v", err)
	}
	if fmt.Sprint(f.api.refreshes) != "[idle]" {
		t.Fatalf("refreshed %v", f.api.refreshes)
	}
	if live := f.liveTokens(); f.entryTokens("work") != live || f.entryTokens("idle").RefreshToken != "fake-refresh-idle-101" {
		t.Fatalf("live %+v, work %+v, idle %+v", live, f.entryTokens("work"), f.entryTokens("idle"))
	}
	if log.String() != "refreshed account idle\n" {
		t.Fatalf("log %q", log.String())
	}
}

func TestKeepFreshWithNoAccountsLeavesTheBoxAlone(t *testing.T) {
	f := newFixture(t)
	f.setLive(f.login("stray", 1, time.Hour, 24*time.Hour), account("stray"))
	if err := f.vault.KeepFresh(ctx, io.Discard); err != nil || len(f.api.refreshes) != 0 || f.api.profileReads != 0 {
		t.Fatalf("KeepFresh = %v, refreshed %v, profile reads %d", err, f.api.refreshes, f.api.profileReads)
	}
}

func TestRender(t *testing.T) {
	reset := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)
	week := time.Date(2026, 11, 9, 23, 59, 0, 0, time.UTC)
	l := Listing{Accounts: []Row{
		{Name: "", Email: "stray@example.com", Active: true, FiveHour: &Window{Utilization: 3, ResetsAt: &reset}},
		{Name: "home", Email: "home@example.com", SevenDay: &Window{Utilization: 49.4, ResetsAt: &week}},
		{Name: "work", Email: "work@example.com", Error: "signed out"},
	}}
	var out bytes.Buffer
	if err := Render(&out, l, t0, "rclaude"); err != nil {
		t.Fatal(err)
	}
	want := `   ACCOUNT    EMAIL              5-HOUR          WEEKLY
*  (no name)  stray@example.com  3% until 15:00  -
1  home       home@example.com   -               49% until Nov 9 23:59
2  work       work@example.com   error: signed out
The box's login has no name: keep it with ` + "`rclaude account save <name>`" + `.
`
	if out.String() != want {
		t.Fatalf("rendered\n%s\nwant\n%s", out.String(), want)
	}
	// An unverified login cannot be saved, so the hint stays away.
	out.Reset()
	if err := Render(&out, Listing{Accounts: []Row{{Active: true, Error: "unverified"}}}, t0, "rcodex"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "account save") {
		t.Fatalf("rendered\n%s", out.String())
	}
}
