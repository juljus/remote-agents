package cmuxtranscript

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	idA = "aaaaaaaa-1111-4222-8333-444444444444"
	idB = "bbbbbbbb-1111-4222-8333-444444444444"
)

func hook(id, transcript string) []byte {
	return []byte(`{"session_id":"` + id + `","transcript_path":"` + transcript + `","hook_event_name":"SessionStart"}`)
}

func boxTranscript(id string) string {
	return "/home/me/.claude/projects/-home-me-work/" + id + ".jsonl"
}

func standInPath(claudeDir, id string) string {
	return filepath.Join(claudeDir, "projects", "-home-me-work", id+".jsonl")
}

func TestWritesAStandInInTheBoxProjectDirectory(t *testing.T) {
	s := &StandIns{ClaudeDir: t.TempDir()}
	if err := s.Observe(hook(idA, boxTranscript(idA))); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(standInPath(s.ClaudeDir, idA))
	if err != nil {
		t.Fatal(err)
	}
	// cmux takes only a regular file that is not empty.
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("stand-in is %v, %d bytes", info.Mode(), info.Size())
	}
}

// With the same home on both sides a copied transcript can be at the path.
func TestLeavesAnExistingFileAlone(t *testing.T) {
	s := &StandIns{ClaudeDir: t.TempDir()}
	path := standInPath(s.ClaudeDir, idA)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("real transcript\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Observe(hook(idA, boxTranscript(idA))); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "real transcript\n" {
		t.Fatalf("file holds %q, %v", data, err)
	}
}

// /clear starts another session in the same wrapper.
func TestEachSessionGetsItsStandInOnce(t *testing.T) {
	s := &StandIns{ClaudeDir: t.TempDir()}
	for _, id := range []string{idA, idB} {
		if err := s.Observe(hook(id, boxTranscript(id))); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(standInPath(s.ClaudeDir, idA)); err != nil {
		t.Fatal(err)
	}
	if err := s.Observe(hook(idA, boxTranscript(idA))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(standInPath(s.ClaudeDir, idA)); !os.IsNotExist(err) {
		t.Fatalf("a later hook wrote session A's stand-in again: %v", err)
	}
	if _, err := os.Stat(standInPath(s.ClaudeDir, idB)); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadWithoutATranscriptWritesNothing(t *testing.T) {
	s := &StandIns{ClaudeDir: t.TempDir()}
	if err := s.Observe([]byte(`{"session_id":"` + idA + `","hook_event_name":"Notification"}`)); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(s.ClaudeDir); err != nil || len(entries) != 0 {
		t.Fatalf("Claude directory holds %v, %v", entries, err)
	}
	// The session is not counted as tried: a later hook names the transcript.
	if err := s.Observe(hook(idA, boxTranscript(idA))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(standInPath(s.ClaudeDir, idA)); err != nil {
		t.Fatal(err)
	}
}

// The box picks the path, and it must not pick where the laptop writes.
func TestRefusesAPathThatIsNotTheSessionsTranscript(t *testing.T) {
	for name, payload := range map[string][]byte{
		"not a session id":        hook("../../x", "/home/me/.claude/projects/p/../../x.jsonl"),
		"another file":            hook(idA, "/home/me/.claude/projects/p/settings.json"),
		"another session":         hook(idA, boxTranscript(idB)),
		"parent as project":       hook(idA, "/home/me/.claude/projects/../"+idA+".jsonl"),
		"no project directory":    hook(idA, "/"+idA+".jsonl"),
		"relative path":           hook(idA, "projects/p/"+idA+".jsonl"),
		"backslash in project":    hook(idA, `/home/me/.claude/projects/a\\b/`+idA+".jsonl"),
		"payload that is no JSON": []byte("x"),
	} {
		t.Run(name, func(t *testing.T) {
			s := &StandIns{ClaudeDir: t.TempDir()}
			if err := s.Observe(payload); err == nil {
				t.Fatal("expected an error")
			}
			if entries, err := os.ReadDir(s.ClaudeDir); err != nil || len(entries) != 0 {
				t.Fatalf("Claude directory holds %v, %v", entries, err)
			}
		})
	}
}
