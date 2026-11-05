package box

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/claudeargs"
	"remote-agents/internal/codexargs"
	"remote-agents/internal/spec"
)

// agentKind is what the box does differently for each kind of agent whose
// session it follows.
type agentKind interface {
	// resumeArgs follow a run's resume command to resume session id.
	resumeArgs(id string) []string
	// relaunchArgs follow a run's resume command to continue rec's session
	// after the run's launcher was lost; how says what they do, for the log.
	relaunchArgs(rec runRecord) (args []string, how string, err error)
	// takesSession is the session id an agent's arguments run, or "".
	takesSession(args []string) string
	// startsWith is the session the box follows from the agent's start,
	// before its hooks report one, or "".
	startsWith(args []string) string
	// holder probes for a running agent that holds session id.
	holder(l boxlayout.Layout, env []string, id string) holderProbe
	// withHooks adds the box's hooks and its note on laptop files to the
	// agent command line of s, which runs with env.
	withHooks(l boxlayout.Layout, s spec.Spec, env []string) ([]string, error)
}

var kinds = map[spec.Kind]agentKind{spec.KindClaude: claudeKind{}, spec.KindCodex: codexKind{}}

// followed is the kind and launch description of the agent whose session s
// follows; nil for none.
func followed(s spec.Spec) (agentKind, *spec.Agent) {
	k, agent := s.Followed()
	return kinds[k], agent
}

// resumeArgv is the command line that resumes session id of the agent s follows.
func resumeArgv(s spec.Spec, id string) []string {
	kind, agent := followed(s)
	return slices.Concat(agent.Resume, kind.resumeArgs(id))
}

type claudeKind struct{}

func (claudeKind) resumeArgs(id string) []string { return claudeargs.ResumeArgs(id) }

// Claude refuses to start a session anew once it has a transcript, and has
// none to resume until a turn started.
func (claudeKind) relaunchArgs(rec runRecord) ([]string, string, error) {
	_, err := os.Stat(rec.Transcript)
	switch {
	case err == nil:
		return claudeargs.ResumeArgs(rec.AgentSession), "resuming Claude session " + rec.AgentSession, nil
	case errors.Is(err, fs.ErrNotExist):
		return []string{"--session-id", rec.AgentSession}, "starting Claude session " + rec.AgentSession + ", which has no transcript yet", nil
	}
	return nil, "", fmt.Errorf("checking the transcript of Claude session %s: %w", rec.AgentSession, err)
}

func (claudeKind) takesSession(args []string) string {
	id, _ := claudeargs.SessionOf(args)
	return id
}

// Claude reports its session at startup.
func (claudeKind) startsWith([]string) string { return "" }

func (claudeKind) holder(l boxlayout.Layout, env []string, id string) holderProbe {
	return claudeProbe(claudeSessionsDir(l, env), os.DirFS("/proc"), id)
}

func (claudeKind) withHooks(l boxlayout.Layout, s spec.Spec, _ []string) ([]string, error) {
	argv, err := withActivityHooks(l, s.Argv, s.Workdir)
	if err != nil {
		return nil, err
	}
	return withClaudeNote(argv), nil
}

type codexKind struct{}

func (codexKind) resumeArgs(id string) []string { return codexargs.ResumeArgs(id) }

// Codex writes a session's rollout before its hooks first report the session.
func (codexKind) relaunchArgs(rec runRecord) ([]string, string, error) {
	return codexargs.ResumeArgs(rec.AgentSession), "resuming Codex session " + rec.AgentSession, nil
}

func (codexKind) takesSession(args []string) string { return codexargs.SessionOf(args) }

// Codex reports a resumed session only at its next turn.
func (k codexKind) startsWith(args []string) string { return k.takesSession(args) }

func (codexKind) holder(l boxlayout.Layout, env []string, id string) holderProbe {
	return codexProbe(codexHome(l, env), id)
}

func (codexKind) withHooks(l boxlayout.Layout, s spec.Spec, env []string) ([]string, error) {
	argv, err := withCodexNote(s.Argv, codexHome(l, env))
	if err != nil {
		return nil, err
	}
	return withCodexHooks(l, argv, s.HookRelay)
}

// codexHome is where Codex keeps its sessions and config.
func codexHome(l boxlayout.Layout, env []string) string {
	if dir := envValue(env, "CODEX_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(l.Home, ".codex")
}
