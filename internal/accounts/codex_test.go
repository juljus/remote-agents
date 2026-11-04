package accounts

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"remote-agents/internal/codexauth"
	"remote-agents/internal/codexauth/codexfake"
)

type codexFixture struct {
	t     *testing.T
	api   *codexfake.Server
	paths codexauth.Paths
	vault CodexVault
	// live holds every refresh token the box's login had: none may ever be presented.
	live []string
}

func newCodexFixture(t *testing.T) *codexFixture {
	home := t.TempDir()
	now := func() time.Time { return t0 }
	api := codexfake.NewServer(t, now)
	paths := codexauth.Paths{Home: filepath.Join(home, ".codex")}
	store := codexauth.Store{Paths: paths, Endpoints: codexauth.Endpoints{TokenOverride: api.URL + "/oauth/token", APIBase: api.URL + "/backend-api"}, Client: http.DefaultClient, Now: now}
	f := &codexFixture{t: t, api: api, paths: paths, vault: CodexVault{Dir: filepath.Join(home, ".config/remote-agents/accounts/codex"), Store: store}}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, rt := range api.PresentedAny(f.live) {
			t.Errorf("refreshed the box's login %s", rt)
		}
	})
	return f
}

func (f *codexFixture) setLive(tag string, n int, expires time.Time) {
	f.t.Helper()
	f.live = append(f.live, codexfake.RefreshToken(tag, n))
	f.writeLive(codexfake.Login(tag, n, expires))
}

