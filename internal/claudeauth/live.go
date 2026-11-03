package claudeauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// readLogin reads claudeAiOauth, nil when there is none; the caller holds
// the credentials write lock.
func readLogin(p Paths) (*Login, error) {
	creds, err := readObjectFile(p.Credentials())
	if err != nil {
		return nil, err
	}
	raw, ok := creds.get("claudeAiOauth")
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	l, err := parseLogin(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.Credentials(), err)
	}
	return &l, nil
}

// readAccount reads the global config's oauthAccount, nil when there is none.
func readAccount(p Paths) (*Account, error) {
	config, err := readObjectFile(p.GlobalConfig)
	if err != nil {
		return nil, err
	}
	raw, ok := config.get("oauthAccount")
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	a, err := parseAccount(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.GlobalConfig, err)
	}
	return &a, nil
}

// requireGlobalConfig is the file a symlinked global config points to. The
// config must exist: Claude Code writes it on its first start.
func requireGlobalConfig(p Paths) (string, error) {
	path, err := filepath.EvalSymlinks(p.GlobalConfig)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s is missing: start Claude Code once first", p.GlobalConfig)
	}
	return path, err
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

func withStorageLock(ctx context.Context, p Paths, fn func() error) error {
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	return withLocks(ctx, []string{p.storageWriteLock()}, storageWriteLockTiming, fn)
}

// writeLogin replaces claudeAiOauth, if it is still read, and keeps every
// other key, such as mcpOAuth, under the lock Claude Code takes for each
// write of the file; a Claude Code sign-in takes only that lock.
func writeLogin(ctx context.Context, p Paths, read *Login, l Login) error {
	return withStorageLock(ctx, p, func() error {
		// Claude Code refuses a symlinked credentials file, so it is never one of ours.
		if info, err := os.Lstat(p.Credentials()); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink, which Claude Code refuses", p.Credentials())
		}
		now, err := readLogin(p)
		if err != nil {
			return err
		}
		if !sameLogin(read, now) {
			return ErrLoginChanged
		}
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
	})
}

// writeAccount replaces oauthAccount under Claude Code's global config lock,
// in the file a symlinked config points to.
func writeAccount(ctx context.Context, p Paths, a Account) error {
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	return withLocks(ctx, []string{p.globalConfigLock()}, configLockTiming, func() error {
		path, err := requireGlobalConfig(p)
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		config, err := readObjectFile(path)
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
		return atomicfile.Write(path, data, info.Mode().Perm())
	})
}
