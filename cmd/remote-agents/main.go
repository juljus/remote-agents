//go:build darwin

// Command remote-agents runs coding agents on a remote box while the laptop
// terminal only displays them. Invoked as r<agent> (a symlink, such as
// rclaude) it is equivalent to `remote-agents run <agent>`.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/term"

	"remote-agents/internal/accounts"
	"remote-agents/internal/accountui"
	"remote-agents/internal/agentcli"
	"remote-agents/internal/cmuxcapture"
	"remote-agents/internal/cmuxcodex"
	"remote-agents/internal/config"
	"remote-agents/internal/control"
	"remote-agents/internal/entrypoint"
	"remote-agents/internal/hookexec"
	"remote-agents/internal/laptopfiles"
	"remote-agents/internal/laptoppaths"
	"remote-agents/internal/link"
	"remote-agents/internal/linkproto"
	"remote-agents/internal/resume"
	"remote-agents/internal/session"
	"remote-agents/internal/spec"
	"remote-agents/internal/sshrun"
	"remote-agents/internal/triggers"
)

const usage = `usage:
  remote-agents run <agent> [--] [agent args...]   start or resume an agent session on the box
  remote-agents attach <session>                   reattach to a running session
  remote-agents ls                                 list sessions running on the box
  remote-agents link                               run the link daemon (launchd runs this)
  r<agent> [agent args...]                         same as run <agent>, e.g. rclaude
  r<agent> account                                 pick, add, rename and remove the box's accounts
  r<agent> account [list [--json] | use <name> | add <name> | save <name> |
                    rename <name> <new name> | remove <name> | start <name>]`

// configEnvVar points at a config file other than ~/.config/remote-agents/config.toml.
const configEnvVar = "REMOTE_AGENTS_CONFIG"

