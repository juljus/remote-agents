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
	// ClaudeConfigDir is the one dir Claude Code keeps the login in, as
	// CLAUDE_CONFIG_DIR would point it; it defaults to ~/.claude only for
	// the production install, so a test install never reaches the real login.
	ClaudeConfigDir *string `toml:"claude_config_dir"`
	// TokenURL and APIURL point at a stand-in server for tests, on loopback only.
	TokenURL *string `toml:"token_url"`
	APIURL   *string `toml:"api_url"`
}

// LoadAccountsConfig reads the install's accounts.toml; an absent file means
// Claude Code's defaults, for the production install only.
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
	case raw.ClaudeConfigDir != nil && !filepath.IsAbs(*raw.ClaudeConfigDir):
		return AccountsConfig{}, fmt.Errorf("claude_config_dir must be absolute, got %q", *raw.ClaudeConfigDir)
	case raw.ClaudeConfigDir != nil:
		cfg.Paths, err = claudeauth.PathsIn(*raw.ClaudeConfigDir)
	case l.Name != boxlayout.DefaultName:
		return AccountsConfig{}, fmt.Errorf("install %s needs claude_config_dir: only the %s install may use ~/.claude", l.Name, boxlayout.DefaultName)
	default:
		cfg.Paths, err = claudeauth.DefaultPaths(l.Home)
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
		if err := loopbackURL(*set.raw); err != nil {
			return AccountsConfig{}, fmt.Errorf("%s: %w", key, err)
		}
		*set.field = *set.raw
	}
	return cfg, nil
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
