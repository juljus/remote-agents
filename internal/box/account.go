package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"

	"remote-agents/internal/accounts"
	"remote-agents/internal/boxlayout"
	"remote-agents/internal/claudeauth"
	"remote-agents/internal/codexauth"
	"remote-agents/internal/spec"
)

const AccountUsage = "usage: remote-agents-box account [claude|codex] keep-fresh | claude|codex list | use <name> | save <name> | remove <name> | rename <name> <new name> | add <name> <exec spec> | claude start <name> <exec spec>"

// vault is one agent's saved accounts, as account commands use them.
type vault interface {
	Usage(ctx context.Context) (accounts.Listing, error)
	Use(ctx context.Context, name string) (bool, error)
	Save(ctx context.Context, name string) error
	Remove(ctx context.Context, name string) error
	Rename(ctx context.Context, old, new string) error
	Add(ctx context.Context, name string, signIn func(dir string) error) (accounts.Added, error)
	KeepFresh(ctx context.Context, log io.Writer) error
}

// accountAgent is what account commands do differently for each agent.
type accountAgent struct {
	// envVars move the agent's login, which the box's sessions may not see.
	envVars []string
	// dirVar points a sign-in at its own dir.
	dirVar string
	open   func(l boxlayout.Layout) (openVault, error)
}

// openVault is an agent's vault, and how to tell which sessions follow a
// switch to name.
type openVault struct {
	vault
	switched func(stdout io.Writer, name string) error
	// start sends an account a request through ask, to start its weekly
	// window; nil for an agent whose window does not start that way.
	start func(ctx context.Context, name string, ask func(workdir, configDir string) error) error
}

var accountAgents = map[string]accountAgent{
	"claude": {envVars: claudeauth.EnvVars, dirVar: "CLAUDE_CONFIG_DIR", open: openClaudeVault},
	"codex":  {envVars: []string{codexauth.EnvVar}, dirVar: codexauth.EnvVar, open: openCodexVault},
}

// Account runs one account command for an agent kind. The caller catches
// SIGINT, SIGTERM and SIGHUP into ctx: a token refresh, once sent, must live
// to save what the server answers.
func Account(ctx context.Context, l boxlayout.Layout, args []string, stdio IO, base []string, lookupEnv func(string) (string, bool)) error {
	if len(args) == 0 {
		return errors.New(AccountUsage)
	}
	// Refreshing saved logins on a timer is the production install's job alone.
	keepFresh := (len(args) == 1 && args[0] == "keep-fresh") || (len(args) == 2 && args[1] == "keep-fresh")
	if keepFresh && l.Name != boxlayout.DefaultName {
		return fmt.Errorf("keep-fresh runs in only the %s install", boxlayout.DefaultName)
	}
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
	need := map[string]int{"list": 0, "use": 1, "save": 1, "remove": 1, "rename": 2, "add": 2, "start": 2, "keep-fresh": 0}
	if n, ok := need[verb]; !ok || len(rest) != n {
		return errors.New(AccountUsage)
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
		return v.switched(stdio.Stdout, rest[0])
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
	case "rename":
		if err := v.Rename(ctx, rest[0], rest[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdio.Stdout, "renamed %s to %s\n", rest[0], rest[1])
		return nil
	case "start":
		if v.start == nil {
			return fmt.Errorf("%s accounts have no weekly window a request starts", kind)
		}
		s, err := execSpec(rest[1])
		if err != nil {
			return err
		}
		ask := func(workdir, configDir string) error { return askAgent(ctx, l, agent, s, base, workdir, configDir) }
		if err := v.start(ctx, rest[0], ask); err != nil {
			return err
		}
		fmt.Fprintf(stdio.Stdout, "sent account %s a request\n", rest[0])
		return nil
	case "add":
		s, err := execSpec(rest[1])
		if err != nil {
			return err
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
		// An agent nobody saved accounts of may not even be set up for them.
		saved, err := accounts.Saved(l.AccountsDir(kind))
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", kind, err))
			continue
		}
		if !saved {
			continue
		}
		if err := Account(ctx, l, []string{kind, "keep-fresh"}, stdio, base, lookupEnv); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", kind, err))
		}
	}
	return errors.Join(failed...)
}

func openClaudeVault(l boxlayout.Layout) (openVault, error) {
	cfg, err := LoadClaudeAccounts(l)
	if err != nil {
		return openVault{}, err
	}
	store := claudeauth.Store{Paths: cfg.Paths, Endpoints: cfg.Endpoints, Client: &http.Client{}, Now: time.Now}
	switched := func(w io.Writer, name string) error {
		_, err := fmt.Fprintf(w, "switched the box to %s; running sessions use it from their next request\n", name)
		return err
	}
	v := accounts.ClaudeVault{Dir: l.AccountsDir("claude"), Store: store}
	return openVault{vault: v, switched: switched, start: v.Start}, nil
}

