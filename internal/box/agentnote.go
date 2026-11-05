package box

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"remote-agents/internal/claudeargs"
)

// agentNote tells a session's agent what reaches the user's laptop.
const agentNote = "Laptop files the user pastes into this session are copies on this box: saving one writes it back to the laptop file, a hidden or system one once the user approves with Touch ID. To give the user a file you made, run laptop-save <file> [<laptop path>]; it lands in ~/Downloads on the laptop unless a path says where, which may be anywhere on the laptop (~/ or absolute; a relative path is in ~/Downloads). Each save waits up to 10 minutes for the user to approve it with Touch ID, so give the command that long; if they decline, it fails and the file stays here, and the same save fails unasked for 5 minutes."

// withClaudeNote gives Claude the note in its system prompt. Claude applies
// only the last --append-system-prompt, so the note joins one given.
func withClaudeNote(argv []string) []string {
	at, found := claudeargs.LastAppendSystemPrompt(argv[1:])
	if !found {
		return slices.Concat(argv[:1], []string{"--append-system-prompt", agentNote}, argv[1:])
	}
	out := slices.Clone(argv)
	out[at.Index+1] += "\n\n" + agentNote
	return out
}

// withCodexNote gives Codex the note as developer instructions, after those
// of its config in codexHome; the laptop refuses a run that sets its own.
func withCodexNote(argv []string, codexHome string) ([]string, error) {
	var config struct {
		DeveloperInstructions string `toml:"developer_instructions"`
	}
	data, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	switch {
	case err == nil:
		if err := toml.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("reading developer_instructions of %s: %w", filepath.Join(codexHome, "config.toml"), err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	instructions := agentNote
	if config.DeveloperInstructions != "" {
		instructions = config.DeveloperInstructions + "\n\n" + agentNote
	}
	encoded, err := toml.Marshal(map[string]string{"developer_instructions": instructions})
	if err != nil {
		return nil, err
	}
	override := strings.Replace(strings.TrimSuffix(string(encoded), "\n"), " = ", "=", 1)
	return slices.Concat(argv[:1], []string{"-c", override}, argv[1:]), nil
}
