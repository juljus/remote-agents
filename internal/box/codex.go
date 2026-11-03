package box

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/cmuxcodex"
	"remote-agents/internal/reaper"
)

// codexEventLabels are the names Codex keys a hook event's trust under.
var codexEventLabels = map[string]string{
	"SessionStart": "session_start", "UserPromptSubmit": "user_prompt_submit", "Stop": "stop",
	"Interrupt": "interrupt", "PermissionRequest": "permission_request", "PreToolUse": "pre_tool_use",
	"PostToolUse": "post_tool_use", "PreCompact": "pre_compact", "PostCompact": "post_compact",
	"SubagentStart": "subagent_start", "SubagentStop": "subagent_stop",
}

// Codex allows Interrupt hooks at most three seconds.
const (
	codexHookTimeout      = 10
	codexInterruptTimeout = 3
)

type codexHook struct {
	event   string
	command string
	// timeout is in seconds; Codex counts it in a hook's trust.
	timeout int
}

// codexEventLabel is the name Codex keys event's trust under.
func codexEventLabel(event string) (string, error) {
	label, ok := codexEventLabels[event]
	if !ok {
		return "", fmt.Errorf("no Codex hook event %q", event)
	}
	return label, nil
}

// codexHookKey is the key under which Codex looks up the trust of a hook
// given in a -c override: group of event's hooks in the session flags.
func codexHookKey(event string, group int) (string, error) {
	label, err := codexEventLabel(event)
	if err != nil {
		return "", err
	}
	return "/<session-flags>/config.toml:" + label + ":" + strconv.Itoa(group) + ":0", nil
}