// Terminal properties the agent needs to render and detect the terminal like a local run.
var terminalEnvKeys = []string{"TERM", "COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "LANG"}

func main() {
	if cmuxcapture.Requested(os.Getenv) {
		if err := cmuxcapture.Report(os.Stdout, os.Args[1:], os.Environ()); err != nil {
			fmt.Fprintf(os.Stderr, "remote-agents: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	code, err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "remote-agents: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run() (int, error) {
	if agent, ok := entrypoint.Agent(os.Args[0]); ok {
		return runAgent(agent, os.Args[1:])
	}
	if len(os.Args) < 2 {
		return 2, errors.New(usage)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "run":
		if len(args) == 0 {
			return 2, errors.New(usage)
		}
		rest := args[1:]
		if len(rest) > 0 && rest[0] == "--" {
			rest = rest[1:]
		}
		return runAgent(args[0], rest)
	case "attach":
		if len(args) != 1 {
			return 2, errors.New(usage)
		}
		return attach(args[0])
	case "ls":
		if len(args) != 0 {
			return 2, errors.New(usage)
		}
		return list()
	case "link":
		if len(args) != 0 {
			return 2, errors.New(usage)
		}
		return 0, runLink()
	}
	return 2, errors.New(usage)
}

type env struct {
	cfg   config.Config
	ssh   string
	paths laptoppaths.Paths
}

func (e env) box() sshrun.Box { return sshrun.Box{Host: e.cfg.Host, Install: e.cfg.BoxInstall} }

func load() (env, error) {
	path := os.Getenv(configEnvVar)
	if path == "" {
		var err error
		if path, err = config.DefaultPath(); err != nil {
			return env{}, err
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return env{}, err
	}
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return env{}, fmt.Errorf("finding ssh: %w", err)
	}
	paths, err := laptoppaths.Default()
	if err != nil {
		return env{}, err
	}
	return env{cfg: cfg, ssh: ssh, paths: paths}, nil
}

func terminalEnv() map[string]string {
	out := map[string]string{}
	for _, k := range terminalEnvKeys {
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

func runAgent(agentName string, args []string) (int, error) {
	e, err := load()
	if err != nil {
		return 1, err
	}
	agent, err := e.cfg.Agent(agentName)
	if err != nil {
		return 1, err
	}
	if len(args) > 0 && args[0] == "account" {
		return account(e, agent, args[1:])
	}
	cli := agent.CLI()
	plan, err := cli.Analyze(append(slices.Clone(agent.Args), args...), uuid.NewString)
	if err != nil {
		return 1, err
	}
	if plan.Mode == agentcli.ModeCommand {
		return runCommand(e, agent, plan.Args)
	}

	resumeCommand := resumeCommandOf(agentName, cli)
	sessionID := plan.SessionID
	hint := resumeCommand(sessionID)
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	name := spec.SessionName(agentName, sessionID)
	if plan.SessionID == "" {
		hint = "remote-agents attach " + name
	}
	agentArgs, hooks, err := cmuxHooks(e, agentName, agent.Kind, plan.Args, args)
	if err != nil {
		return 1, err
	}
	followed := &spec.Agent{Resume: append([]string{agent.Command}, cli.ResumeBase(agentArgs)...)}
	if plan.Resumes {
		followed.Resumes = plan.SessionID
	}
	s := spec.Spec{
		Mode:      spec.ModeSession,
		Session:   name,
		RunID:     uuid.NewString(),
		Workdir:   e.cfg.Workdir,
		Argv:      append([]string{agent.Command}, agentArgs...),
		Env:       terminalEnv(),
		HookRelay: hooks != nil,
	}
	if err := s.Follow(agent.Kind, followed); err != nil {
		return 1, err
	}
	return runSession(e, s, hint, hooks, resume.NewTracker(plan.SessionID, plan.Resumes, resumeCommand))
}

// resumeCommandOf gives the laptop command line that resumes a session of the agent.
func resumeCommandOf(agentName string, cli agentcli.CLI) func(id string) string {
	return func(id string) string {
		return strings.Join(append([]string{entrypoint.Name(agentName)}, cli.ResumeArgs(id)...), " ")
	}
}

// cmuxHooks returns the agent arguments with cmux's hooks added and, when
// running inside cmux, the executor for the hooks the box relays back. cmux's
// claude wrapper picks Claude's hooks; Codex's are the ones cmux installs
// for it, which the box adds itself. userArgs are the wrapper's own, which
// cmux restores a Codex tab with.
func cmuxHooks(e env, agentName string, kind config.Kind, args, userArgs []string) ([]string, *hookexec.Executor, error) {
	if e.cfg.Cmux == nil {
		return args, nil, nil
	}
	entry, err := agentEntry(agentName)
	if err != nil {
		return nil, nil, err
	}
	if kind == config.KindCodex {
		dir, err := os.Getwd()
		if err != nil {
			return nil, nil, fmt.Errorf("reading the working directory: %w", err)
		}
		hooks, err := cmuxcodex.NewExecutor(os.Environ(), os.Getpid(), cmuxcodex.Launch{Executable: entry, Args: userArgs, Dir: dir})
		return args, hooks, err
	}
	captured, launched := cmuxcapture.LaunchedByCmux(entry, args, os.Environ(), os.Getpid())
	if !launched {
		if captured, err = cmuxcapture.Capture(context.Background(), e.cfg.Cmux.Wrapper, entry, args, os.Environ()); err != nil {
			return nil, nil, err
		}
	}
	if !captured.InCmux() {
		return captured.Args, nil, nil
	}
	allowed, err := captured.HookArgvs()
	if err != nil {
		return nil, nil, err
	}
	captured.Env["CMUX_CLAUDE_PID"] = strconv.Itoa(os.Getpid())
	return captured.Args, &hookexec.Executor{BaseEnv: os.Environ(), Env: captured.HookEnv, Allowed: allowed}, nil
}

// agentEntry is the r<agent> symlink next to this binary. cmux records the
// launching executable to resume sessions later, so it must be a command that
// takes the agent's own arguments.
func agentEntry(agentName string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating remote-agents: %w", err)
	}
	entry := filepath.Join(filepath.Dir(exe), entrypoint.Name(agentName))
	if _, err := os.Stat(entry); err != nil {
		return "", fmt.Errorf("%s missing (run `make install`): %w", entry, err)
	}
	return entry, nil
}

func requireDaemon(e env) (control.Client, error) {
	c := control.Client{Socket: e.paths.ControlSocket()}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.Status(ctx); err != nil {
		return control.Client{}, fmt.Errorf("%w; load it with: launchctl bootstrap gui/%d ~/Library/LaunchAgents/com.remote-agents.link.plist", err, os.Getuid())
	}
	return c, nil
}

// runSession prints how to resume the session on the box after the agent
// exits; tracker follows its id through the hooks the box relays.
func runSession(e env, s spec.Spec, hint string, hooks *hookexec.Executor, tracker *resume.Tracker) (int, error) {
	ctl, err := requireDaemon(e)
	if err != nil {
		return 1, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger, closeLog, err := sessionLogger(e.paths)
	if err != nil {
		return 1, err
	}
	defer closeLog()
	trig := make(chan string, 1)
	watchErr := make(chan error, 1)
	go func() {
		err := triggers.WatchSystem(ctx, trig)
		// The session goes on without reconnect triggers; the error reaches the screen only at exit.
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("%s: network watcher stopped, wake and network changes no longer reconnect: %v", s.Session, err)
		}
		watchErr <- err
	}()
	opts := session.Options{
		SSH: e.ssh, Box: e.box(), Spec: s, ReattachHint: hint, Triggers: trig,
		ExitHint: tracker.Hint,
		Stdin:    os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Logf: logger.Printf,
		Registration: &session.Registration{Control: ctl},
		Pasted:       laptopfiles.Records{Dir: e.paths.PastedRecords()},
	}
	if hooks != nil {
		run := func(ctx context.Context, req linkproto.HookRequest) (linkproto.HookResult, error) {
			res, err := hooks.Run(ctx, req)
			// Only hooks cmux injected run, so only their payloads name the session.
			if err == nil {
				if oerr := tracker.ObserveHook(req.Stdin); oerr != nil {
					logger.Printf("%s: %v", s.Session, oerr)
				}
			}
			return res, err
		}
		opts.Registration.RunHook = run
	}
	code, err := session.Run(ctx, opts)
	cancel()
	if werr := <-watchErr; werr != nil && !errors.Is(werr, context.Canceled) {
		return code, errors.Join(err, werr)
	}
	return code, err
}

func sessionLogger(paths laptoppaths.Paths) (*log.Logger, func(), error) {
	if err := os.MkdirAll(paths.LogDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("creating log dir: %w", err)
	}
	f, err := os.OpenFile(paths.SessionLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("opening session log: %w", err)
	}
	return log.New(f, fmt.Sprintf("pid %d ", os.Getpid()), log.LstdFlags), func() { f.Close() }, nil
}

func attach(name string) (int, error) {
	if err := spec.ValidSessionName(name); err != nil {
		return 1, err
	}
	e, err := load()
	if err != nil {
		return 1, err
	}
	agentName, err := spec.AgentOf(name)
	if err != nil {
		return 1, err
	}
	agent, err := e.cfg.Agent(agentName)
	if err != nil {
		return 1, err
	}
	_, hooks, err := cmuxHooks(e, agentName, agent.Kind, nil, nil)
	if err != nil {
		return 1, err
	}
	s := spec.Spec{Mode: spec.ModeReattach, Session: name, RunID: uuid.NewString()}
	return runSession(e, s, "remote-agents attach "+name, hooks, resume.NewTracker("", false, resumeCommandOf(agentName, agent.CLI())))
}

// runCommand runs a one-shot agent invocation without shpool or reconnects.
func runCommand(e env, agent config.Agent, args []string) (int, error) {
	s := spec.Spec{Mode: spec.ModeExec, Workdir: e.cfg.Workdir, Argv: append([]string{agent.Command}, args...), Env: terminalEnv()}
	token, err := s.Encode()
	if err != nil {
		return 1, err
	}
	tty := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	return passthrough(sshrun.ExecArgv(e.ssh, e.box(), token, tty))
}

// accountUsage is the account command's usage; %s is the agent's kind.
const accountUsage = "usage: r%s account [list [--json] | use <name> | add <name> | save <name> | rename <name> <new name> | remove <name> | start <name>]"

// account runs the box's account commands; without arguments it opens the
// account picker.
func account(e env, agent config.Agent, args []string) (int, error) {
	box := boxAccounts{e: e, agent: agent}
	cli := "r" + box.kind()
	if len(args) == 0 {
		return pickAccount(box)
	}
	switch {
	case len(args) == 1 && args[0] == "list":
		listing, err := box.List()
		if err != nil {
			return 1, err
		}
		return 0, accounts.Render(os.Stdout, listing, time.Now(), term.IsTerminal(int(os.Stdout.Fd())), cli)
	case len(args) == 2 && args[0] == "list" && args[1] == "--json":
		return passthrough(sshrun.AccountArgv(e.ssh, e.box(), false, box.kind(), "list"))
	case len(args) == 2 && (args[0] == "use" || args[0] == "save" || args[0] == "remove"),
		len(args) == 3 && args[0] == "rename":
		if err := validNames(args[1:]); err != nil {
			return 2, err
		}
		return passthrough(sshrun.AccountArgv(e.ssh, e.box(), false, box.kind(), args...))
	case len(args) == 2 && args[0] == "add":
		if err := validNames(args[1:]); err != nil {
			return 2, err
		}
		argv, err := box.addArgv(args[1])
		if err != nil {
			return 1, err
		}
		return passthrough(argv)
	case len(args) == 2 && args[0] == "start":
		if err := validNames(args[1:]); err != nil {
			return 2, err
		}
		argv, err := box.startArgv(args[1])
		if err != nil {
			return 2, err
		}
		return passthrough(argv)
	}
	return 2, fmt.Errorf(accountUsage, box.kind())
}

func validNames(names []string) error {
	for _, name := range names {
		if err := accounts.ValidName(name); err != nil {
			return err
		}
	}
	return nil
}

// pickAccount opens the picker on a terminal; elsewhere it lists the
// accounts and says how to switch.
func pickAccount(box boxAccounts) (int, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		listing, err := box.List()
		if err != nil {
			return 1, err
		}
		cli := "r" + box.kind()
		if err := accounts.Render(os.Stdout, listing, time.Now(), term.IsTerminal(int(os.Stdout.Fd())), cli); err != nil {
			return 1, err
		}
		fmt.Printf("not a terminal, so no picker; switch with: %s account use <name>\n", cli)
		return 0, nil
	}
	said, err := accountui.Run(os.Stdin, os.Stdout, box)
	if err != nil {
		return 1, err
	}
	fmt.Print(said)
	return 0, nil
}

// boxAccounts runs the box's account commands for one agent.
type boxAccounts struct {
	e     env
	agent config.Agent
}

func (b boxAccounts) kind() string { return string(b.agent.Kind) }

func (b boxAccounts) List() (accounts.Listing, error) {
	out, err := b.Do("list")
	if err != nil {
		return accounts.Listing{}, err
	}
	var l accounts.Listing
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return accounts.Listing{}, fmt.Errorf("reading the box's account listing: %w", err)
	}
	return l, nil
}

func (b boxAccounts) Do(args ...string) (string, error) {
	return b.query(sshrun.AccountArgv(b.e.ssh, b.e.box(), false, b.kind(), args...))
}

func (b boxAccounts) Add(name string) error {
	argv, err := b.addArgv(name)
	if err != nil {
		return err
	}
	code, err := passthrough(argv)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("adding %s failed (exit %d)", name, code)
	}
	return nil
}

func (b boxAccounts) Start(name string) error {
	argv, err := b.startArgv(name)
	if err != nil {
		return err
	}
	_, err = b.query(argv)
	return err
}

// addArgv signs a new account in on the box, in a terminal.
func (b boxAccounts) addArgv(name string) ([]string, error) {
	token, err := b.execToken(b.agent.CLI().SignInArgs(), terminalEnv())
	if err != nil {
		return nil, err
	}
	return sshrun.AccountArgv(b.e.ssh, b.e.box(), true, b.kind(), "add", name, token), nil
}

// startArgv sends the account the agent's one short request.
func (b boxAccounts) startArgv(name string) ([]string, error) {
	hello, ok := b.agent.CLI().HelloArgs()
	if !ok {
		return nil, fmt.Errorf("%s accounts have no weekly window a request starts", b.kind())
	}
	token, err := b.execToken(hello, map[string]string{})
	if err != nil {
		return nil, err
	}
	return sshrun.AccountArgv(b.e.ssh, b.e.box(), false, b.kind(), "start", name, token), nil
}

func (b boxAccounts) execToken(args []string, env map[string]string) (string, error) {
	s := spec.Spec{Mode: spec.ModeExec, Workdir: b.e.cfg.Workdir, Argv: append([]string{b.agent.Command}, args...), Env: env}
	return s.Encode()
}

// query runs an account command that needs no terminal, and returns what
// it printed; a failure is the box's own message.
func (b boxAccounts) query(argv []string) (string, error) {
	var out, errOut bytes.Buffer
	res := sshrun.Run(context.Background(), argv, sshrun.Stdio{Stdout: &out, Stderr: &errOut}, 2*time.Second)
	switch {
	case res.Err != nil:
		return "", res.Err
	case res.Transport:
		return "", fmt.Errorf("link to box failed: %s", res.Detail)
	case res.ExitCode != 0:
		said := strings.Fields(strings.TrimSpace(out.String() + "\n" + strings.ReplaceAll(errOut.String(), "remote-agents-box: ", "")))
		if len(said) == 0 {
			return "", fmt.Errorf("the box's account command failed (exit %d)", res.ExitCode)
		}
		return "", errors.New(strings.Join(said, " "))
	}
	return out.String(), nil
}

func list() (int, error) {
	e, err := load()
	if err != nil {
		return 1, err
	}
	return passthrough(sshrun.ListArgv(e.ssh, e.box()))
}

// passthrough runs ssh once; terminal signals reach ssh directly, so they are only absorbed here.
func passthrough(argv []string) (int, error) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	res := sshrun.Run(context.Background(), argv, sshrun.Stdio{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}, 2*time.Second)
	if res.Err != nil {
		return 1, res.Err
	}
	if res.Transport {
		return 255, fmt.Errorf("link to box failed: %s", res.Detail)
	}
	return res.ExitCode, nil
}

func resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func runLink() error {
	e, err := load()
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "", log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	trig := make(chan string, 1)
	watchErr := make(chan error, 1)
	go func() { watchErr <- triggers.WatchSystem(ctx, trig) }()
	reg := control.NewRegistry()
	forwarder := &link.SSHForwarder{SSH: e.ssh, Box: e.box(), Lifetime: link.ForwardLifetime, ProbeEvery: link.ForwardProbeEvery, Clock: link.SystemClock{}, Logf: logger.Printf}
	defer forwarder.Close()
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	trash := func(path string) error {
		trashed, err := laptopfiles.Trash(path)
		if err == nil {
			logger.Printf("moved %s to the Trash as %s", path, trashed)
		}
		return err
	}
	d := &link.Daemon{
		SSH:   e.ssh,
		Box:   e.box(),
		Paths: e.paths,
		Handler: &link.Handler{
			Pasteboard: link.MacPasteboard{},
			Opener:     link.CommandOpener{Path: e.cfg.Link.Opener},
			Forwarder:  forwarder,
			Registry:   reg,
			Resolve:    resolveHost,
			Files:      laptopfiles.Files{Records: laptopfiles.Records{Dir: e.paths.PastedRecords()}, Home: home, Trash: trash},
			Fetcher:    link.SSHFetcher{SSH: e.ssh, Box: e.box()},
			Revealer:   link.FinderRevealer{},
			Logf:       logger.Printf,
		},
		Registry: reg,
		Triggers: trig,
		Logf:     logger.Printf,
	}
	logger.Printf("link daemon starting for %s", e.cfg.Host)
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx) }()
	select {
	case err = <-runErr:
	case err = <-watchErr:
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