func openCodexVault(l boxlayout.Layout) (openVault, error) {
	cfg, err := LoadCodexAccounts(l)
	if err != nil {
		return openVault{}, err
	}
	if err := checkCodexStoresInFile(cfg.Paths); err != nil {
		return openVault{}, err
	}
	store := codexauth.Store{Paths: cfg.Paths, Endpoints: cfg.Endpoints, Client: &http.Client{}, Now: time.Now}
	switched := func(w io.Writer, name string) error { return codexSwitched(l, cfg.Paths.Home, w, name) }
	running := func() ([]string, error) { return heldCodexSessions(cfg.Paths.Home) }
	return openVault{vault: accounts.CodexVault{Dir: l.AccountsDir("codex"), Store: store, Running: running}, switched: switched}, nil
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

// codexSwitched names the running Codex sessions of the box's runs, which
// keep the login they started with until they are restarted.
func codexSwitched(l boxlayout.Layout, home string, w io.Writer, name string) error {
	if _, err := fmt.Fprintf(w, "switched the box's Codex to %s; Codex sessions started from now on use it\n", name); err != nil {
		return err
	}
	held, err := heldCodexSessions(home)
	if err == nil {
		var runs map[string]bool
		if runs, err = runSessions(l); err == nil {
			return reportHeld(w, held, runs)
		}
	}
	return fmt.Errorf("could not tell which Codex sessions are running (%w); each one keeps its account until restarted with: rcodex resume <id>", err)
}

// reportHeld names the held Codex sessions the box's runs started, and
// counts the other open Codex threads, such as subagents' or those of a
// Codex started outside rcodex.
func reportHeld(w io.Writer, held []string, runs map[string]bool) error {
	var listed []string
	for _, id := range held {
		if runs[id] {
			listed = append(listed, id)
		}
	}
	var b strings.Builder
	if len(listed) > 0 {
		fmt.Fprintf(&b, "running Codex sessions keep their account until restarted; restart each with rcodex resume <id>:\n  %s\n", strings.Join(listed, "\n  "))
	}
	if others := len(held) - len(listed); others > 0 {
		fmt.Fprintf(&b, "%d other open Codex threads, such as subagents' or a Codex's started outside rcodex, keep their account until their Codex restarts\n", others)
	}
	if len(held) > 0 {
		b.WriteString("the box neither refreshes nor switches back to the account they keep until they exit\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// runSessions are the agent sessions the box's runs last saw running.
func runSessions(l boxlayout.Layout) (map[string]bool, error) {
	records, err := filepath.Glob(filepath.Join(l.RunsDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	sessions := map[string]bool{}
	for _, record := range records {
		rec, found, err := readRunRecord(l, strings.TrimSuffix(filepath.Base(record), ".json"))
		if err != nil {
			return nil, err
		}
		if found && rec.AgentSession != "" {
			sessions[rec.AgentSession] = true
		}
	}
	return sessions, nil
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

func execSpec(token string) (spec.Spec, error) {
	s, err := spec.Decode(token)
	if err != nil {
		return spec.Spec{}, err
	}
	if s.Mode != spec.ModeExec {
		return spec.Spec{}, fmt.Errorf("want an exec spec, got mode %q", s.Mode)
	}
	return s, nil
}

// agentCommand is the spec's command with the session's shims, and with
// configDir, when set, as the agent's config dir.
func agentCommand(ctx context.Context, l boxlayout.Layout, agent accountAgent, s spec.Spec, base []string, configDir string) (*exec.Cmd, error) {
	// The base environment sets none of these either: Account refuses them.
	for _, key := range agent.envVars {
		if _, set := s.Env[key]; set {
			return nil, fmt.Errorf("the spec sets %s, which would move the login it runs with", key)
		}
	}
	path, env, err := prepareAgent(l, s, base)
	if err != nil {
		return nil, err
	}
	if configDir != "" {
		env = append(env, agent.dirVar+"="+configDir)
	}
	cmd := exec.CommandContext(ctx, path, s.Argv[1:]...)
	cmd.Args[0] = s.Argv[0]
	cmd.Dir, cmd.Env = s.Workdir, env
	return cmd, nil
}

// signIn runs the spec's sign-in command with dir as the agent's config dir
// and the session's shims, so the login's browser opens on the laptop and
// its callback reaches the box.
func signIn(l boxlayout.Layout, agent accountAgent, s spec.Spec, base []string, dir string, stdio IO) error {
	cmd, err := agentCommand(context.Background(), l, agent, s, base, dir)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("signing in: %w", err)
	}
	return nil
}

// askTimeout bounds one request an account sends to start its window, and
// askGrace how long it has to exit once stopped.
const (
	askTimeout = 2 * time.Minute
	askGrace   = 10 * time.Second
)

// askAgent runs the spec's one-request command in workdir, its output kept
// from the terminal and quoted only when it fails.
func askAgent(ctx context.Context, l boxlayout.Layout, agent accountAgent, s spec.Spec, base []string, workdir, configDir string) error {
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	cmd, err := agentCommand(ctx, l, agent, s, base, configDir)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	cmd.Dir, cmd.Stdout, cmd.Stderr = workdir, &out, &out
	// Stopped, Claude Code gets to finish writing a login it rotated.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = askGrace
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the request failed (%w): %s", err, lastLine(out.String()))
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
