// Package boxlayout fixes where remote-agents lives on the box, relative to
// the box user's home. box/install.sh creates the same layout.
package boxlayout

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
)

// DefaultName is the production install. Other names, such as the e2e
// suite's, install side by side with their own shpool daemon and sockets.
const DefaultName = "remote-agents"

// ClaudeBinDir is where Claude Code's official installer puts claude.
const ClaudeBinDir = ".local/bin"

const binaryName = "remote-agents-box"

// The name ends up in paths, unit names and a double-quoted remote shell command.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func ValidName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid install name %q: want lowercase letters, digits and dashes", name)
	}
	return nil
}

// BinaryRel is the box binary of install name, relative to the box user's home.
func BinaryRel(name string) string {
	return filepath.Join(".local/share", name, "bin", binaryName)
}

type Layout struct{ Home, Name string }

// FromExecutable finds the install a box binary at exe belongs to. exe has
// its symlinks resolved, as the kernel reports it; home may contain symlinks.
func FromExecutable(home, exe string) (Layout, error) {
	if filepath.Base(exe) != binaryName {
		return Layout{}, fmt.Errorf("%s is not the box binary %s", exe, binaryName)
	}
	l := Layout{Home: home, Name: filepath.Base(filepath.Dir(filepath.Dir(exe)))}
	if err := ValidName(l.Name); err != nil {
		return Layout{}, fmt.Errorf("%s: %w", exe, err)
	}
	resolved, err := filepath.EvalSymlinks(l.Binary())
	if errors.Is(err, fs.ErrNotExist) || (err == nil && resolved != exe) {
		return Layout{}, fmt.Errorf("%s is not installed under %s", exe, filepath.Join(home, ".local/share", l.Name, "bin"))
	}
	if err != nil {
		return Layout{}, err
	}
	return l, nil
}

func (l Layout) share(rel string) string { return filepath.Join(l.Home, ".local/share", l.Name, rel) }
func (l Layout) state(rel string) string { return filepath.Join(l.Home, ".local/state", l.Name, rel) }

func (l Layout) Binary() string                  { return filepath.Join(l.Home, BinaryRel(l.Name)) }
func (l Layout) Shims() string                   { return l.share("shims") }
func (l Layout) HookBin() string                 { return l.share("hookbin") }
func (l Layout) Shpool() string                  { return l.share("bin/shpool") }
func (l Layout) ShpoolConfig() string            { return filepath.Join(l.Home, ".config", l.Name, "shpool.toml") }
func (l Layout) ShpoolSocket() string            { return l.state("shpool.sock") }
func (l Layout) LaptopSocket() string            { return l.state("laptop.sock") }
func (l Layout) EndedDir() string                { return l.state("ended") }
func (l Layout) ClaudeBin() string               { return filepath.Join(l.Home, ClaudeBinDir) }
func (l Layout) EndedMarker(runID string) string { return filepath.Join(l.EndedDir(), runID+".json") }

// The latest connection attempt that claimed each session, and the lock that
// orders the claims.
func (l Layout) AttachmentsDir() string  { return l.state("attachments") }
func (l Layout) AttachmentsLock() string { return l.state("attachments.lock") }
func (l Layout) Attachment(session string) string {
	return filepath.Join(l.AttachmentsDir(), session+".json")
}

// PastedDir holds the laptop image files pasted into sessions, by content hash.
func (l Layout) PastedDir() string { return l.state("pasted") }

// The reaper's files: hook reports and the launcher's status per shpool
// session, records of reaped Claude sessions, its log and its config.
func (l Layout) ActivityDir() string { return l.state("activity") }
func (l Layout) HookState(session string) string {
	return filepath.Join(l.ActivityDir(), session+".hooks.json")
}
func (l Layout) Status(session string) string {
	return filepath.Join(l.ActivityDir(), session+".status.json")
}
func (l Layout) ReapedDir() string { return l.state("reaped") }
func (l Layout) Reaped(claudeSession string) string {
	return filepath.Join(l.ReapedDir(), claudeSession+".json")
}
func (l Layout) ReapedSessionsDir() string { return l.state("reaped-sessions") }
func (l Layout) ReapedSession(session string) string {
	return filepath.Join(l.ReapedSessionsDir(), session+".json")
}
func (l Layout) ReaperLog() string    { return l.state("reaper.log") }
func (l Layout) ReaperConfig() string { return filepath.Join(l.Home, ".config", l.Name, "reaper.toml") }
