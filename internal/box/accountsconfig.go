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
	"remote-agents/internal/codexauth"
)

// ClaudeAccounts is where an install's Claude account commands find Claude
// Code's login and the services it talks to.
type ClaudeAccounts struct {
	Paths     claudeauth.Paths
	Endpoints claudeauth.Endpoints
}

// CodexAccounts is where an install's Codex account commands find Codex's
// login and the services it talks to.
type CodexAccounts struct {
	Paths     codexauth.Paths
	Endpoints codexauth.Endpoints
}

// rawAccountsConfig is accounts.toml: a section per agent, for test
// installs only. The production install has none: its sessions use the
// agents' own dirs and services.
type rawAccountsConfig struct {
	Claude *rawAgentAccounts `toml:"claude"`
	Codex  *rawAgentAccounts `toml:"codex"`
}

type rawAgentAccounts struct {
	// ConfigDir is the one dir the agent keeps a test install's login in, as
	// CLAUDE_CONFIG_DIR or CODEX_HOME would point it. Test installs must set
	// it, so they never reach the real login.
	ConfigDir *string `toml:"config_dir"`
	// TokenURL and APIURL point at a stand-in server, on loopback only.
	TokenURL *string `toml:"token_url"`
	APIURL   *string `toml:"api_url"`
}

// testAccounts is one agent's section of a test install, checked.
type testAccounts struct {
	dir      string
	tokenURL *string
	apiURL   *string
}

// LoadClaudeAccounts reads the Claude section of the install's accounts.toml.
func LoadClaudeAccounts(l boxlayout.Layout) (ClaudeAccounts, error) {
	data, err := readAccountsConfig(l)
	if err != nil {
		return ClaudeAccounts{}, err
	}
	cfg, err := ParseClaudeAccounts(data, l)
	if err != nil {
		return ClaudeAccounts{}, fmt.Errorf("accounts config %s: %w", l.AccountsConfig(), err)
	}
	return cfg, nil
}

// LoadCodexAccounts reads the Codex section of the install's accounts.toml.
func LoadCodexAccounts(l boxlayout.Layout) (CodexAccounts, error) {
	data, err := readAccountsConfig(l)
	if err != nil {
		return CodexAccounts{}, err
	}
	cfg, err := ParseCodexAccounts(data, l)
	if err != nil {
		return CodexAccounts{}, fmt.Errorf("accounts config %s: %w", l.AccountsConfig(), err)
	}
	return cfg, nil
}

// readAccountsConfig is accounts.toml, empty when there is none.
func readAccountsConfig(l boxlayout.Layout) ([]byte, error) {
	data, err := os.ReadFile(l.AccountsConfig())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading accounts config: %w", err)
	}
	return data, nil
}

func ParseClaudeAccounts(data []byte, l boxlayout.Layout) (ClaudeAccounts, error) {
	raw, err := parseAccountsConfig(data)
	if err != nil {
		return ClaudeAccounts{}, err
	}
	cfg := ClaudeAccounts{Endpoints: claudeauth.DefaultEndpoints}
	if l.Name == boxlayout.DefaultName {
		if raw.Claude != nil {
			return ClaudeAccounts{}, fmt.Errorf("[claude] is for test installs only: the %s install's sessions use ~/.claude", l.Name)
		}
		cfg.Paths, err = claudeauth.DefaultPaths(l.Home)
		return cfg, err
	}
	test, err := checkTestAccounts(raw.Claude, "claude", l, []string{l.Home, filepath.Join(l.Home, ".claude")}, func(dir string) ([]string, error) {
		paths, err := claudeauth.PathsIn(dir)
		return []string{paths.Credentials(), paths.GlobalConfig}, err
	})
	if err != nil {
		return ClaudeAccounts{}, err
	}
	if cfg.Paths, err = claudeauth.PathsIn(test.dir); err != nil {
		return ClaudeAccounts{}, err
	}
	setURL(&cfg.Endpoints.TokenURL, test.tokenURL)
	setURL(&cfg.Endpoints.APIBase, test.apiURL)
	return cfg, nil
}

func ParseCodexAccounts(data []byte, l boxlayout.Layout) (CodexAccounts, error) {
	raw, err := parseAccountsConfig(data)
	if err != nil {
		return CodexAccounts{}, err
	}
	cfg := CodexAccounts{Endpoints: codexauth.DefaultEndpoints}
	if l.Name == boxlayout.DefaultName {
		if raw.Codex != nil {
			return CodexAccounts{}, fmt.Errorf("[codex] is for test installs only: the %s install's sessions use ~/.codex", l.Name)
		}
		cfg.Paths = codexauth.DefaultPaths(l.Home)
		return cfg, nil
	}
	test, err := checkTestAccounts(raw.Codex, "codex", l, []string{codexauth.DefaultPaths(l.Home).Home}, func(dir string) ([]string, error) {
		return []string{codexauth.Paths{Home: dir}.AuthFile()}, nil
	})
	if err != nil {
		return CodexAccounts{}, err
	}
	cfg.Paths = codexauth.Paths{Home: test.dir}
	setURL(&cfg.Endpoints.TokenOverride, test.tokenURL)
	setURL(&cfg.Endpoints.APIBase, test.apiURL)
	return cfg, nil
}

func setURL(field *string, value *string) {
	if value != nil {
		*field = *value
	}
}

func parseAccountsConfig(data []byte) (rawAccountsConfig, error) {
	var raw rawAccountsConfig
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return rawAccountsConfig{}, fmt.Errorf("unknown keys:\n%s", strict.String())
		}
		return rawAccountsConfig{}, fmt.Errorf("parsing toml: %w", err)
	}
	return raw, nil
}

// checkTestAccounts checks a test install's section of agent: its config
// dir must exist, must not be one of the real login's dirs, and the login
// files loginFiles names in it must be its own, not links to others; its
// URLs must be on loopback.
func checkTestAccounts(raw *rawAgentAccounts, agent string, l boxlayout.Layout, realDirs []string, loginFiles func(dir string) ([]string, error)) (testAccounts, error) {
	if raw == nil || raw.ConfigDir == nil {
		return testAccounts{}, fmt.Errorf("install %s needs [%s] config_dir: only the %s install may use the real login", l.Name, agent, boxlayout.DefaultName)
	}
	dir := *raw.ConfigDir
	if !filepath.IsAbs(dir) {
		return testAccounts{}, fmt.Errorf("[%s] config_dir must be absolute, got %q", agent, dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return testAccounts{}, fmt.Errorf("[%s] config_dir: %w", agent, err)
	}
	for _, realDir := range realDirs {
		realResolved, err := filepath.EvalSymlinks(realDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return testAccounts{}, err
		}
		if resolved == realResolved {
			return testAccounts{}, fmt.Errorf("[%s] config_dir %s is the real login's dir %s", agent, dir, realDir)
		}
	}
	files, err := loginFiles(resolved)
	if err != nil {
		return testAccounts{}, err
	}
	for _, file := range files {
		info, err := os.Lstat(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return testAccounts{}, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return testAccounts{}, fmt.Errorf("%s is a symlink, which may lead to the real login's files", file)
		}
	}
	for key, value := range map[string]*string{"token_url": raw.TokenURL, "api_url": raw.APIURL} {
		if value == nil {
			continue
		}
		if err := loopbackURL(*value); err != nil {
			return testAccounts{}, fmt.Errorf("[%s] %s: %w", agent, key, err)
		}
	}
	return testAccounts{dir: resolved, tokenURL: raw.TokenURL, apiURL: raw.APIURL}, nil
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
