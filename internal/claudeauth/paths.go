// Package claudeauth reads and writes Claude Code's login on the box the way
// Claude Code itself does: the same files, the same locks, the same token
// endpoint. It never logs or prints a token.
package claudeauth

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Paths are the files Claude Code keeps its login in.
type Paths struct {
	// ConfigDir is $CLAUDE_CONFIG_DIR, else ~/.claude.
	ConfigDir string
	// SecureDir holds .credentials.json: $CLAUDE_SECURESTORAGE_CONFIG_DIR, else ConfigDir.
	SecureDir string
	// GlobalConfig holds oauthAccount: .claude.json in $CLAUDE_CONFIG_DIR, else in home.
	GlobalConfig string
}

// PathsFromEnv resolves Paths as Claude Code does for a process with this
// environment; lookupEnv is os.LookupEnv or a stand-in.
func PathsFromEnv(lookupEnv func(string) (string, bool), home string) (Paths, error) {
	p := Paths{ConfigDir: filepath.Join(home, ".claude"), GlobalConfig: filepath.Join(home, ".claude.json")}
	if dir, _ := lookupEnv("CLAUDE_CONFIG_DIR"); dir != "" {
		p.ConfigDir = dir
		p.GlobalConfig = filepath.Join(dir, ".claude.json")
	}
	p.SecureDir = p.ConfigDir
	// Set but empty, it means ~/.claude.
	if dir, set := lookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR"); set {
		p.SecureDir = dir
		if dir == "" {
			p.SecureDir = filepath.Join(home, ".claude")
		}
	}
	// Claude Code prefers a legacy .config.json in the config dir over .claude.json.
	legacy := filepath.Join(p.ConfigDir, ".config.json")
	if _, err := os.Stat(legacy); err == nil {
		return Paths{}, fmt.Errorf("%s is Claude Code's legacy global config, which remote-agents does not handle", legacy)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Paths{}, err
	}
	return p, nil
}

func (p Paths) Credentials() string { return filepath.Join(p.SecureDir, ".credentials.json") }

// refreshLocks are the lock directories Claude Code takes, in this order,
// while it refreshes or saves the login: its own and a legacy one beside the dir.
func (p Paths) refreshLocks() ([2]string, error) {
	real, err := filepath.EvalSymlinks(p.SecureDir)
	if errors.Is(err, fs.ErrNotExist) {
		real = p.SecureDir
	} else if err != nil {
		return [2]string{}, err
	}
	return [2]string{filepath.Join(p.SecureDir, ".oauth_refresh.lock"), real + ".lock"}, nil
}

func (p Paths) globalConfigLock() string { return p.GlobalConfig + ".lock" }