func (f *codexFixture) writeLive(data string) {
	f.t.Helper()
	if err := os.WriteFile(f.paths.AuthFile(), []byte(data), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *codexFixture) seed(name, tag string, n int, expires time.Time) {
	f.t.Helper()
	if err := f.vault.files().ensureDir(); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.vault.files().entryPath(name), []byte(codexfake.Login(tag, n, expires)), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

type codexTokens struct {
	Tokens struct {
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
}

func readRefreshToken(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v codexTokens
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v.Tokens.RefreshToken
}

func (f *codexFixture) liveRefresh() string { return readRefreshToken(f.t, f.paths.AuthFile()) }
func (f *codexFixture) entryRefresh(name string) string {
	return readRefreshToken(f.t, f.vault.files().entryPath(name))
}

var later = t0.Add(48 * time.Hour)

func TestCodexUseSavesTheLiveLoginThenSwitches(t *testing.T) {
	f := newCodexFixture(t)
	f.seed("work", "work", 1, later)
	f.seed("home", "home", 1, later)
	f.setLive("work", 2, later)
	if switched, err := f.vault.Use(ctx, "home"); err != nil || !switched {
		t.Fatalf("Use = %v, %v", switched, err)
	}
	if f.entryRefresh("work") != "fake-refresh-work-2" || f.liveRefresh() != "fake-refresh-home-1" {
		t.Fatalf("work %s, live %s", f.entryRefresh("work"), f.liveRefresh())
	}
	if info, err := os.Stat(f.paths.AuthFile()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("auth.json mode: %v", err)
	}
	if switched, err := f.vault.Use(ctx, "home"); err != nil || switched {
		t.Fatalf("again: %v, %v", switched, err)
	}
	if _, err := f.vault.Use(ctx, "nobody"); err == nil {
		t.Fatal("switched to no account")
	}
}

func TestCodexUseRefreshesAnExpiredTargetAndRefusesASignedOutOne(t *testing.T) {
	f := newCodexFixture(t)
	f.seed("home", "home", 1, t0.Add(time.Minute))
	f.setLive("work", 1, later)
	if err := f.vault.Save(ctx, "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.vault.Use(ctx, "home"); err != nil {
		t.Fatal(err)
	}
	if got := f.liveRefresh(); got == "fake-refresh-home-1" || !strings.HasPrefix(got, "fake-refresh-home-") {
		t.Fatalf("live %s", got)
	}
	// Another copy of gone's login refreshed it: the server refuses the saved token.
	f.seed("gone", "gone", 1, t0)
	f.api.Spend(codexfake.RefreshToken("gone", 1))
	if _, err := f.vault.Use(ctx, "gone"); err == nil || !strings.Contains(err.Error(), "rcodex account add gone") {
		t.Fatalf("signed out: %v", err)
	}
}

func TestCodexUseRefusesToDropALogin(t *testing.T) {
	t.Run("unnamed", func(t *testing.T) {
		f := newCodexFixture(t)
		f.seed("home", "home", 1, later)
		f.setLive("work", 1, later)
		if _, err := f.vault.Use(ctx, "home"); err == nil || !strings.Contains(err.Error(), "work@example.com") || !strings.Contains(err.Error(), "rcodex account save") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("api key", func(t *testing.T) {
		f := newCodexFixture(t)
		f.seed("home", "home", 1, later)
		f.writeLive(`{"OPENAI_API_KEY":"sk-fake"}`)
		if _, err := f.vault.Use(ctx, "home"); err == nil || !strings.Contains(err.Error(), "not signed in with ChatGPT") {
			t.Fatalf("err = %v", err)
		}
		if data, _ := os.ReadFile(f.paths.AuthFile()); string(data) != `{"OPENAI_API_KEY":"sk-fake"}` {
			t.Fatalf("auth.json %s", data)
		}
	})
	t.Run("about to refresh", func(t *testing.T) {
		f := newCodexFixture(t)
		f.seed("home", "home", 1, later)
		f.seed("work", "work", 1, t0.Add(10*time.Minute))
		f.setLive("work", 1, t0.Add(10*time.Minute))
		if _, err := f.vault.Use(ctx, "home"); err == nil || !strings.Contains(err.Error(), "about to refresh") {
			t.Fatalf("err = %v", err)
		}
		if f.liveRefresh() != "fake-refresh-work-1" {
			t.Fatalf("live %s", f.liveRefresh())
		}
	})
	t.Run("expired", func(t *testing.T) {
		f := newCodexFixture(t)
		f.seed("home", "home", 1, later)
		f.seed("work", "work", 1, t0)
		f.setLive("work", 1, t0)
		if _, err := f.vault.Use(ctx, "home"); err == nil || !strings.Contains(err.Error(), "send a Codex session a message") || !strings.Contains(err.Error(), "codex logout") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("signed out", func(t *testing.T) {
		f := newCodexFixture(t)
		f.seed("home", "home", 1, later)
		if switched, err := f.vault.Use(ctx, "home"); err != nil || !switched || f.liveRefresh() != "fake-refresh-home-1" {
			t.Fatalf("Use = %v, %v", switched, err)
		}
	})
}

func TestCodexUseRefusesALoginThatChangedWhileItWorked(t *testing.T) {
	f := newCodexFixture(t)
	f.seed("work", "work", 1, later)
	f.seed("home", "home", 1, t0)
	f.setLive("work", 1, later)
	// A Codex signs in again while the switch refreshes home.
	f.api.OnRefresh(func() { f.setLive("work", 9, later) })
	if _, err := f.vault.Use(ctx, "home"); err == nil || !strings.Contains(err.Error(), "changed meanwhile") {
		t.Fatalf("err = %v", err)
	}
	if f.liveRefresh() != "fake-refresh-work-9" {
		t.Fatalf("live %s", f.liveRefresh())
	}
	if _, err := f.vault.Use(ctx, "home"); err != nil || f.entryRefresh("work") != "fake-refresh-work-9" {
		t.Fatalf("again: %v, work %s", err, f.entryRefresh("work"))
	}
}

func TestCodexKeepFreshRefreshesOnlyInactiveDueLogins(t *testing.T) {
	f := newCodexFixture(t)
	f.seed("work", "work", 1, t0.Add(time.Hour))
	f.setLive("work", 1, t0.Add(time.Hour))
	f.seed("home", "home", 1, t0.Add(12*time.Hour))
	f.seed("idle", "idle", 1, t0.Add(5*24*time.Hour))
	var log bytes.Buffer
	if err := f.vault.KeepFresh(ctx, &log); err != nil {
		t.Fatal(err)
	}
	if f.entryRefresh("home") == "fake-refresh-home-1" || f.entryRefresh("idle") != "fake-refresh-idle-1" || f.entryRefresh("work") != "fake-refresh-work-1" {
		t.Fatalf("home %s, idle %s, work %s", f.entryRefresh("home"), f.entryRefresh("idle"), f.entryRefresh("work"))
	}
	if log.String() != "refreshed account home\n" {
		t.Fatalf("log %q", log.String())
	}
}

func TestCodexKeepFreshWithNoAccountsLeavesTheBoxAlone(t *testing.T) {
	f := newCodexFixture(t)
	f.writeLive(`not json`)
	if err := f.vault.KeepFresh(ctx, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexUsageListsEveryAccount(t *testing.T) {
	f := newCodexFixture(t)
	f.seed("home", "home", 1, t0)
	f.seed("ok", "ok", 1, later)
	f.setLive("work", 1, later)
	l, err := f.vault.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowSummary(l); got != "*:40 home:40 ok:20" {
		t.Fatalf("rows %q", got)
	}
	if l.Accounts[0].Email != "work@example.com" || l.Accounts[0].Weekly == nil || l.Accounts[0].Weekly.Utilization != 40 || l.Accounts[1].Weekly.Utilization != 40 {
		t.Fatalf("listing %+v", l)
	}
	if f.entryRefresh("home") == "fake-refresh-home-1" {
		t.Fatal("expired home not refreshed")
	}
	// The live login's usage reads with its own token, never a refreshed one.
	if err := f.vault.Save(ctx, "work"); err != nil {
		t.Fatal(err)
	}
	f.setLive("work", 1, t0)
	if l, err = f.vault.Usage(ctx); err != nil || !strings.Contains(l.Accounts[len(l.Accounts)-1].Error, "Codex refreshes it") || !l.Accounts[len(l.Accounts)-1].Active {
		t.Fatalf("expired live: %+v, %v", l, err)
	}
	// Signed in with an API key instead, the box has no account to show.
	f = newCodexFixture(t)
	f.writeLive(`{"OPENAI_API_KEY":"sk-fake"}`)
	if l, err = f.vault.Usage(ctx); err != nil || len(l.Accounts) != 1 || !l.Accounts[0].Active || !strings.Contains(l.Accounts[0].Error, "not signed in with ChatGPT") {
		t.Fatalf("api key: %+v, %v", l, err)
	}
}

func TestCodexAdd(t *testing.T) {
	f := newCodexFixture(t)
	signIn := func(data string) func(string) error {
		return func(dir string) error { return os.WriteFile(filepath.Join(dir, "auth.json"), []byte(data), 0o600) }
	}
	f.setLive("work", 1, later)
	// The box's unnamed login keeps its tokens under the name.
	if added, err := f.vault.Add(ctx, "work", signIn(codexfake.Login("work", 7, later))); err != nil || added != AddedLive || f.entryRefresh("work") != "fake-refresh-work-1" {
		t.Fatalf("live account: %v, %v", added, err)
	}
	if added, err := f.vault.Add(ctx, "work", signIn(codexfake.Login("work", 8, later))); err != nil || added != AddedAlready || f.liveRefresh() != "fake-refresh-work-1" {
		t.Fatalf("active account: %v, %v", added, err)
	}
	if added, err := f.vault.Add(ctx, "home", signIn(codexfake.Login("home", 1, later))); err != nil || added != AddedNew || f.entryRefresh("home") != "fake-refresh-home-1" {
		t.Fatalf("new: %v, %v", added, err)
	}
	if added, err := f.vault.Add(ctx, "home", signIn(codexfake.Login("home", 2, later))); err != nil || added != AddedNew || f.entryRefresh("home") != "fake-refresh-home-2" {
		t.Fatalf("again: %v, %v", added, err)
	}
	for name, c := range map[string]struct {
		name, data, want string
	}{
		"mismatch":  {"home", codexfake.Login("other", 1, later), "home is home@example.com, not other@example.com"},
		"duplicate": {"again", codexfake.Login("home", 3, later), "account home already"},
		"api key":   {"x", `{"OPENAI_API_KEY":"sk-fake"}`, "not signed in with ChatGPT"},
		"nothing":   {"x", "", "no Codex login"},
	} {
		sign := signIn(c.data)
		if c.data == "" {
			sign = func(string) error { return nil }
		}
		if _, err := f.vault.Add(ctx, c.name, sign); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	dirents, err := os.ReadDir(f.vault.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirents {
		if strings.HasPrefix(d.Name(), scratchPrefix) {
			t.Fatalf("sign-in dir %s left", d.Name())
		}
	}
}

func TestCodexSaveAndRemove(t *testing.T) {
	f := newCodexFixture(t)
	if err := f.vault.Save(ctx, "work"); err == nil || !strings.Contains(err.Error(), "no login to save") {
		t.Fatalf("signed out: %v", err)
	}
	f.setLive("work", 1, later)
	if err := f.vault.Save(ctx, "work"); err != nil || f.entryRefresh("work") != "fake-refresh-work-1" {
		t.Fatalf("save: %v", err)
	}
	if err := f.vault.Save(ctx, "again"); err == nil || !strings.Contains(err.Error(), "saved as work already") {
		t.Fatalf("save again: %v", err)
	}
	if err := f.vault.Remove(ctx, "work"); err == nil || !strings.Contains(err.Error(), "the box's login") {
		t.Fatalf("remove active: %v", err)
	}
	f.seed("home", "home", 1, later)
	if err := f.vault.Remove(ctx, "home"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.vault.files().entryPath("home")); !os.IsNotExist(err) {
		t.Fatalf("home left: %v", err)
	}
}

func TestCodexRename(t *testing.T) {
	f := newCodexFixture(t)
	f.seed("work", "work", 1, later)
	f.seed("home", "home", 1, later)
	if err := f.vault.Rename(ctx, "work", "job"); err != nil || f.entryRefresh("job") != "fake-refresh-work-1" {
		t.Fatalf("rename: %v", err)
	}
	if err := f.vault.Rename(ctx, "job", "home"); err == nil || !strings.Contains(err.Error(), "exists already") {
		t.Fatalf("onto home: %v", err)
	}
}

func TestCodexSignedOutAccountShowsAndFailsKeepFresh(t *testing.T) {
	f := newCodexFixture(t)
	f.setLive("work", 1, later)
	f.seed("gone", "gone", 1, t0)
	// A refresh that raced a switch spent gone's saved token.
	f.api.Spend(codexfake.RefreshToken("gone", 1))
	want := "signed out: add it again: press a in `rcodex account`, or run `rcodex account add gone`"
	l, err := f.vault.Usage(ctx)
	if err != nil || l.Accounts[1].Name != "gone" || l.Accounts[1].Error != want {
		t.Fatalf("listing %+v, %v", l, err)
	}
	if err := f.vault.KeepFresh(ctx, &bytes.Buffer{}); err == nil || err.Error() != "account gone: "+want {
		t.Fatalf("KeepFresh = %v", err)
	}
}
