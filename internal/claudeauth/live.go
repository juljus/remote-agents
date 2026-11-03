package claudeauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"remote-agents/internal/atomicfile"
)

// credentialsMode is the mode Claude Code gives .credentials.json.
const credentialsMode = 0o600

// Live is the login Claude Code on this box uses now.
type Live struct {
	// Login is nil when Claude Code is not logged in.
	Login *Login
	// Account is nil when the global config names no account.
	Account *Account
}

// readLive reads both files; the caller holds the refresh lock.
func readLive(p Paths) (Live, error) {
	creds, err := readObjectFile(p.Credentials())
	if err != nil {
		return Live{}, err
	}
	var live Live
	if raw, ok := creds.get("claudeAiOauth"); ok && string(raw) != "null" {
		l, err := parseLogin(raw)
		if err != nil {
			return Live{}, fmt.Errorf("%s: %w", p.Credentials(), err)
		}
		live.Login = &l
	}
	config, err := readObjectFile(p.GlobalConfig)
	if err != nil {
		return Live{}, err
	}
	if raw, ok := config.get("oauthAccount"); ok && string(raw) != "null" {
		a, err := parseAccount(raw)
		if err != nil {
			return Live{}, fmt.Errorf("%s: %w", p.GlobalConfig, err)
		}
		live.Account = &a
	}
	return live, nil
}

// readObjectFile reads a JSON object file; a missing file is an empty object.
func readObjectFile(path string) (object, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o, err := parseObject(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return o, nil
}

// writeLogin replaces claudeAiOauth and keeps every other key, such as
// mcpOAuth; the caller holds the refresh lock.
func writeLogin(p Paths, l Login) error {
	creds, err := readObjectFile(p.Credentials())
	if err != nil {
		return err
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	data, err := creds.with("claudeAiOauth", raw).marshal("")
	if err != nil {
		return err
	}
	return atomicfile.Write(p.Credentials(), data, credentialsMode)
}

// writeAccount replaces oauthAccount under Claude Code's global config lock.
// The config must exist: Claude Code writes it on its first start.
func writeAccount(ctx context.Context, p Paths, a Account) error {
	return withLocks(ctx, []string{p.globalConfigLock()}, configLockTiming, func() error {
		info, err := os.Stat(p.GlobalConfig)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s is missing: start Claude Code once first", p.GlobalConfig)
		}
		if err != nil {
			return err
		}
		config, err := readObjectFile(p.GlobalConfig)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(a)
		if err != nil {
			return err
		}
		data, err := config.with("oauthAccount", raw).marshal("  ")
		if err != nil {
			return err
		}
		return atomicfile.Write(p.GlobalConfig, data, info.Mode().Perm())
	})
}
