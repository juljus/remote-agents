package claudeargs

import (
	"slices"
	"testing"
)

const (
	generated = "11111111-2222-4333-8444-555555555555"
	existing  = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

func newID() string { return generated }

func TestAnalyze(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		mode      Mode
		wantArgs  []string
		sessionID string
	}{
		{"bare", nil, ModeSession, []string{"--session-id", generated}, generated},
		{"flags only", []string{"--dangerously-skip-permissions"}, ModeSession,
			[]string{"--session-id", generated, "--dangerously-skip-permissions"}, generated},
		{"initial prompt", []string{"fix the bug"}, ModeSession,
			[]string{"--session-id", generated, "fix the bug"}, generated},
		{"resume id", []string{"--resume", existing}, ModeSession, []string{"--resume", existing}, existing},
		{"resume short", []string{"-r", existing}, ModeSession, []string{"-r", existing}, existing},
		{"resume equals", []string{"--resume=" + existing}, ModeSession, []string{"--resume=" + existing}, existing},
		{"resume after flag with value", []string{"--model", "opus", "--resume", existing}, ModeSession,
			[]string{"--model", "opus", "--resume", existing}, existing},
		{"resume picker", []string{"--resume"}, ModeSession, []string{"--resume"}, ""},
		{"resume picker then flag", []string{"--resume", "--dangerously-skip-permissions"}, ModeSession,
			[]string{"--resume", "--dangerously-skip-permissions"}, ""},
		{"resume search term", []string{"--resume", "auth bug"}, ModeSession, []string{"--resume", "auth bug"}, ""},
		{"continue", []string{"--continue"}, ModeSession, []string{"--continue"}, ""},
		{"continue short", []string{"-c"}, ModeSession, []string{"-c"}, ""},
		{"explicit session id", []string{"--session-id", existing}, ModeSession, []string{"--session-id", existing}, existing},
		{"explicit session id equals", []string{"--session-id=" + existing}, ModeSession, []string{"--session-id=" + existing}, existing},
		{"fork from resume", []string{"--resume", existing, "--fork-session"}, ModeSession,
			[]string{"--session-id", generated, "--resume", existing, "--fork-session"}, generated},
		{"fork from continue", []string{"-c", "--fork-session"}, ModeSession,
			[]string{"--session-id", generated, "-c", "--fork-session"}, generated},
		{"version", []string{"--version"}, ModeCommand, []string{"--version"}, ""},
		{"help short", []string{"-h"}, ModeCommand, []string{"-h"}, ""},
		{"print", []string{"-p", "hello"}, ModeCommand, []string{"-p", "hello"}, ""},
		{"print with resume", []string{"--resume", existing, "--print", "hi"}, ModeCommand,
			[]string{"--resume", existing, "--print", "hi"}, ""},
		{"subcommand", []string{"mcp", "list"}, ModeCommand, []string{"mcp", "list"}, ""},
		{"subcommand after flag", []string{"--debug", "doctor"}, ModeCommand, []string{"--debug", "doctor"}, ""},
		{"value option hides subcommand name", []string{"--model", "update"}, ModeSession,
			[]string{"--session-id", generated, "--model", "update"}, generated},
		{"prompt then subcommand word", []string{"fix it", "update"}, ModeSession,
			[]string{"--session-id", generated, "fix it", "update"}, generated},
		{"double dash stops parsing", []string{"--", "--resume"}, ModeSession,
			[]string{"--session-id", generated, "--", "--resume"}, generated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Analyze(tc.args, newID)
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if plan.Mode != tc.mode {
				t.Fatalf("mode = %v, want %v", plan.Mode, tc.mode)
			}
			if !slices.Equal(plan.Args, tc.wantArgs) {
				t.Fatalf("args = %q, want %q", plan.Args, tc.wantArgs)
			}
			if plan.SessionID != tc.sessionID {
				t.Fatalf("session id = %q, want %q", plan.SessionID, tc.sessionID)
			}
		})
	}
}

func TestAnalyzeTellsResumesFromNewSessions(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		resumes bool
	}{
		{[]string{"--resume", existing}, true},
		{[]string{"--resume=" + existing}, true},
		{nil, false},
		{[]string{"--session-id", existing}, false},
		{[]string{"--resume", existing, "--fork-session"}, false},
		{[]string{"--continue"}, false},
	} {
		plan, err := Analyze(tc.args, newID)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Resumes != tc.resumes {
			t.Fatalf("Analyze(%q).Resumes = %v", tc.args, plan.Resumes)
		}
	}
}

func TestAnalyzeRejectsNonUUIDSessionID(t *testing.T) {
	if _, err := Analyze([]string{"--session-id", "not-a-uuid"}, newID); err == nil {
		t.Fatal("expected error for non-UUID --session-id")
	}
}

func TestAnalyzeDoesNotMutateInput(t *testing.T) {
	in := []string{"--model", "opus"}
	if _, err := Analyze(in, newID); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !slices.Equal(in, []string{"--model", "opus"}) {
		t.Fatalf("input mutated: %q", in)
	}
}

func TestCheckDefaults(t *testing.T) {
	for _, ok := range [][]string{
		nil,
		{"--dangerously-skip-permissions"},
		{"--model=opus", "--permission-mode=plan", "--allowedTools=Bash"},
	} {
		if err := CheckDefaults(ok); err != nil {
			t.Fatalf("CheckDefaults(%q): %v", ok, err)
		}
	}
	for _, bad := range [][]string{
		{"--resume"}, {"-c"}, {"--continue"}, {"--fork-session"},
		{"--session-id", "aaaaaaaa-1111-4222-8333-444444444444"},
		{"--print"}, {"--version"}, {"mcp"}, {"hello"},
		{"--"}, {"--model"}, {"--model", "opus"}, {"--allowedTools", "Bash"}, {"-m"}, {"-m=opus"}, {"-n=x"}, {""},
	} {
		if err := CheckDefaults(bad); err == nil {
			t.Fatalf("CheckDefaults(%q) should fail", bad)
		}
	}
}

func TestResumeBaseKeepsOptionsButNotTheSessionOrPrompt(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"--dangerously-skip-permissions", "--session-id", "a-b", "--model", "opus"}, []string{"--dangerously-skip-permissions", "--model", "opus"}},
		{[]string{"--settings", "{}", "--resume", "a-b"}, []string{"--settings", "{}"}},
		{[]string{"-r", "a-b", "--resume=c-d", "--session-id=e-f", "--model=x"}, []string{"--model=x"}},
		{[]string{"--continue", "-c", "--fork-session", "--verbose"}, []string{"--verbose"}},
		{[]string{"--resume", "--model", "x"}, []string{"--model", "x"}},
		{[]string{"--model", "x", "fix the bug", "--verbose"}, []string{"--model", "x", "--verbose"}},
		{[]string{"--verbose", "--", "--not-an-option"}, []string{"--verbose"}},
	}
	for _, tc := range cases {
		if got := ResumeBase(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("ResumeBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
