package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"time"

	"remote-agents/internal/accounts"
	"remote-agents/internal/boxlayout"
	"remote-agents/internal/claudeauth"
	"remote-agents/internal/spec"
)

// Endpoint overrides, for tests against stand-in servers.
const (
	TokenURLEnvVar = "REMOTE_AGENTS_CLAUDE_TOKEN_URL"
	APIURLEnvVar   = "REMOTE_AGENTS_CLAUDE_API_URL"
)

const AccountUsage = "usage: remote-agents-box account claude list | use <name> | save <name> | remove <name> | add <name> <exec spec> | keep-fresh"

// Account runs one account command for an agent kind; only claude has
// accounts. The caller catches SIGINT, SIGTERM and SIGHUP into ctx: a token
// refresh, once sent, must live to save what the server answers.
func Account(ctx context.Context, l boxlayout.Layout, args []string, stdio IO, base []string, lookupEnv func(string) (string, bool)) error {
	if len(args) < 2 || args[0] != "claude" {
		return errors.New(AccountUsage)
	}
	vault, err := claudeVault(l, lookupEnv)
	if err != nil {
		return err
	}
	verb, rest := args[1], args[2:]
	need := map[string]int{"list": 0, "use": 1, "save": 1, "remove": 1, "add": 2, "keep-fresh": 0}
	if n, ok := need[verb]; !ok || len(rest) != n {
		return errors.New(AccountUsage)
	}
	switch verb {
	case "list":
		listing, err := vault.Usage(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdio.Stdout).Encode(listing)
	case "use":
		switched, err := vault.Use(ctx, rest[0])
		if err != nil {
			return err
		}
		if !switched {
			fmt.Fprintf(stdio.Stdout, "%s is the box's account already\n", rest[0])
			return nil
		}
		fmt.Fprintf(stdio.Stdout, "switched the box to %s; running sessions use it from their next request\n", rest[0])
		return nil
	case "save":
		if err := vault.Save(ctx, rest[0]); err != nil {
			return err
		}
		fmt.Fprintf(stdio.Stdout, "saved the box's login as %s\n", rest[0])
		return nil
	case "remove":
		if err := vault.Remove(ctx, rest[0]); err != nil {
			return err
		}
		fmt.Fprintf(stdio.Stdout, "removed %s\n", rest[0])
		return nil
	case "add":
		s, err := spec.Decode(rest[1])
		if err != nil {
			return err
		}
		if s.Mode != spec.ModeExec {
			return fmt.Errorf("add needs an exec spec, got mode %q", s.Mode)
		}
		signIn := func(dir string) error { return signInClaude(l, s, base, dir, stdio) }
		if err := vault.Add(ctx, rest[0], signIn); err != nil {
			return err
		}
		fmt.Fprintf(stdio.Stdout, "saved the new login as %s; switch to it with: rclaude account use %s\n", rest[0], rest[0])
		return nil
	}
	return vault.KeepFresh(ctx, stdio.Stdout)
}

func claudeVault(l boxlayout.Layout, lookupEnv func(string) (string, bool)) (accounts.Vault, error) {
	paths, err := claudeauth.PathsFromEnv(lookupEnv, l.Home)
	if err != nil {
		return accounts.Vault{}, err
	}
	ep := claudeauth.DefaultEndpoints
	if u, ok := lookupEnv(TokenURLEnvVar); ok {
		ep.TokenURL = u
	}
	if u, ok := lookupEnv(APIURLEnvVar); ok {
		ep.APIBase = u
	}
	store := claudeauth.Store{Paths: paths, Endpoints: ep, Client: &http.Client{}, Now: time.Now}
	return accounts.Vault{Dir: l.AccountsDir("claude"), Store: store}, nil
}

// signInClaude runs the spec's sign-in command, Claude Code's `auth login`,
// with dir as its config dir and the session's shims, so the login's browser
// opens on the laptop and its callback reaches the box.
func signInClaude(l boxlayout.Layout, s spec.Spec, base []string, dir string, stdio IO) error {
	path, env, err := prepareAgent(l, s, base)
	if err != nil {
		return err
	}
	env = withoutEnv(env, "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	env = append(env, "CLAUDE_CONFIG_DIR="+dir)
	cmd := exec.Command(path, s.Argv[1:]...)
	cmd.Args[0] = s.Argv[0]
	cmd.Dir, cmd.Env = s.Workdir, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("signing in: %w", err)
	}
	return nil
}

func withoutEnv(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(keys, k) {
			out = append(out, kv)
		}
	}
	return out
}
