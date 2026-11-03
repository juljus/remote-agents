package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	mu        sync.Mutex
	valid     map[string]string // refresh token -> account tag
	usage     map[string]string // access token -> usage reply
	next      int
	refreshes []string // tags refreshed, in order
}

func newFakeAnthropic(t *testing.T) (*fakeAnthropic, claudeauth.Endpoints) {
	f := &fakeAnthropic{valid: map[string]string{}, usage: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
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

// issue registers a token pair for tag; the caller holds mu.
func (f *fakeAnthropic) issue(tag string, n int) (access, refresh string) {
	access, refresh = fmt.Sprintf("fake-access-%s-%d", tag, n), fmt.Sprintf("fake-refresh-%s-%d", tag, n)
	f.valid[refresh] = tag
	f.usage[access] = fmt.Sprintf(`{"five_hour":{"utilization":%d,"resets_at":"2026-11-03T15:00:00+00:00"},"seven_day":null}`, len(tag)*10)
	return access, refresh
}

type fixture struct {
	t     *testing.T
	api   *fakeAnthropic
	paths claudeauth.Paths
	vault Vault
}

func newFixture(t *testing.T) *fixture {
	home := t.TempDir()
	api, ep := newFakeAnthropic(t)
	paths := claudeauth.Paths{ConfigDir: home + "/.claude", SecureDir: home + "/.claude", GlobalConfig: home + "/.claude.json"}
	store := claudeauth.Store{Paths: paths, Endpoints: ep, Client: http.DefaultClient, Now: func() time.Time { return t0 }}
	f := &fixture{t: t, api: api, paths: paths, vault: Vault{Dir: home + "/.config/remote-agents/accounts/claude", Store: store}}
	if err := os.MkdirAll(paths.SecureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.GlobalConfig, []byte("{\n  \"numStartups\": 1\n}"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if err := f.vault.ensureDir(); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.vault.entryPath(name), []byte(`{"claudeAiOauth":`+login+`,"oauthAccount":`+acct+`}`), 0o600); err != nil {
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
	data, err := os.ReadFile(f.vault.entryPath(name))
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
	if _, err := os.Stat(f.vault.journalPath()); !os.IsNotExist(err) {
		t.Fatalf("journal left: %v", err)
	}
	if switched, err := f.vault.Use(ctx, "home"); err != nil || switched {
		t.Fatalf("Use of the active account = %v, %v", switched, err)
	}
	for _, p := range []string{f.vault.Dir, f.vault.entryPath("work")} {
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
	if _, err := f.vault.Use(ctx, "home"); err == nil || !strings.Contains(err.Error(), "stray@example.com under no name") {
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
	if err := f.vault.Save(ctx, "other"); err == nil || !strings.Contains(err.Error(), "already saved as stray") {
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

func TestAnInterruptedSwitchIsFinishedByTheNextCommand(t *testing.T) {
	cases := map[string]struct {
		live     func(f *fixture) string
		wantLive string
	}{
		"before the credentials": {func(f *fixture) string { return f.login("work", 2, time.Hour, 20*24*time.Hour) }, "fake-refresh-home-1"},
		// Claude Code refreshed the home login it already had.
		"after the credentials": {func(f *fixture) string { return f.login("home", 7, time.Hour, 20*24*time.Hour) }, "fake-refresh-home-7"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			live := c.live(f)
			work := f.login("work", 2, time.Hour, 20*24*time.Hour)
			if strings.Contains(live, "work") {
				work = live
			}
			f.seed("work", work, account("work"))
			f.seed("home", f.login("home", 1, time.Hour, 20*24*time.Hour), account("home"))
			f.setLive(live, account("work"))
			if err := f.vault.writeJournal(journal{From: "work", To: "home"}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.vault.Usage(ctx); err != nil {
				t.Fatal(err)
			}
			if got := f.liveTokens().RefreshToken; got != c.wantLive || f.liveEmail() != "home@example.com" {
				t.Fatalf("live %s for %s", got, f.liveEmail())
			}
			if f.entryTokens("work").RefreshToken != "fake-refresh-work-2" {
				t.Fatalf("work entry %+v", f.entryTokens("work"))
			}
		})
	}
}

func TestUsageListsEveryAccountRefreshingWhatExpired(t *testing.T) {
	f := newFixture(t)
	f.seed("work", f.login("work", 1, time.Hour, 20*24*time.Hour), account("work"))
	f.setLive(f.login("work", 2, -time.Minute, 20*24*time.Hour), account("work"))
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
	if live := f.liveTokens(); live.RefreshToken != "fake-refresh-work-102" || f.entryTokens("work") != live {
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
	if err := f.vault.Add(ctx, "home", signIn("home", 1)); err != nil {
		t.Fatal(err)
	}
	if f.entryTokens("home").RefreshToken != "fake-refresh-home-1" {
		t.Fatalf("home entry %+v", f.entryTokens("home"))
	}
	// Signing in again replaces a login the server stopped honouring.
	if err := f.vault.Add(ctx, "home", signIn("home", 2)); err != nil || f.entryTokens("home").RefreshToken != "fake-refresh-home-2" {
		t.Fatalf("re-adding: %v, %+v", err, f.entryTokens("home"))
	}
	if err := f.vault.Add(ctx, "home", signIn("other", 1)); err == nil || !strings.Contains(err.Error(), "home is home@example.com, not other@example.com") {
		t.Fatalf("mismatch: %v", err)
	}
	if err := f.vault.Add(ctx, "again", signIn("home", 3)); err == nil || !strings.Contains(err.Error(), "account home already") {
		t.Fatalf("duplicate: %v", err)
	}
	if err := f.vault.Add(ctx, "x", func(string) error { return nil }); err == nil || !strings.Contains(err.Error(), "no Claude login") {
		t.Fatalf("empty sign-in: %v", err)
	}
	if err := f.vault.Add(ctx, "x", func(string) error { return fmt.Errorf("cancelled") }); err == nil || err.Error() != "cancelled" {
		t.Fatalf("failed sign-in: %v", err)
	}
	if err := f.vault.Add(ctx, "work", signIn("work", 9)); err != nil || f.liveTokens().RefreshToken != "fake-refresh-work-1" {
		t.Fatalf("adding the active account: %v", err)
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
	if _, err := os.Stat(f.vault.entryPath("home")); !os.IsNotExist(err) {
		t.Fatalf("home entry left: %v", err)
	}
	if err := f.vault.Remove(ctx, "home"); err == nil {
		t.Fatal("removed a missing account")
	}
}

func TestKeepFreshRefreshesOnlyLoginsNearTheirExpiry(t *testing.T) {
	f := newFixture(t)
	f.setLive(f.login("work", 1, time.Hour, 3*24*time.Hour), account("work"))
	f.seed("work", f.login("work", 0, time.Hour, 3*24*time.Hour), account("work"))
	f.seed("idle", f.login("idle", 1, -time.Hour, 6*24*time.Hour), account("idle"))
	f.seed("recent", f.login("recent", 1, -time.Hour, 20*24*time.Hour), account("recent"))
	f.seed("gone", `{"accessToken":"x","refreshToken":"fake-refresh-unknown","expiresAt":0}`, account("gone"))
	var log bytes.Buffer
	err := f.vault.KeepFresh(ctx, &log)
	if err == nil || !strings.Contains(err.Error(), "account gone: ") {
		t.Fatalf("err = %v", err)
	}
	if fmt.Sprint(f.api.refreshes) != "[work idle]" {
		t.Fatalf("refreshed %v", f.api.refreshes)
	}
	if live := f.liveTokens(); f.entryTokens("work") != live || f.entryTokens("idle").RefreshToken != "fake-refresh-idle-102" {
		t.Fatalf("live %+v, work %+v, idle %+v", live, f.entryTokens("work"), f.entryTokens("idle"))
	}
	if log.String() != "refreshed the live login\nrefreshed account idle\n" {
		t.Fatalf("log %q", log.String())
	}
}

func TestKeepFreshLeavesAnUnnamedLiveLoginAlone(t *testing.T) {
	f := newFixture(t)
	f.setLive(f.login("stray", 1, time.Hour, 24*time.Hour), account("stray"))
	if err := f.vault.KeepFresh(ctx, io.Discard); err != nil || len(f.api.refreshes) != 0 {
		t.Fatalf("KeepFresh = %v, refreshed %v", err, f.api.refreshes)
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
	if err := Render(&out, l, t0); err != nil {
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
}

