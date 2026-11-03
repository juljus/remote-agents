// Package codexargs understands the Codex CLI command line well enough to
// decide whether an invocation is an interactive session and which session
// it resumes. Codex picks a new session's id itself; the box learns it from
// Codex's SessionStart hook.
package codexargs

import (
	"errors"
	"fmt"
	"strings"

	"remote-agents/internal/agentcli"
	"remote-agents/internal/claudeargs"
)

// Options that take a value, as codex-cli 0.166 defines them. --image takes
// every following argument up to the next option.
var (
	valueLong = map[string]bool{
		"--config": true, "--enable": true, "--disable": true, "--remote": true,
		"--remote-auth-token-env": true, "--model": true, "--local-provider": true,
		"--profile": true, "--sandbox": true, "--cd": true, "--add-dir": true,
		"--ask-for-approval": true, "--image": true,
	}
	valueShort = map[string]bool{"-c": true, "-m": true, "-p": true, "-s": true, "-C": true, "-a": true, "-i": true}
)

// Subcommands that run once instead of an interactive session; resume and
// fork start one.
var subcommands = map[string]bool{
	"agents": true, "tcp-tunnel": true, "exec": true, "e": true, "review": true, "login": true,
	"logout": true, "mcp": true, "plugin": true, "app-server": true, "remote-control": true,
	"app": true, "completion": true, "update": true, "doctor": true, "sandbox": true,
	"debug": true, "execpolicy": true, "apply": true, "a": true, "queue": true, "archive": true,
	"delete": true, "migrate-rollouts": true, "migrate-nfs-db": true, "unarchive": true,
	"cloud": true, "cloud-tasks": true, "responses-api-proxy": true, "stdio-to-uds": true,
	"exec-server": true, "features": true, "help": true,
}

var commandFlags = map[string]bool{"--help": true, "-h": true, "--version": true, "-V": true}

// pickerFlags choose among sessions for resume and fork.
var pickerFlags = map[string]bool{"--last": true, "--all": true, "--include-non-interactive": true}

// firstRunOnly are options a resume must not repeat: the images belong to the
// first prompt, and --worktree would make the session a new worktree.
var firstRunOnly = map[string]bool{"--image": true, "-i": true, "--worktree": true}

// option is one option and the arguments holding it and its values.
type option struct {
	name   string
	values []string
	args   []string
}

// readOption reads the option at args[i]; next is the index after it.
func readOption(args []string, i int) (opt option, next int) {
	arg := args[i]
	next = i + 1
	switch {
	case strings.HasPrefix(arg, "--"):
		if name, value, joined := strings.Cut(arg, "="); joined {
			return option{name: name, values: []string{value}, args: args[i:next]}, next
		}
		opt.name = arg
	case len(arg) > 2:
		// clap reads -mo3 and -m=o3 as -m with the value o3.
		return option{name: arg[:2], values: []string{strings.TrimPrefix(arg[2:], "=")}, args: args[i:next]}, next
	default:
		opt.name = arg
	}
	if valueLong[opt.name] || valueShort[opt.name] {
		for next < len(args) && !strings.HasPrefix(args[next], "-") {
			opt.values = append(opt.values, args[next])
			next++
			if opt.name != "--image" && opt.name != "-i" {
				break
			}
		}
	}
	opt.args = args[i:next]
	return opt, next
}

// overridesHooks reports whether opt replaces or disables the hooks the box adds.
func (o option) overridesHooks() bool {
	for _, v := range o.values {
		switch o.name {
		case "-c", "--config":
			key, _, _ := strings.Cut(v, "=")
			key = strings.TrimSpace(key)
			if key == "hooks" || strings.HasPrefix(key, "hooks.") || key == "features.hooks" {
				return true
			}
		case "--disable":
			if v == "hooks" {
				return true
			}
		}
	}
	return false
}

type scan struct {
	command bool
	// sub is "resume" or "fork" when the run picks an existing session.
	sub         string
	positionals []string
	last        bool
	hooks       []string
}

