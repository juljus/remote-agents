package config

import (
	"slices"
	"strings"
	"testing"
)

const valid = `
host = "box.example"
workdir = "/home/agent/work"

[link]
opener = "/opt/homebrew/bin/firefox-open"

[cmux]
wrapper = "/Applications/cmux.app/Contents/Resources/bin/cmux-claude-wrapper"

[agents.claude]
command = "claude"
kind = "claude"
args = ["--dangerously-skip-permissions"]
`

func TestParseValid(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Host != "box.example" || cfg.Workdir != "/home/agent/work" {
		t.Fatalf("unexpected host/workdir: %+v", cfg)
	}
	if cfg.Link.Opener != "/opt/homebrew/bin/firefox-open" {
		t.Fatalf("unexpected opener: %q", cfg.Link.Opener)
	}
	if cfg.Cmux == nil || cfg.Cmux.Wrapper == "" {
		t.Fatalf("cmux table not parsed: %+v", cfg.Cmux)
	}
	agent, err := cfg.Agent("claude")
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
	if agent.Command != "claude" || agent.Kind != KindClaude || !slices.Equal(agent.Args, []string{"--dangerously-skip-permissions"}) {
		t.Fatalf("unexpected agent: %+v", agent)
	}
}

func TestParseWithoutCmux(t *testing.T) {
	src := strings.Replace(valid, "[cmux]\nwrapper = \"/Applications/cmux.app/Contents/Resources/bin/cmux-claude-wrapper\"\n", "", 1)
	cfg, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Cmux != nil {
		t.Fatalf("expected no cmux integration, got %+v", cfg.Cmux)
	}
}

func TestParseWithoutOpenerOpensWithOpen(t *testing.T) {
	for name, src := range map[string]string{
		"no link table":    strings.Replace(valid, "[link]\nopener = \"/opt/homebrew/bin/firefox-open\"\n", "", 1),
		"empty link table": strings.Replace(valid, "opener = \"/opt/homebrew/bin/firefox-open\"\n", "", 1),
	} {
		cfg, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		if cfg.Link.Opener != "/usr/bin/open" {
			t.Fatalf("%s: opener = %q, want /usr/bin/open", name, cfg.Link.Opener)
		}
	}
}

func TestParseBoxInstall(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BoxInstall != "remote-agents" {
		t.Fatalf("default box install = %q", cfg.BoxInstall)
	}
	cfg, err = Parse([]byte(strings.Replace(valid, "workdir =", "box_install = \"remote-agents-e2e\"\nworkdir =", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BoxInstall != "remote-agents-e2e" {
		t.Fatalf("box install = %q", cfg.BoxInstall)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct {
		src  string
		want string
	}{
		"unknown top-level key":  {valid + "\nbogus = 1\n", "bogus"},
		"unknown agent key":      {strings.Replace(valid, `kind = "claude"`, "kind = \"claude\"\nextra = true", 1), "extra"},
		"missing host":           {strings.Replace(valid, `host = "box.example"`, "", 1), "host"},
		"missing workdir":        {strings.Replace(valid, `workdir = "/home/agent/work"`, "", 1), "workdir"},
		"relative workdir":       {strings.Replace(valid, `"/home/agent/work"`, `"work"`, 1), "absolute"},
		"forward_ports removed":  {strings.Replace(valid, "[link]\n", "[link]\nforward_ports = [8000]\n", 1), "forward_ports"},
		"relative opener":        {strings.Replace(valid, `"/opt/homebrew/bin/firefox-open"`, `"open"`, 1), "absolute"},
		"no agents":              {strings.Replace(valid, "[agents.claude]\ncommand = \"claude\"\nkind = \"claude\"\nargs = [\"--dangerously-skip-permissions\"]\n", "", 1), "agents"},
		"missing command":        {strings.Replace(valid, `command = "claude"`, "", 1), "command"},
		"unknown kind":           {strings.Replace(valid, `kind = "claude"`, `kind = "codex"`, 1), "codex"},
		"missing kind":           {strings.Replace(valid, `kind = "claude"`, "", 1), "kind"},
		"args picking a session": {strings.Replace(valid, `["--dangerously-skip-permissions"]`, `["--continue"]`, 1), "agents.claude.args"},
		"args not strings":       {strings.Replace(valid, `["--dangerously-skip-permissions"]`, `[1]`, 1), "Args"},
		"args as a string":       {strings.Replace(valid, `["--dangerously-skip-permissions"]`, `"--dangerously-skip-permissions"`, 1), "Args"},
		"invalid box install":    {strings.Replace(valid, "workdir =", "box_install = \"../x\"\nworkdir =", 1), "install name"},
		"empty cmux wrapper":     {strings.Replace(valid, `wrapper = "/Applications/cmux.app/Contents/Resources/bin/cmux-claude-wrapper"`, `wrapper = ""`, 1), "wrapper"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestAgentArgsOptional(t *testing.T) {
	cfg, err := Parse([]byte(strings.Replace(valid, "args = [\"--dangerously-skip-permissions\"]\n", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if agent, _ := cfg.Agent("claude"); agent.Args != nil {
		t.Fatalf("args = %q, want none", agent.Args)
	}
}

func TestAgentUnknown(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := cfg.Agent("codex"); err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("expected unknown agent error, got %v", err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	if _, err := Load("../../config.example.toml"); err != nil {
		t.Fatalf("Load: %v", err)
	}
}
