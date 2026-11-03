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
		if r.URL.Path != "/api/oauth/usage" || r.Header.Get("Authorization") != "Bearer fake-access" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"five_hour":null,"seven_day":{"utilization":7,"resets_at":null}}`)
	}))
	defer api.Close()
	env := map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(l.Home, "cfg"), "CLAUDE_SECURESTORAGE_CONFIG_DIR": filepath.Join(l.Home, "cfg"), APIURLEnvVar: api.URL}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	token, err := spec.Spec{Mode: spec.ModeExec, Workdir: work, Argv: []string{script, "auth", "login"}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	stdio := IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out}
	base := []string{"HOME=" + l.Home, "CLAUDE_CONFIG_DIR=/elsewhere", "CLAUDE_SECURESTORAGE_CONFIG_DIR=/elsewhere"}
	if err := Account(context.Background(), l, []string{"claude", "add", "home", token}, stdio, base, lookup); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "args=auth login browser="+filepath.Join(l.Shims(), "xdg-open")+" secure=unset") {
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