// codexTrustHash is the hash under which Codex trusts h without asking: the
// SHA-256 of h's normalized declaration as JSON with sorted keys, which
// cmux computes too to trust the hooks it installs. Go and Codex encode
// only printable ASCII alike.
func codexTrustHash(h codexHook) (string, error) {
	if strings.IndexFunc(h.command, func(r rune) bool { return r < ' ' || r > '~' }) >= 0 {
		return "", fmt.Errorf("hook command %q is not printable ASCII", h.command)
	}
	label, err := codexEventLabel(h.event)
	if err != nil {
		return "", err
	}
	identity := map[string]any{
		"event_name": label,
		"hooks":      []map[string]any{{"async": false, "command": h.command, "timeout": h.timeout, "type": "command"}},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(identity); err != nil {
		return "", fmt.Errorf("encoding hook identity: %w", err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// tomlString quotes s as a TOML basic string; s is printable ASCII.
func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// codexHooks are the hooks the box gives Codex, by event in a fixed order:
// those that report activity to the box, then with relay cmux's own.
func codexHooks(l boxlayout.Layout, relay bool) (events []string, hooks map[string][]codexHook) {
	hooks = map[string][]codexHook{}
	add := func(h codexHook) {
		if _, ok := hooks[h.event]; !ok {
			events = append(events, h.event)
		}
		hooks[h.event] = append(hooks[h.event], h)
	}
	activity := shellQuote(l.Binary()) + " activity"
	for _, event := range reaper.CodexHookEvents {
		timeout := codexHookTimeout
		if event == "Interrupt" {
			timeout = codexInterruptTimeout
		}
		add(codexHook{event: event, command: activity, timeout: timeout})
	}
	if relay {
		cmux := shellQuote(filepath.Join(l.HookBin(), "cmux"))
		for _, h := range cmuxcodex.Hooks {
			add(codexHook{event: h.Event, command: cmux + " " + strings.Join(h.Argv, " "), timeout: h.Timeout})
		}
	}
	return events, hooks
}

// codexHooksOverride is the -c value that gives Codex the box's hooks, each
// trusted, so that Codex runs them without asking and without a change to
// the user's own config.
func codexHooksOverride(l boxlayout.Layout, relay bool) (string, error) {
	events, hooks := codexHooks(l, relay)
	var decls, trust []string
	for _, event := range events {
		var groups []string
		for g, h := range hooks[event] {
			hash, err := codexTrustHash(h)
			if err != nil {
				return "", err
			}
			key, err := codexHookKey(event, g)
			if err != nil {
				return "", err
			}
			groups = append(groups, fmt.Sprintf(`{hooks=[{type="command",command=%s,timeout=%d}]}`, tomlString(h.command), h.timeout))
			trust = append(trust, fmt.Sprintf(`%s={trusted_hash=%s}`, tomlString(key), tomlString(hash)))
		}
		decls = append(decls, event+"=["+strings.Join(groups, ",")+"]")
	}
	return "hooks={" + strings.Join(decls, ",") + ",state={" + strings.Join(trust, ",") + "}}", nil
}

// withCodexHooks adds the box's hooks to a codex command line, enables
// hooks should the user's config disable them, and keeps Codex from its
// shared background server: the session's agent, its memory cap and its
// tools all stay in the session.
func withCodexHooks(l boxlayout.Layout, argv []string, relay bool) ([]string, error) {
	override, err := codexHooksOverride(l, relay)
	if err != nil {
		return nil, err
	}
	front := []string{"--no-daemon", "--enable", "hooks", "-c", override}
	options := argv[1:]
	if end := slices.Index(options, "--"); end >= 0 {
		options = options[:end]
	}
	if slices.Contains(options, "--no-daemon") {
		front = front[1:]
	}
	return slices.Concat(argv[:1], front, argv[1:]), nil
}

// errCodexCoordinating: a Codex holds the coordination lock, which it does
// only briefly; a later probe may get it.
var errCodexCoordinating = errors.New("a Codex holds the coordination lock of its session locks")

// codexHeld reports whether a running Codex holds session id. A Codex
// writing a session holds an exclusive flock on its lock file, which it
// takes only under the coordination lock; probing under that lock cannot
// race a Codex taking the session; a Codex that wants the coordination lock
// meanwhile retries it for 30 seconds.
func codexHeld(home, id string) (bool, error) {
	held := false
	err := underCodexCoordination(home, func(dir string) (err error) {
		held, err = codexLockHeld(filepath.Join(dir, id+".lock"))
		return err
	})
	return held, err
}

// codexHeldSessions are the sessions running Codex processes hold, sorted.
func codexHeldSessions(home string) ([]string, error) {
	var held []string
	err := underCodexCoordination(home, func(dir string) error {
		locks, err := filepath.Glob(filepath.Join(dir, "*.lock"))
		if err != nil {
			return err
		}
		for _, lock := range locks {
			id := strings.TrimSuffix(filepath.Base(lock), ".lock")
			if strings.HasPrefix(id, ".") {
				continue
			}
			ok, err := codexLockHeld(lock)
			if err != nil {
				return err
			}
			if ok {
				held = append(held, id)
			}
		}
		return nil
	})
	slices.Sort(held)
	return held, err
}

// underCodexCoordination runs fn on Codex's session lock dir holding the
// coordination lock of its session locks. Without a coordination lock file,
// no Codex has taken a session lock, and fn does not run.
func underCodexCoordination(home string, fn func(dir string) error) error {
	dir := filepath.Join(home, "thread-writer-locks")
	coord, err := os.OpenFile(filepath.Join(dir, ".coordination.lock"), os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening Codex's coordination lock: %w", err)
	}
	defer coord.Close()
	err = syscall.Flock(int(coord.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errCodexCoordinating
	}
	if err != nil {
		return fmt.Errorf("taking Codex's coordination lock: %w", err)
	}
	return fn(dir)
}

// codexLockHeld reports whether a running Codex holds the session lock at path.
func codexLockHeld(path string) (bool, error) {
	lock, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("opening Codex's session lock %s: %w", filepath.Base(path), err)
	}
	defer lock.Close()
	err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("probing Codex's session lock %s: %w", filepath.Base(path), err)
	}
	return false, nil
}

func codexProbe(home, id string) holderProbe {
	return func() (string, error, error) {
		held, err := codexHeld(home, id)
		switch {
		case errors.Is(err, errCodexCoordinating):
			return "", err, nil
		case err != nil || !held:
			return "", nil, err
		}
		return "a running Codex", nil, nil
	}
}
