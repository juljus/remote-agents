package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"remote-agents/internal/accounts"
	"remote-agents/internal/boxlayout"
	"remote-agents/internal/claudeauth"
	"remote-agents/internal/codexauth"
	"remote-agents/internal/spec"
)

const AccountUsage = "usage: remote-agents-box account [claude|codex] keep-fresh | claude|codex list | use <name> | save <name> | remove <name> | add <name> <exec spec>"

// vault is one agent's saved accounts, as account commands use them.
type vault interface {
	Usage(ctx context.Context) (accounts.Listing, error)
	Use(ctx context.Context, name string) (bool, error)
	Save(ctx context.Context, name string) error
	Remove(ctx context.Context, name string) error
	Add(ctx context.Context, name string, signIn func(dir string) error) (accounts.Added, error)
	KeepFresh(ctx context.Context, log io.Writer) error
}

// accountAgent is what account commands do differently for each agent.
type accountAgent struct {
	// envVars move the agent's login, which the box's sessions may not see.
	envVars []string
	// dirVar points a sign-in at its own dir.
	dirVar string
	open   func(l boxlayout.Layout) (vault, error)
	// switched tells which sessions follow a switch to name.
	switched func(l boxlayout.Layout, w io.Writer, name string) error
}

var accountAgents = map[string]accountAgent{
	"claude": {envVars: claudeauth.EnvVars, dirVar: "CLAUDE_CONFIG_DIR", open: openClaudeVault, switched: claudeSwitched},
	"codex":  {envVars: []string{codexauth.EnvVar}, dirVar: codexauth.EnvVar, open: openCodexVault, switched: codexSwitched},
}

// Account runs one account command for an agent kind. The caller catches
// SIGINT, SIGTERM and SIGHUP into ctx: a token refresh, once sent, must live
// to save what the server answers.
func Account(ctx context.Context, l boxlayout.Layout, args []string, stdio IO, base []string, lookupEnv func(string) (string, bool)) error {
	if len(args) == 1 && args[0] == "keep-fresh" {
		return keepEveryAgentFresh(ctx, l, stdio, base, lookupEnv)
	}
	if len(args) < 2 {
		return errors.New(AccountUsage)
	}
	kind := args[0]
	agent, ok := accountAgents[kind]
	if !ok {
		return errors.New(AccountUsage)
	}
	verb, rest := args[1], args[2:]
	need := map[string]int{"list": 0, "use": 1, "save": 1, "remove": 1, "add": 2, "keep-fresh": 0}
	if n, ok := need[verb]; !ok || len(rest) != n {
		return errors.New(AccountUsage)
	}
	// Refreshing saved logins on a timer is the production install's job alone.
	if verb == "keep-fresh" && l.Name != boxlayout.DefaultName {
		return fmt.Errorf("keep-fresh runs in only the %s install", boxlayout.DefaultName)
	}
	for _, key := range agent.envVars {
		if _, set := lookupEnv(key); set {
			return fmt.Errorf("%s is set, and the box's sessions may not see it: set [%s] config_dir in %s instead", key, kind, l.AccountsConfig())
		}
	}
	v, err := agent.open(l)
	if err != nil {
		return err
	}
	cli := "r" + kind
	switch verb {
	case "list":
		listing, err := v.Usage(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdio.Stdout).Encode(listing)
	case "use":
		switched, err := v.Use(ctx, rest[0])
		if err != nil {
			return err
		}
		if !switched {
			fmt.Fprintf(stdio.Stdout, "%s is the box's account already\n", rest[0])
			return nil
		}
		return agent.switched(l, stdio.Stdout, rest[0])
	case "save":
		if err := v.Save(ctx, rest[0]); err != nil {
			return err
		}
		fmt.Fprintf(stdio.Stdout, "saved the box's login as %s\n", rest[0])
		return nil
	case "remove":
		if err := v.Remove(ctx, rest[0]); err != nil {
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
		signIn := func(dir string) error { return signIn(l, agent, s, base, dir, stdio) }
		added, err := v.Add(ctx, rest[0], signIn)
		if err != nil {
			return err
		}
		switch added {
		case accounts.AddedNew:
			fmt.Fprintf(stdio.Stdout, "saved the new login as %s; switch to it with: %s account use %s\n", rest[0], cli, rest[0])
		case accounts.AddedLive:
			fmt.Fprintf(stdio.Stdout, "the box is logged in to that account already; saved its login as %s\n", rest[0])
		case accounts.AddedAlready:
			fmt.Fprintf(stdio.Stdout, "%s is the box's login already; it stays as it is\n", rest[0])
		}
		return nil
	}
	return v.KeepFresh(ctx, stdio.Stdout)
}

// keepEveryAgentFresh runs each agent's keep-fresh, every one whatever the
// others' errors, which it reports together.
func keepEveryAgentFresh(ctx context.Context, l boxlayout.Layout, stdio IO, base []string, lookupEnv func(string) (string, bool)) error {
	var failed []error
	for _, kind := range slices.Sorted(maps.Keys(accountAgents)) {
		if err := Account(ctx, l, []string{kind, "keep-fresh"}, stdio, base, lookupEnv); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", kind, err))
		}
	}
	return errors.Join(failed...)
}

