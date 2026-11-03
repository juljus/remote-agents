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
	// ConfigDir and SecureDir are ~/.claude, or one dir Claude Code is pointed at.
	ConfigDir string
	// SecureDir holds .credentials.json.
	SecureDir string
	// GlobalConfig holds oauthAccount: ~/.claude.json, or .claude.json in that one dir.
	GlobalConfig string
}

// EnvVars point Claude Code at other config dirs. A command can't tell
// whether the box's sessions see the same values, so it refuses them and
// takes the dir from its own config instead.
var EnvVars = []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"}

// DefaultPaths are where Claude Code keeps a login with none of EnvVars set.
func DefaultPaths(home string) (Paths, error) {
	return checked(Paths{ConfigDir: filepath.Join(home, ".claude"), SecureDir: filepath.Join(home, ".claude"), GlobalConfig: filepath.Join(home, ".claude.json")})
}

// PathsIn are where Claude Code keeps a login with CLAUDE_CONFIG_DIR=dir.
func PathsIn(dir string) (Paths, error) {
	return checked(Paths{ConfigDir: dir, SecureDir: dir, GlobalConfig: filepath.Join(dir, ".claude.json")})
}

// EnvDir is the CLAUDE_CONFIG_DIR that points Claude Code at p: "" for
// DefaultPaths, dir for PathsIn(dir).
func (p Paths) EnvDir() string {
	if p.GlobalConfig == filepath.Join(p.ConfigDir, ".claude.json") {
		return p.ConfigDir
	}
	return ""
}

// checked refuses a legacy .config.json, which Claude Code prefers over .claude.json.
func checked(p Paths) (Paths, error) {
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

func (p Paths) storageWriteLock() string { return filepath.Join(p.SecureDir, ".storage-write.lock") }

func (p Paths) globalConfigLock() string { return p.GlobalConfig + ".lock" }
