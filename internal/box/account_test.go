package box

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"remote-agents/internal/accounts"
	"remote-agents/internal/boxlayout"
	"remote-agents/internal/claudeauth"
	"remote-agents/internal/codexauth"
	"remote-agents/internal/codexauth/codexfake"
	"remote-agents/internal/spec"
)

// fakeSignIn stands in for `claude auth login`: it records its environment
// and leaves a login in $CLAUDE_CONFIG_DIR.
const fakeSignIn = `#!/bin/sh
echo "args=$* browser=$BROWSER secure=${CLAUDE_SECURESTORAGE_CONFIG_DIR:-unset}"
printf '{"claudeAiOauth":{"accessToken":"fake-access","refreshToken":"fake-refresh","expiresAt":4102444800000}}' > "$CLAUDE_CONFIG_DIR/.credentials.json"
printf '{"oauthAccount":{"accountUuid":"acc-home","organizationUuid":"org-home","emailAddress":"home@example.com"}}' > "$CLAUDE_CONFIG_DIR/.claude.json"
`

func TestAccountAddsASignInAndListsItsUsage(t *testing.T) {
	l := testLayout(t)
	l.Name = "remote-agents-test"
	if err := os.Mkdir(filepath.Join(l.Home, "cfg"), 0o700); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	script := filepath.Join(work, "claude")
	if err := os.WriteFile(script, []byte(fakeSignIn), 0o755); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-access" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/oauth/profile":
			fmt.Fprint(w, `{"account":{"uuid":"acc-home"},"organization":{"uuid":"org-home"}}`)
		case "/api/oauth/usage":
			fmt.Fprint(w, `{"five_hour":null,"seven_day":{"utilization":7,"resets_at":null}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	writeAccountsConfig(t, l, fmt.Sprintf("[claude]\nconfig_dir = %q\napi_url = %q\n", filepath.Join(l.Home, "cfg"), api.URL))
	lookup := func(string) (string, bool) { return "", false }
	token, err := spec.Spec{Mode: spec.ModeExec, Workdir: work, Argv: []string{script, "auth", "login"}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	stdio := IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out}
	base := []string{"HOME=" + l.Home}
	if err := Account(context.Background(), l, []string{"claude", "add", "home", token}, stdio, base, lookup); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "args=auth login browser="+filepath.Join(l.Shims(), "xdg-open")+" secure=unset") || !strings.Contains(out.String(), "saved the new login as home") {
		t.Fatalf("sign-in ran as:\n%s", out.String())
	}
	if info, err := os.Stat(filepath.Join(l.AccountsDir("claude"), "home.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("entry: %v", err)
	}
	out.Reset()
	if err := Account(context.Background(), l, []string{"claude", "list"}, stdio, base, lookup); err != nil {
		t.Fatal(err)
	}
	var listing accounts.Listing
	if err := json.Unmarshal(out.Bytes(), &listing); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if len(listing.Accounts) != 1 || listing.Accounts[0].Name != "home" || listing.Accounts[0].SevenDay == nil || listing.Accounts[0].SevenDay.Utilization != 7 || listing.Accounts[0].Error != "" {
		t.Fatalf("listing %s", out.String())
	}
}

func writeAccountsConfig(t *testing.T, l boxlayout.Layout, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(l.AccountsConfig()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.AccountsConfig(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAccountsConfigKeepsTestInstallsOffTheRealLogin(t *testing.T) {
	home := t.TempDir()
	prod := boxlayout.Layout{Home: home, Name: boxlayout.DefaultName}
	cfg, err := ParseClaudeAccounts(nil, prod)
	if err != nil || cfg.Paths.SecureDir != home+"/.claude" || cfg.Endpoints != claudeauth.DefaultEndpoints {
		t.Fatalf("production default %+v, %v", cfg, err)
	}
	codex, err := ParseCodexAccounts(nil, prod)
	if err != nil || codex.Paths.Home != home+"/.codex" || codex.Endpoints != codexauth.DefaultEndpoints {
		t.Fatalf("production Codex default %+v, %v", codex, err)
	}
	// The production install's sessions use the agents' own dirs, so its account commands do too.
	for _, section := range []string{"[claude]\nconfig_dir = \"/t/cfg\"", "[claude]\ntoken_url = \"http://127.0.0.1:1/token\"", "[claude]"} {
		if _, err := ParseClaudeAccounts([]byte(section), prod); err == nil || !strings.Contains(err.Error(), "for test installs only") {
			t.Errorf("production install with %q: %v", section, err)
		}
	}
	if _, err := ParseCodexAccounts([]byte("[codex]\napi_url = \"http://127.0.0.1:1\""), prod); err == nil || !strings.Contains(err.Error(), "for test installs only") {
		t.Fatalf("production Codex with a URL: %v", err)
	}
	test := boxlayout.Layout{Home: home, Name: "remote-agents-e2e"}
	if _, err := ParseClaudeAccounts(nil, test); err == nil || !strings.Contains(err.Error(), "needs [claude] config_dir") {
		t.Fatalf("test install without a dir: %v", err)
	}
	if _, err := ParseCodexAccounts([]byte("[claude]\nconfig_dir = \"/x\""), test); err == nil || !strings.Contains(err.Error(), "needs [codex] config_dir") {
		t.Fatalf("test install without a Codex dir: %v", err)
	}
	dir := filepath.Join(home, "cfg")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	both := fmt.Appendf(nil, "[claude]\nconfig_dir = %[1]q\ntoken_url = \"http://127.0.0.1:9/token\"\napi_url = \"http://[::1]:9\"\n[codex]\nconfig_dir = %[1]q\ntoken_url = \"http://127.0.0.1:8/oauth/token\"\napi_url = \"http://localhost:8/backend-api\"\n", dir)
	cfg, err = ParseClaudeAccounts(both, test)
	if err != nil || cfg.Paths.GlobalConfig != resolved+"/.claude.json" || cfg.Endpoints.TokenURL != "http://127.0.0.1:9/token" || cfg.Endpoints.APIBase != "http://[::1]:9" {
		t.Fatalf("test install %+v, %v", cfg, err)
	}
	codex, err = ParseCodexAccounts(both, test)
	if err != nil || codex.Paths.Home != resolved || codex.Endpoints.TokenOverride != "http://127.0.0.1:8/oauth/token" || codex.Endpoints.APIBase != "http://localhost:8/backend-api" {
		t.Fatalf("test Codex %+v, %v", codex, err)
	}
	for _, real := range []string{".claude", ".codex"} {
		if err := os.Mkdir(filepath.Join(home, real), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, ".claude"), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	// A test dir whose files point at the real login's.
	for _, c := range []struct{ section, file, target string }{
		{"claude", ".claude.json", filepath.Join(home, ".claude.json")},
		{"claude", ".credentials.json", filepath.Join(home, ".claude", ".credentials.json")},
		{"codex", "auth.json", filepath.Join(home, ".codex", "auth.json")},
	} {
		linked := filepath.Join(home, "linked"+c.file)
		if err := os.Mkdir(linked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.target, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(c.target, filepath.Join(linked, c.file)); err != nil {
			t.Fatal(err)
		}
		data := fmt.Appendf(nil, "[%s]\nconfig_dir = %q", c.section, linked)
		_, claudeErr := ParseClaudeAccounts(data, test)
		_, codexErr := ParseCodexAccounts(data, test)
		if err := map[string]error{"claude": claudeErr, "codex": codexErr}[c.section]; err == nil || !strings.Contains(err.Error(), "is a symlink") {
			t.Errorf("%s linked to the real one: %v", c.file, err)
		}
	}
	for _, bad := range []string{
		"config_dir = \"/x\"",
		"[claude]\nconfig_dir = \"rel\"",
		fmt.Sprintf("[claude]\nconfig_dir = %q", filepath.Join(home, "missing")),
		fmt.Sprintf("[claude]\nconfig_dir = %q", filepath.Join(home, ".claude")),
		fmt.Sprintf("[claude]\nconfig_dir = %q", filepath.Join(home, "link")),
		fmt.Sprintf("[claude]\nconfig_dir = %q", home+"/"),
		fmt.Sprintf("[claude]\nconfig_dir = %q\ntoken_url = \"https://127.0.0.1:1\"", dir),
		fmt.Sprintf("[claude]\nconfig_dir = %q\napi_url = \"http://example.com\"", dir),
		fmt.Sprintf("[claude]\nconfig_dir = %q\napi_url = \"http://10.0.0.1:80\"", dir),
		fmt.Sprintf("[claude]\nconfig_dir = %q\napi_url = \"http://127.0.0.1@evil.example\"", dir),
		fmt.Sprintf("[claude]\nconfig_dir = %q\nfrob = 1", dir),
	} {
		if _, err := ParseClaudeAccounts([]byte(bad), test); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
	for _, bad := range []string{
		fmt.Sprintf("[codex]\nconfig_dir = %q", filepath.Join(home, ".codex")),
		fmt.Sprintf("[codex]\nconfig_dir = %q\ntoken_url = \"http://example.com/oauth/token\"", dir),
	} {
		if _, err := ParseCodexAccounts([]byte(bad), test); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}

func TestAccountAddRefusesASpecThatMovesClaudesConfig(t *testing.T) {
	l := testLayout(t)
	l.Name = "remote-agents-test"
	if err := os.Mkdir(filepath.Join(l.Home, "cfg"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeAccountsConfig(t, l, fmt.Sprintf("[claude]\nconfig_dir = %q\n", filepath.Join(l.Home, "cfg")))
	work := t.TempDir()
	script := filepath.Join(work, "claude")
	if err := os.WriteFile(script, []byte(fakeSignIn), 0o755); err != nil {
		t.Fatal(err)
	}
	lookup := func(string) (string, bool) { return "", false }
	for _, key := range claudeauth.EnvVars {
		token, err := spec.Spec{Mode: spec.ModeExec, Workdir: work, Argv: []string{script, "auth", "login"}, Env: map[string]string{key: l.Home + "/.claude"}}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err = Account(context.Background(), l, []string{"claude", "add", "home", token}, IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out}, []string{"HOME=" + l.Home}, lookup)
		if err == nil || !strings.Contains(err.Error(), key) || strings.Contains(out.String(), "args=") {
			t.Fatalf("%s: %v\n%s", key, err, out.String())
		}
	}
}

// codexSignIn stands in for `codex login`: it records its environment and
// leaves the login in the file login in $CODEX_HOME.
const codexSignIn = `#!/bin/sh
echo "args=$* browser=$BROWSER"
cp "$(dirname "$0")/login.json" "$CODEX_HOME/auth.json"
`

func TestCodexAccountAddsListsAndSwitches(t *testing.T) {
	l := testLayout(t)
	l.Name = "remote-agents-test"
	cfg := filepath.Join(l.Home, "cfg")
	if err := os.Mkdir(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	api := codexfake.NewServer(t, time.Now)
	writeAccountsConfig(t, l, fmt.Sprintf("[codex]\nconfig_dir = %q\ntoken_url = %q\napi_url = %q\n", cfg, api.URL+"/oauth/token", api.URL+"/backend-api"))
	work := t.TempDir()
	script := filepath.Join(work, "codex")
	if err := os.WriteFile(script, []byte(codexSignIn), 0o755); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(48 * time.Hour)
	if err := os.WriteFile(filepath.Join(work, "login.json"), []byte(codexfake.Login("home", 1, later)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "auth.json"), []byte(codexfake.Login("work", 1, later)), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := func(string) (string, bool) { return "", false }
	var out bytes.Buffer
	stdio := IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out}
	run := func(args ...string) string {
		t.Helper()
		out.Reset()
		if err := Account(context.Background(), l, append([]string{"codex"}, args...), stdio, []string{"HOME=" + l.Home}, lookup); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	token, err := spec.Spec{Mode: spec.ModeExec, Workdir: work, Argv: []string{script, "login"}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if got := run("add", "home", token); !strings.Contains(got, "args=login browser="+filepath.Join(l.Shims(), "xdg-open")) || !strings.Contains(got, "switch to it with: rcodex account use home") {
		t.Fatalf("add:\n%s", got)
	}
	run("save", "work")
	var listing accounts.Listing
	if err := json.Unmarshal([]byte(run("list")), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Accounts) != 2 || listing.Accounts[1].Name != "work" || !listing.Accounts[1].Active || listing.Accounts[0].SevenDay == nil || listing.Accounts[0].SevenDay.Utilization != 40 {
		t.Fatalf("listing %+v", listing)
	}
	// A run's Codex holds its session and a subagent's: the switch leaves both on work.
	for _, id := range []string{codexSession, "01a1a1e4-0000-7753-927c-b59c2ec2f926"} {
		unlock := lockFile(t, filepath.Join(cfg, "thread-writer-locks", id+".lock"))
		defer unlock()
	}
	if err := os.WriteFile(filepath.Join(cfg, "thread-writer-locks", ".coordination.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeRunRecord(l, "11111111-2222-4333-8444-555555555555", runRecord{AgentSession: codexSession}); err != nil {
		t.Fatal(err)
	}
	got := run("use", "home")
	if !strings.Contains(got, "switched the box's Codex to home; Codex sessions started from now on use it") ||
		!strings.Contains(got, "rcodex resume <id>:\n  "+codexSession+"\n1 other open Codex threads") {
		t.Fatalf("use:\n%s", got)
	}
	if data, err := os.ReadFile(filepath.Join(cfg, "auth.json")); err != nil || !strings.Contains(string(data), codexfake.RefreshToken("home", 1)) {
		t.Fatalf("auth.json: %v", err)
	}
}

func TestCodexAccountsNeedLoginsInAuthJSON(t *testing.T) {
	l := testLayout(t)
	l.Name = "remote-agents-test"
	cfg := filepath.Join(l.Home, "cfg")
	if err := os.Mkdir(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	writeAccountsConfig(t, l, fmt.Sprintf("[codex]\nconfig_dir = %q\n", cfg))
	lookup := func(string) (string, bool) { return "", false }
	for store, ok := range map[string]bool{`"file"`: true, `"keyring"`: false, `"auto"`: false} {
		if err := os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("model = \"x\"\ncli_auth_credentials_store = "+store+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := Account(context.Background(), l, []string{"codex", "list"}, IO{Stdout: &out}, nil, lookup)
		if (err == nil) != ok || (err != nil && !strings.Contains(err.Error(), "cli_auth_credentials_store")) {
			t.Errorf("%s: %v", store, err)
		}
	}
}

func TestCodexAccountAddRefusesASpecThatMovesCodexHome(t *testing.T) {
	l := testLayout(t)
	l.Name = "remote-agents-test"
	if err := os.Mkdir(filepath.Join(l.Home, "cfg"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeAccountsConfig(t, l, fmt.Sprintf("[codex]\nconfig_dir = %q\n", filepath.Join(l.Home, "cfg")))
	work := t.TempDir()
	token, err := spec.Spec{Mode: spec.ModeExec, Workdir: work, Argv: []string{"/bin/echo", "login"}, Env: map[string]string{codexauth.EnvVar: l.Home + "/.codex"}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = Account(context.Background(), l, []string{"codex", "add", "home", token}, IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out}, []string{"HOME=" + l.Home}, func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), codexauth.EnvVar) || out.Len() != 0 {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestKeepFreshRunsOnlyInTheProductionInstall(t *testing.T) {
	l := testLayout(t)
	l.Name = "remote-agents-e2e"
	if err := os.Mkdir(filepath.Join(l.Home, "cfg"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeAccountsConfig(t, l, fmt.Sprintf("[claude]\nconfig_dir = %q\n", filepath.Join(l.Home, "cfg")))
	lookup := func(string) (string, bool) { return "", false }
	for _, args := range [][]string{{"claude", "keep-fresh"}, {"codex", "keep-fresh"}, {"keep-fresh"}} {
		if err := Account(context.Background(), l, args, IO{}, nil, lookup); err == nil || !strings.Contains(err.Error(), "runs in only the remote-agents install") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	// An account may be named keep-fresh.
	if err := Account(context.Background(), l, []string{"claude", "remove", "keep-fresh"}, IO{}, nil, lookup); err == nil || !strings.Contains(err.Error(), "no account keep-fresh") {
		t.Fatalf("remove keep-fresh: %v", err)
	}
}

func TestKeepFreshOfEveryAgentRunsEachDespiteTheOthers(t *testing.T) {
	l := testLayout(t)
	for _, kind := range []string{"claude", "codex"} {
		if err := os.MkdirAll(l.AccountsDir(kind), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(l.AccountsDir(kind), "broken.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	none := func(string) (string, bool) { return "", false }
	err := Account(context.Background(), l, []string{"keep-fresh"}, IO{Stdout: &bytes.Buffer{}}, nil, none)
	if err == nil || !strings.Contains(err.Error(), "claude: account broken") || !strings.Contains(err.Error(), "codex: account broken") {
		t.Fatalf("err = %v", err)
	}
	// An agent with no saved accounts is left alone, however it keeps its login.
	if err := os.RemoveAll(l.AccountsDir("codex")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(l.AccountsDir("claude"), "broken.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.Home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.Home, ".codex", "config.toml"), []byte("cli_auth_credentials_store = \"keyring\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Account(context.Background(), l, []string{"keep-fresh"}, IO{Stdout: &bytes.Buffer{}}, nil, none); err != nil {
		t.Fatalf("no accounts: %v", err)
	}
}

func TestAccountRefusesConfigDirsFromTheEnvironment(t *testing.T) {
	l := testLayout(t)
	for kind, keys := range map[string][]string{"claude": claudeauth.EnvVars, "codex": {codexauth.EnvVar}} {
		for _, key := range keys {
			lookup := func(k string) (string, bool) { return "/x", k == key }
			if err := Account(context.Background(), l, []string{kind, "list"}, IO{}, nil, lookup); err == nil || !strings.Contains(err.Error(), key+" is set") {
				t.Errorf("%s: %v", key, err)
			}
		}
	}
}

func TestAccountRefusesUnknownCommands(t *testing.T) {
	l := testLayout(t)
	none := func(string) (string, bool) { return "", false }
	for _, args := range [][]string{nil, {"gemini", "list"}, {"claude"}, {"claude", "use"}, {"claude", "list", "x"}, {"claude", "frob"}} {
		if err := Account(context.Background(), l, args, IO{}, nil, none); err == nil || err.Error() != AccountUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
	token, err := spec.Spec{Mode: spec.ModeReattach, Session: "claude-x"}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := Account(context.Background(), l, []string{"claude", "add", "x", token}, IO{}, nil, none); err == nil || !strings.Contains(err.Error(), "exec spec") {
		t.Fatalf("reattach spec: %v", err)
	}
}

func TestCodexSwitchSaysWhenItCannotTellTheRunningSessions(t *testing.T) {
	l := testLayout(t)
	home := t.TempDir()
	unlock := lockFile(t, filepath.Join(home, "thread-writer-locks", ".coordination.lock"))
	defer unlock()
	var out bytes.Buffer
	err := codexSwitched(l, home, &out, "home")
	if err == nil || !strings.Contains(err.Error(), "could not tell which Codex sessions are running") || !strings.Contains(out.String(), "switched the box's Codex to home") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}