func scanArgs(args []string) scan {
	var s scan
	sawPositional := false
	for i := 0; i < len(args); {
		arg := args[i]
		switch {
		case arg == "--":
			return s
		case commandFlags[arg]:
			s.command = true
			i++
		case strings.HasPrefix(arg, "-") && arg != "-":
			opt, next := readOption(args, i)
			if opt.overridesHooks() {
				s.hooks = opt.args
			}
			if opt.name == "--last" {
				s.last = true
			}
			i = next
		default:
			switch {
			case s.sub != "":
				s.positionals = append(s.positionals, arg)
			case !sawPositional && (arg == "resume" || arg == "fork"):
				s.sub = arg
			case !sawPositional && subcommands[arg]:
				s.command = true
			}
			sawPositional = true
			i++
		}
	}
	return s
}

// resumed is the session id a resume names, or "" for the picker, --last or a session name.
func (s scan) resumed() string {
	if s.sub != "resume" || s.last || len(s.positionals) == 0 || !claudeargs.IsUUID(s.positionals[0]) {
		return ""
	}
	return s.positionals[0]
}

func hooksError(args []string) error {
	return fmt.Errorf("%s would replace the hooks remote-agents gives Codex, which follow its session", strings.Join(args, " "))
}

// Analyze plans an invocation. Codex names new sessions itself, so a plan
// carries a session id only when it resumes one by id.
func Analyze(args []string) (agentcli.Plan, error) {
	s := scanArgs(args)
	if s.command {
		return agentcli.Plan{Mode: agentcli.ModeCommand, Args: args}, nil
	}
	if s.hooks != nil {
		return agentcli.Plan{}, hooksError(s.hooks)
	}
	id := s.resumed()
	return agentcli.Plan{Mode: agentcli.ModeSession, Args: args, SessionID: id, Resumes: id != ""}, nil
}

// SessionOf is the session id the arguments of a codex command line resume, or "".
func SessionOf(args []string) string { return scanArgs(args).resumed() }

// ResumeBase is args without the session they pick, their prompt and the
// images attached to it: the options a later resume of the same session
// runs with. Codex takes them before its resume subcommand.
func ResumeBase(args []string) []string {
	out := []string{}
	for i := 0; i < len(args); {
		arg := args[i]
		if arg == "--" {
			return out
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			i++
			continue
		}
		opt, next := readOption(args, i)
		if !pickerFlags[opt.name] && !firstRunOnly[opt.name] {
			out = append(out, opt.args...)
		}
		i = next
	}
	return out
}

// ResumeArgs resume session id.
func ResumeArgs(id string) []string { return []string{"resume", id} }

// CheckDefaults accepts arguments that are prepended to every invocation,
// such as --yolo. They may only be options, may not make every run one-shot
// or replace the box's hooks, and an option's value must be joined with "="
// in its long form: a separate value would read the user's own first
// argument, as --image does.
func CheckDefaults(args []string) error {
	for i, arg := range args {
		switch {
		case arg == "":
			return errors.New("an argument is empty")
		case arg == "--" || !strings.HasPrefix(arg, "-"):
			return fmt.Errorf("%q is not an option", arg)
		case commandFlags[arg]:
			return fmt.Errorf("%s makes every run a one-shot command", arg)
		}
		opt, _ := readOption(args[i:i+1], 0)
		if valueShort[opt.name] {
			return fmt.Errorf("%s takes a value; use its long form as --option=<value>", opt.name)
		}
		if valueLong[arg] {
			return fmt.Errorf("write %s=<value>", arg)
		}
		if opt.overridesHooks() {
			return hooksError(opt.args)
		}
		if pickerFlags[arg] {
			return fmt.Errorf("%s picks the session; pass it on the command line", arg)
		}
	}
	return nil
}

// CLI is the Codex command line as agentcli sees it.
type CLI struct{}

func (CLI) Analyze(args []string, _ func() string) (agentcli.Plan, error) { return Analyze(args) }
func (CLI) CheckDefaults(args []string) error                             { return CheckDefaults(args) }
func (CLI) ResumeBase(args []string) []string                             { return ResumeBase(args) }
func (CLI) ResumeArgs(id string) []string                                 { return ResumeArgs(id) }
