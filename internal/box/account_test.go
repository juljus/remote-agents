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

	"remote-agents/internal/accounts"
	"remote-agents/internal/boxlayout"
	"remote-agents/internal/claudeauth"
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
	writeAccountsConfig(t, l, fmt.Sprintf("claude_config_dir = %q\napi_url = %q\n", filepath.Join(l.Home, "cfg"), api.URL))
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
	cfg, err := ParseAccountsConfig(nil, prod)
	if err != nil || cfg.Paths.SecureDir != home+"/.claude" || cfg.Endpoints != claudeauth.DefaultEndpoints {
		t.Fatalf("production default %+v, %v", cfg, err)
	}
	test := boxlayout.Layout{Home: home, Name: "remote-agents-e2e"}
	if _, err := ParseAccountsConfig(nil, test); err == nil || !strings.Contains(err.Error(), "needs claude_config_dir") {
		t.Fatalf("test install without a dir: %v", err)
	}
	cfg, err = ParseAccountsConfig([]byte("claude_config_dir = \"/t/cfg\"\ntoken_url = \"http://127.0.0.1:9/token\"\napi_url = \"http://[::1]:9\"\n"), test)
	if err != nil || cfg.Paths.GlobalConfig != "/t/cfg/.claude.json" || cfg.Endpoints.TokenURL != "http://127.0.0.1:9/token" || cfg.Endpoints.APIBase != "http://[::1]:9" {
		t.Fatalf("test install %+v, %v", cfg, err)
	}
	for _, bad := range []string{
		`claude_config_dir = "rel"`,
		`token_url = "https://127.0.0.1:1"`,
		`api_url = "http://example.com"`,
		`api_url = "http://10.0.0.1:80"`,
		`api_url = "http://127.0.0.1@evil.example"`,
		`frob = 1`,
	} {
		if _, err := ParseAccountsConfig([]byte(bad), prod); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}

func TestAccountRefusesClaudeConfigDirsFromTheEnvironment(t *testing.T) {
	l := testLayout(t)
	for _, key := range claudeauth.EnvVars {
		lookup := func(k string) (string, bool) { return "/x", k == key }
		if err := Account(context.Background(), l, []string{"claude", "list"}, IO{}, nil, lookup); err == nil || !strings.Contains(err.Error(), key+" is set") {
			t.Errorf("%s: %v", key, err)
		}
	}
}

func TestAccountRefusesUnknownCommands(t *testing.T) {
	l := testLayout(t)
	none := func(string) (string, bool) { return "", false }
	for _, args := range [][]string{nil, {"codex", "list"}, {"claude"}, {"claude", "use"}, {"claude", "list", "x"}, {"claude", "frob"}} {
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