func openClaudeVault(l boxlayout.Layout) (vault, error) {
	cfg, err := LoadClaudeAccounts(l)
	if err != nil {
		return nil, err
	}
	store := claudeauth.Store{Paths: cfg.Paths, Endpoints: cfg.Endpoints, Client: &http.Client{}, Now: time.Now}
	return accounts.ClaudeVault{Dir: l.AccountsDir("claude"), Store: store}, nil
}

func openCodexVault(l boxlayout.Layout) (vault, error) {
	cfg, err := LoadCodexAccounts(l)
	if err != nil {
		return nil, err
	}
	if err := checkCodexStoresInFile(cfg.Paths); err != nil {
		return nil, err
	}
	store := codexauth.Store{Paths: cfg.Paths, Endpoints: cfg.Endpoints, Client: &http.Client{}, Now: time.Now}
	return accounts.CodexVault{Dir: l.AccountsDir("codex"), Store: store}, nil
}

// checkCodexStoresInFile refuses a Codex config that keeps logins in the
// keyring, where a switch of auth.json would not reach them.
func checkCodexStoresInFile(p codexauth.Paths) error {
	data, err := os.ReadFile(p.Config())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg struct {
		Store *string `toml:"cli_auth_credentials_store"`
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("%s: %w", p.Config(), err)
	}
	if cfg.Store != nil && *cfg.Store != "file" {
		return fmt.Errorf("%s keeps Codex's login in %q; accounts need it in auth.json (cli_auth_credentials_store = \"file\")", p.Config(), *cfg.Store)
	}
	return nil
}

func claudeSwitched(_ boxlayout.Layout, w io.Writer, name string) error {
	_, err := fmt.Fprintf(w, "switched the box to %s; running sessions use it from their next request\n", name)
	return err
}

// codexSwitched names the running Codex sessions, which keep the login
// they started with until they are restarted.
func codexSwitched(l boxlayout.Layout, w io.Writer, name string) error {
	fmt.Fprintf(w, "switched the box's Codex to %s; Codex sessions started from now on use it\n", name)
	cfg, err := LoadCodexAccounts(l)
	if err != nil {
		return err
	}
	held, err := heldCodexSessions(cfg.Paths.Home)
	if err != nil {
		_, err = fmt.Fprintf(w, "could not tell which Codex sessions are running (%v); each one keeps its account until restarted with: rcodex resume <id>\n", err)
		return err
	}
	if len(held) == 0 {
		return nil
	}
	_, err = fmt.Fprintf(w, "running Codex sessions keep their account until restarted; restart each with rcodex resume <id>:\n  %s\n", strings.Join(held, "\n  "))
	return err
}

// heldCodexSessions lists the running Codex sessions, retrying while a
// Codex holds the coordination lock, as it does only briefly.
func heldCodexSessions(home string) ([]string, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		held, err := codexHeldSessions(home)
		if !errors.Is(err, errCodexCoordinating) || time.Now().After(deadline) {
			return held, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// signIn runs the spec's sign-in command with dir as the agent's config dir
// and the session's shims, so the login's browser opens on the laptop and
// its callback reaches the box.
func signIn(l boxlayout.Layout, agent accountAgent, s spec.Spec, base []string, dir string, stdio IO) error {
	// The base environment sets none of these either: Account refuses them.
	for _, key := range agent.envVars {
		if _, set := s.Env[key]; set {
			return fmt.Errorf("the sign-in spec sets %s, which would move the login out of its sign-in dir", key)
		}
	}
	path, env, err := prepareAgent(l, s, base)
	if err != nil {
		return err
	}
	env = append(env, agent.dirVar+"="+dir)
	cmd := exec.Command(path, s.Argv[1:]...)
	cmd.Args[0] = s.Argv[0]
	cmd.Dir, cmd.Env = s.Workdir, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("signing in: %w", err)
	}
	return nil
}
