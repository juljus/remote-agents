package box

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

func TestClaudeIsToldAboutLaptopFiles(t *testing.T) {
	got := withClaudeNote([]string{"claude", "--model", "x"})
	if !slices.Equal(got, []string{"claude", "--append-system-prompt", agentNote, "--model", "x"}) {
		t.Fatalf("argv = %q", got)
	}
	// Claude reads only the last --append-system-prompt, so the note joins it.
	for _, argv := range [][]string{
		{"claude", "--append-system-prompt", "be brief", "--verbose"},
		{"claude", "--append-system-prompt=be brief", "--verbose"},
	} {
		got := withClaudeNote(argv)
		if len(got) != len(argv) || !strings.HasSuffix(got[len(got)-2], "be brief\n\n"+agentNote) {
			t.Fatalf("withClaudeNote(%q) = %q", argv, got)
		}
		if argv[1] == "--append-system-prompt=be brief" && !strings.HasPrefix(got[1], "--append-system-prompt=be brief") {
			t.Fatalf("joined form lost: %q", got)
		}
	}
}

func codexInstructions(t *testing.T, argv []string) string {
	t.Helper()
	i := slices.Index(argv, "-c")
	if i < 0 || i+1 >= len(argv) || !strings.HasPrefix(argv[i+1], "developer_instructions=") {
		t.Fatalf("no developer_instructions in %q", argv)
	}
	var v struct {
		DeveloperInstructions string `toml:"developer_instructions"`
	}
	if err := toml.Unmarshal([]byte(argv[i+1]), &v); err != nil {
		t.Fatalf("parsing %q: %v", argv[i+1], err)
	}
	return v.DeveloperInstructions
}

func TestCodexIsToldAboutLaptopFiles(t *testing.T) {
	home := t.TempDir()
	argv, err := withCodexNote([]string{"codex", "resume", "x"}, home)
	if err != nil {
		t.Fatal(err)
	}
	if got := codexInstructions(t, argv); got != agentNote {
		t.Fatalf("instructions %q", got)
	}
	if !slices.Equal(argv[3:], []string{"resume", "x"}) {
		t.Fatalf("argv = %q", argv)
	}
	// The config's own instructions stay, with the note after them.
	config := "model = \"o3\"\ndeveloper_instructions = \"\"\"Say 'hi'.\nBe brief.\"\"\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	argv, err = withCodexNote([]string{"codex"}, home)
	if err != nil {
		t.Fatal(err)
	}
	if got := codexInstructions(t, argv); got != "Say 'hi'.\nBe brief.\n\n"+agentNote {
		t.Fatalf("instructions %q", got)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("developer_instructions = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := withCodexNote([]string{"codex"}, home); err == nil {
		t.Fatal("a broken config went unread")
	}
}
