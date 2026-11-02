package box

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/spec"
)

const otherRunID = "99999999-2222-4333-8444-555555555555"

func attempt(run string, n int) spec.Spec {
	s := sessionSpec("/w", "claude")
	s.RunID, s.Attempt = run, n
	return s
}

func mustClaim(t *testing.T, l boxlayout.Layout, s spec.Spec, want bool) {
	t.Helper()
	got, err := claimAttachment(l, s)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("claimAttachment(run %s attempt %d) = %v, want %v", s.RunID, s.Attempt, got, want)
	}
}

func mustOwn(t *testing.T, l boxlayout.Layout, s spec.Spec, want bool) {
	t.Helper()
	got, err := ownsAttachment(l, s)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ownsAttachment(run %s attempt %d) = %v, want %v", s.RunID, s.Attempt, got, want)
	}
}

func TestStaleAttemptCannotClaimTheSessionFromANewerOne(t *testing.T) {
	l := testLayout(t)
	mustClaim(t, l, attempt(runID, 2), true)
	mustClaim(t, l, attempt(runID, 1), false)
	mustOwn(t, l, attempt(runID, 2), true)
	mustOwn(t, l, attempt(runID, 1), false)
	mustClaim(t, l, attempt(runID, 3), true)
	mustOwn(t, l, attempt(runID, 2), false)
}

func TestAnotherRunTakesTheSessionOver(t *testing.T) {
	l := testLayout(t)
	mustClaim(t, l, attempt(runID, 5), true)
	mustClaim(t, l, attempt(otherRunID, 1), true)
	mustOwn(t, l, attempt(runID, 5), false)
	// The first run's next attempt is a takeover of its own.
	mustClaim(t, l, attempt(runID, 6), true)
	mustOwn(t, l, attempt(otherRunID, 1), false)
}

func TestUnnumberedAttemptsTakeOverAndOwnNothing(t *testing.T) {
	l := testLayout(t)
	mustClaim(t, l, attempt(runID, 2), true)
	reattach := spec.Spec{Mode: spec.ModeReattach, Session: attempt(runID, 2).Session}
	mustClaim(t, l, reattach, true)
	mustOwn(t, l, attempt(runID, 2), false)
	mustOwn(t, l, reattach, false)
	mustClaim(t, l, attempt(runID, 1), true)
}

// fakeShpool installs a shpool that logs each attach, runs onAttach then
// exits 0 from it, and lists the session while fewer than listedUntil
// attaches have run.
func fakeShpool(t *testing.T, l boxlayout.Layout, listedUntil int, onAttach string) string {
	t.Helper()
	log := filepath.Join(l.Home, "attaches")
	name := attempt(runID, 1).Session
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    attach) echo attach >> '` + log + `'; ` + onAttach + `; exit 0 ;;
    list)
      n=$(wc -l < '` + log + `' 2>/dev/null || echo 0)
      if [ "$n" -lt ` + strconv.Itoa(listedUntil) + ` ]; then
        echo '{"sessions":[{"name":"` + name + `","started_at_unix_ms":1,"status":"Attached"}]}'
      else
        echo '{"sessions":[]}'
      fi
      exit 0 ;;
  esac
done
exit 9
`
	if err := os.MkdirAll(filepath.Dir(l.Shpool()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Shpool(), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return log
}

func attaches(t *testing.T, log string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "attach\n")
}

func TestAttachRefusesAStaleAttemptWithoutTouchingTheSession(t *testing.T) {
	l := testLayout(t)
	log := fakeShpool(t, l, 9, ":")
	mustClaim(t, l, attempt(runID, 4), true)
	var stderr bytes.Buffer
	code, err := Attach(l, attempt(runID, 3), IO{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &stderr})
	if err != nil || code != spec.ExitStaleAttempt {
		t.Fatalf("Attach = %d, %v; stderr %q", code, err, stderr.String())
	}
	if n := attaches(t, log); n != 0 {
		t.Fatalf("a stale attempt ran shpool attach %d times", n)
	}
	mustOwn(t, l, attempt(runID, 4), true)
}

func TestAttachTakesTheSessionBackFromAStaleAttemptOfItsRun(t *testing.T) {
	l := testLayout(t)
	// The first attach ends with the session still running: something took it
	// over without claiming it, which only a stale attempt of this run does.
	log := fakeShpool(t, l, 2, ":")
	var stderr bytes.Buffer
	code, err := Attach(l, attempt(runID, 2), IO{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &stderr})
	if err != nil || code != 0 {
		t.Fatalf("Attach = %d, %v; stderr %q", code, err, stderr.String())
	}
	if n := attaches(t, log); n != 2 {
		t.Fatalf("shpool attach ran %d times, want 2", n)
	}
	if strings.Contains(stderr.String(), "took over") {
		t.Fatalf("reported a takeover: %q", stderr.String())
	}
}

func TestAttachReportsATakeoverByAnotherRun(t *testing.T) {
	l := testLayout(t)
	// While this attempt is attached, another run claims the session.
	claim := `printf '{"run_id":"` + otherRunID + `","attempt":1}' > '` + l.Attachment(attempt(runID, 1).Session) + `'`
	log := fakeShpool(t, l, 9, claim)
	var stderr bytes.Buffer
	code, err := Attach(l, attempt(runID, 1), IO{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &stderr})
	if err != nil || code != spec.ExitTakenOver {
		t.Fatalf("Attach = %d, %v; stderr %q", code, err, stderr.String())
	}
	if n := attaches(t, log); n != 1 || !strings.Contains(stderr.String(), "another terminal took over") {
		t.Fatalf("attaches %d, stderr %q", n, stderr.String())
	}
}

func TestAttachReportsAgentExitsTheBoxReservesAs254(t *testing.T) {
	for _, code := range []int{spec.ExitTakenOver, spec.ExitStaleAttempt, 255} {
		l := testLayout(t)
		fakeShpool(t, l, 0, "exit "+strconv.Itoa(code))
		var stderr bytes.Buffer
		got, err := Attach(l, attempt(runID, 1), IO{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &stderr})
		if err != nil || got != spec.ExitAgentReserved || !strings.Contains(stderr.String(), "status "+strconv.Itoa(code)+",") {
			t.Fatalf("agent exit %d: Attach = %d, %v; stderr %q", code, got, err, stderr.String())
		}
	}
}
