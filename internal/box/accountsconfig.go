package box

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/claudeauth"
)

// AccountsConfig is where an install's account commands find Claude Code's
// login and the services it talks to.
type AccountsConfig struct {
	Paths     claudeauth.Paths
	Endpoints claudeauth.Endpoints
}

type rawAccountsConfig struct {
	// ClaudeConfigDir is the one dir Claude Code keeps a test install's
	// login in, as CLAUDE_CONFIG_DIR would point it. Test installs must set
	// it, so they never reach the real login, and the production install
	// may not: its sessions use ~/.claude.
	ClaudeConfigDir *string `toml:"claude_config_dir"`
	// TokenURL and APIURL point at a stand-in server for tests, on loopback only.
	TokenURL *string `toml:"token_url"`
	APIURL   *string `toml:"api_url"`
}

// LoadAccountsConfig reads the install's accounts.toml; an absent file means
// Claude Code's defaults, which only the production install may use.
func LoadAccountsConfig(l boxlayout.Layout) (AccountsConfig, error) {
	path := l.AccountsConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		data, err = nil, nil
	}
	if err != nil {
		return AccountsConfig{}, fmt.Errorf("reading accounts config: %w", err)
	}
	cfg, err := ParseAccountsConfig(data, l)
	if err != nil {
		return AccountsConfig{}, fmt.Errorf("accounts config %s: %w", path, err)
	}
	return cfg, nil
}

func ParseAccountsConfig(data []byte, l boxlayout.Layout) (AccountsConfig, error) {
	var raw rawAccountsConfig
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return AccountsConfig{}, fmt.Errorf("unknown keys:\n%s", strict.String())
		}
		return AccountsConfig{}, fmt.Errorf("parsing toml: %w", err)
	}
	cfg := AccountsConfig{Endpoints: claudeauth.DefaultEndpoints}
	var err error
	switch {
	case l.Name == boxlayout.DefaultName && raw.ClaudeConfigDir != nil:
		return AccountsConfig{}, fmt.Errorf("claude_config_dir is for test installs only: the %s install's sessions use ~/.claude", l.Name)
	case l.Name == boxlayout.DefaultName:
		cfg.Paths, err = claudeauth.DefaultPaths(l.Home)
	case raw.ClaudeConfigDir == nil:
		return AccountsConfig{}, fmt.Errorf("install %s needs claude_config_dir: only the %s install may use ~/.claude", l.Name, boxlayout.DefaultName)
	default:
		var dir string
		dir, err = testConfigDir(*raw.ClaudeConfigDir, l.Home)
		if err != nil {
			return AccountsConfig{}, fmt.Errorf("claude_config_dir: %w", err)
		}
		cfg.Paths, err = claudeauth.PathsIn(dir)
	}
	if err != nil {
		return AccountsConfig{}, err
	}
	for key, set := range map[string]struct {
		raw   *string
		field *string
	}{"token_url": {raw.TokenURL, &cfg.Endpoints.TokenURL}, "api_url": {raw.APIURL, &cfg.Endpoints.APIBase}} {
		if set.raw == nil {
			continue
		}
		if l.Name == boxlayout.DefaultName {
			return AccountsConfig{}, fmt.Errorf("%s is for test installs only", key)
		}
		if err := loopbackURL(*set.raw); err != nil {
			return AccountsConfig{}, fmt.Errorf("%s: %w", key, err)
		}
		*set.field = *set.raw
	}
	return cfg, nil
}

// testConfigDir resolves a test install's Claude Code config dir, which
// must exist and must not be the real login's: ~/.claude holds its
// credentials, and home its global config. Its login files must be its own,
// not links to others.
func testConfigDir(dir, home string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("must be absolute, got %q", dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	for _, loginDir := range []string{home, filepath.Join(home, ".claude")} {
		loginResolved, err := filepath.EvalSymlinks(loginDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if resolved == loginResolved {
			return "", fmt.Errorf("%s is the real login's dir %s", dir, loginDir)
		}
	}
	paths, err := claudeauth.PathsIn(resolved)
	if err != nil {
		return "", err
	}
	for _, file := range []string{paths.Credentials(), paths.GlobalConfig} {
		info, err := os.Lstat(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a symlink, which may lead to the real login's files", file)
		}
	}
	return resolved, nil
}

// loopbackURL admits only a test server on the box itself, so no setting
// can send a login's tokens anywhere else.
func loopbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if u.Scheme != "http" || u.User != nil || (u.Hostname() != "localhost" && (err != nil || !ip.IsLoopback())) {
		return fmt.Errorf("%q is not an http URL on a loopback address", raw)
	}
	return nil
}
