package spec

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func validSession() Spec {
	return Spec{
		Mode:      ModeSession,
		Session:   "claude-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		RunID:     "11111111-2222-4333-8444-555555555555",
		Attempt:   2,
		Workdir:   "/home/agent/work",
		Argv:      []string{"claude", "--session-id", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "it's \"quoted\" $HOME"},
		Env:       map[string]string{"TERM": "xterm-256color"},
		HookRelay: true,
		Claude: &Agent{
			Resume:  []string{"claude", "--dangerously-skip-permissions"},
			Resumes: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		},
	}
}

func TestRoundTrip(t *testing.T) {
	in := validSession()
	enc, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.ContainsAny(enc, " '\"$\\\n") {
		t.Fatalf("encoding is not shell-safe: %q", enc)
	}
	out, err := Decode(enc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n%+v\n%+v", in, out)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*Spec){
		"unknown mode":      func(s *Spec) { s.Mode = "bogus" },
		"relative workdir":  func(s *Spec) { s.Workdir = "work" },
		"empty argv":        func(s *Spec) { s.Argv = nil },
		"bad session name":  func(s *Spec) { s.Session = "claude/x" },
		"missing session":   func(s *Spec) { s.Session = "" },
		"bad run id":        func(s *Spec) { s.RunID = "nope" },
		"exec with session": func(s *Spec) { s.Mode = ModeExec },
		"empty resume":      func(s *Spec) { s.Claude.Resume = nil },
		"blank resume":      func(s *Spec) { s.Claude.Resume = []string{""} },
		"bad resumes":       func(s *Spec) { s.Claude.Resumes = "x" },
		"reattach claude":   func(s *Spec) { s.Mode, s.Argv, s.RunID = ModeReattach, nil, "" },
		"negative attempt":  func(s *Spec) { s.Attempt = -1 },
		"exec attempt": func(s *Spec) {
			s.Mode, s.Session, s.RunID, s.Claude, s.Attempt = ModeExec, "", "", nil, 1
		},
		"claude and codex": func(s *Spec) { s.Codex = &Agent{Resume: []string{"codex"}} },
		"reattach codex": func(s *Spec) {
			s.Mode, s.Argv, s.RunID, s.Claude, s.Codex = ModeReattach, nil, "", nil, &Agent{Resume: []string{"codex"}}
		},
		"exec codex": func(s *Spec) {
			s.Mode, s.Session, s.RunID, s.Attempt, s.Claude, s.Codex = ModeExec, "", "", 0, nil, &Agent{Resume: []string{"codex"}}
		},
		"bad codex resumes":   func(s *Spec) { s.Claude, s.Codex = nil, &Agent{Resume: []string{"codex"}, Resumes: "x"} },
		"reattach bad run id": func(s *Spec) { s.Mode, s.Argv, s.Claude, s.RunID = ModeReattach, nil, nil, "nope" },
		"attempt without run": func(s *Spec) { s.Mode, s.Argv, s.Claude, s.RunID, s.Attempt = ModeReattach, nil, nil, "", 2 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := validSession()
			mutate(&s)
			if err := s.Validate(); err == nil {
				t.Fatalf("expected validation error")
			}
			if _, err := s.Encode(); err == nil {
				t.Fatalf("Encode accepted an invalid spec")
			}
		})
	}
}

func TestValidCodexSession(t *testing.T) {
	s := validSession()
	s.Session = "codex-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	s.Argv = []string{"codex", "resume", "01a1a1e4-7eb1-7753-927c-b59c2ec2f926"}
	s.Claude, s.Codex = nil, &Agent{Resume: []string{"codex", "--yolo"}, Resumes: "01a1a1e4-7eb1-7753-927c-b59c2ec2f926"}
	enc, err := s.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(enc)
	if err != nil || !reflect.DeepEqual(s, out) {
		t.Fatalf("round trip = %+v, %v", out, err)
	}
}

func TestFollowNamesTheAgentsKind(t *testing.T) {
	var s Spec
	if err := s.Follow(KindCodex, &Agent{Resume: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	if k, a := s.Followed(); k != KindCodex || a != s.Codex || s.Claude != nil {
		t.Fatalf("followed %q %+v", k, a)
	}
	if err := s.Follow("aider", &Agent{}); err == nil {
		t.Fatal("followed an unknown kind")
	}
}

func TestValidReattach(t *testing.T) {
	for _, s := range []Spec{
		{Mode: ModeReattach, Session: "claude-x"},
		{Mode: ModeReattach, Session: "claude-x", RunID: "11111111-2222-4333-8444-555555555555", Attempt: 3},
	} {
		if err := s.Validate(); err != nil {
			t.Fatalf("Validate(%+v): %v", s, err)
		}
	}
}

func TestValidExec(t *testing.T) {
	s := Spec{Mode: ModeExec, Workdir: "/w", Argv: []string{"claude", "--version"}}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	raw := base64.RawURLEncoding.EncodeToString([]byte(`{"mode":"exec","workdir":"/w","argv":["x"],"extra":1}`))
	if _, err := Decode(raw); err == nil || !strings.Contains(err.Error(), "extra") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestSessionName(t *testing.T) {
	if got := SessionName("claude", "abc"); got != "claude-abc" {
		t.Fatalf("SessionName = %q", got)
	}
}

func TestAgentOf(t *testing.T) {
	for name, want := range map[string]string{
		"claude-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee":      "claude",
		"fake-claude-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee": "fake-claude",
	} {
		if got, err := AgentOf(name); err != nil || got != want {
			t.Errorf("AgentOf(%q) = %q, %v", name, got, err)
		}
	}
	for _, name := range []string{"claude", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "claude-not-a-uuid-at-all-really-not-a-uuid"} {
		if _, err := AgentOf(name); err == nil {
			t.Errorf("AgentOf(%q) accepted", name)
		}
	}
}
