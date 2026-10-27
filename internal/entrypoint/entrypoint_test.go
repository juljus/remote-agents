package entrypoint

import "testing"

func TestName(t *testing.T) {
	if got := Name("claude"); got != "rclaude" {
		t.Fatalf("Name(claude) = %q", got)
	}
}

func TestAgent(t *testing.T) {
	for argv0, want := range map[string]string{
		"rclaude":                           "claude",
		"/Users/x/.local/bin/rclaude":       "claude",
		"/tmp/ra-e2e-1/bin/rfakeclaude":     "fakeclaude",
		"remote-agents":                     "",
		"/Users/x/.local/bin/remote-agents": "",
		"r":                                 "",
		"claude":                            "",
	} {
		got, ok := Agent(argv0)
		if got != want || ok != (want != "") {
			t.Fatalf("Agent(%q) = %q, %v; want %q", argv0, got, ok, want)
		}
	}
}
