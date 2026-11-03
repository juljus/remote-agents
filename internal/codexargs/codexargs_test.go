package codexargs

import (
	"slices"
	"strings"
	"testing"

	"remote-agents/internal/agentcli"
)

const existing = "01a1a1e4-7eb1-7753-927c-b59c2ec2f926"

func noID() string { panic("Codex picks its own session ids") }

func TestAnalyze(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		mode      agentcli.Mode
		sessionID string
	}{
		{"bare", nil, agentcli.ModeSession, ""},
		{"flags only", []string{"--yolo", "-m", "gpt-5"}, agentcli.ModeSession, ""},
		{"initial prompt", []string{"fix the bug"}, agentcli.ModeSession, ""},
		{"resume id", []string{"resume", existing}, agentcli.ModeSession, existing},
		{"resume id after options", []string{"--model", "o3", "resume", "--all", existing, "go on"}, agentcli.ModeSession, existing},
		{"resume options after subcommand", []string{"resume", "-m", "o3", existing}, agentcli.ModeSession, existing},
		{"resume picker", []string{"resume"}, agentcli.ModeSession, ""},
		{"resume last", []string{"resume", "--last"}, agentcli.ModeSession, ""},
		{"resume last with prompt", []string{"resume", "--last", existing}, agentcli.ModeSession, ""},
		{"resume by name", []string{"resume", "auth bug"}, agentcli.ModeSession, ""},
		{"fork", []string{"fork", existing}, agentcli.ModeSession, ""},
		{"value option hides subcommand name", []string{"--model", "exec"}, agentcli.ModeSession, ""},
		{"prompt then subcommand word", []string{"fix it", "exec"}, agentcli.ModeSession, ""},
		{"images take every value up to an option", []string{"-i", "a.png", "b.png", "--yolo"}, agentcli.ModeSession, ""},
		{"double dash stops parsing", []string{"--", "resume", existing}, agentcli.ModeSession, ""},
		{"exec", []string{"exec", "hi"}, agentcli.ModeCommand, ""},
		{"exec alias", []string{"e", "hi"}, agentcli.ModeCommand, ""},
		{"login", []string{"login", "--device-auth"}, agentcli.ModeCommand, ""},
		{"subcommand after option", []string{"-c", "model=\"o3\"", "mcp", "list"}, agentcli.ModeCommand, ""},
		{"help", []string{"--help"}, agentcli.ModeCommand, ""},
		{"version short", []string{"-V"}, agentcli.ModeCommand, ""},
		{"resume help", []string{"resume", "--help"}, agentcli.ModeCommand, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Analyze(tc.args)
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if plan.Mode != tc.mode {
				t.Fatalf("mode = %v, want %v", plan.Mode, tc.mode)
			}
			if !slices.Equal(plan.Args, tc.args) {
				t.Fatalf("args = %q, want them unchanged", plan.Args)
			}
			if plan.SessionID != tc.sessionID || plan.Resumes != (tc.sessionID != "") {
				t.Fatalf("session = %q resumes=%v, want %q", plan.SessionID, plan.Resumes, tc.sessionID)
			}
		})
	}
}

// The box runs Codex with hooks of its own; these would replace or disable them.
func TestAnalyzeRefusesHookOverrides(t *testing.T) {
	for _, args := range [][]string{
		{"-c", "hooks.Stop=[]"},
		{"-c", "hooks={}"},
		{"--config", " hooks.state={}"},
		{"--config=hooks.Stop=[]"},
		{"-chooks={}"},
		{"-c", "features.hooks=false"},
		{"--disable", "hooks"},
		{"--disable=hooks"},
		{"resume", "-c", "hooks={}", existing},
		{"fix", "-c", "hooks={}"},
	} {
		if _, err := Analyze(args); err == nil || !strings.Contains(err.Error(), "hooks") {
			t.Fatalf("Analyze(%q) = %v, want a refusal naming hooks", args, err)
		}
	}
	for _, args := range [][]string{
		{"-c", "hooksmith=1"},
		{"-c", "model=\"hooks\""},
		{"--disable", "web_search"},
		{"--", "-c", "hooks={}"},
	} {
		if _, err := Analyze(args); err != nil {
			t.Fatalf("Analyze(%q): %v", args, err)
		}
	}
}

func TestResumeBase(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"new session", []string{"--yolo", "-m", "o3", "fix it"}, []string{"--yolo", "-m", "o3"}},
		{"joined values", []string{"--model=o3", "-cmodel=\"o3\"", "prompt"}, []string{"--model=o3", "-cmodel=\"o3\""}},
		{"resume", []string{"-c", "x=1", "resume", "-m", "o3", "--all", existing, "go on"}, []string{"-c", "x=1", "-m", "o3"}},
		{"resume last", []string{"resume", "--last", "--include-non-interactive"}, []string{}},
		{"fork", []string{"fork", "--last", "-s", "read-only"}, []string{"-s", "read-only"}},
		{"images belong to the first prompt", []string{"-i", "a.png", "b.png", "--search", "look"}, []string{"--search"}},
		{"a new worktree is not made again", []string{"--worktree", "--add-dir", "/x"}, []string{"--add-dir", "/x"}},
		{"double dash ends options", []string{"--search", "--", "--yolo"}, []string{"--search"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResumeBase(tc.args); !slices.Equal(got, tc.want) {
				t.Fatalf("ResumeBase(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestResumeArgs(t *testing.T) {
	if got := ResumeArgs(existing); !slices.Equal(got, []string{"resume", existing}) {
		t.Fatalf("ResumeArgs = %q", got)
	}
}

func TestSessionOf(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"--no-daemon", "resume", existing}, existing},
		{[]string{"resume", "--last"}, ""},
		{[]string{"fork", existing}, ""},
		{[]string{"say", existing}, ""},
	} {
		if got := SessionOf(tc.argv); got != tc.want {
			t.Fatalf("SessionOf(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

func TestCheckDefaults(t *testing.T) {
	for _, args := range [][]string{
		{"--yolo"},
		{"--model=o3", "--search", "--config=model_reasoning_effort=\"high\""},
	} {
		if err := CheckDefaults(args); err != nil {
			t.Fatalf("CheckDefaults(%q): %v", args, err)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{""}, "empty"},
		{[]string{"resume"}, "not an option"},
		{[]string{"--"}, "not an option"},
		{[]string{"--help"}, "one-shot"},
		{[]string{"-V"}, "one-shot"},
		{[]string{"--model"}, "--model=<value>"},
		{[]string{"-m=o3"}, "long form"},
		{[]string{"-mo3"}, "long form"},
		{[]string{"--config=hooks={}"}, "hooks"},
		{[]string{"--disable=hooks"}, "hooks"},
	} {
		err := CheckDefaults(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("CheckDefaults(%q) = %v, want an error containing %q", tc.args, err, tc.want)
		}
	}
}

func TestCLINeedsNoInjectedID(t *testing.T) {
	plan, err := CLI{}.Analyze([]string{"hi"}, noID)
	if err != nil || plan.SessionID != "" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
}

func TestSignInArgs(t *testing.T) {
	if got := (CLI{}).SignInArgs(); !slices.Equal(got, []string{"login"}) {
		t.Fatalf("got %q", got)
	}
}

func TestCodexHasNoHello(t *testing.T) {
	if got, ok := (CLI{}).HelloArgs(); ok {
		t.Fatalf("got %q", got)
	}
}
