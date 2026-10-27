// Package config loads the laptop-side remote-agents configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/pelletier/go-toml/v2"

	"remote-agents/internal/boxlayout"
	"remote-agents/internal/claudeargs"
)

// Kind selects how the wrapper understands an agent's command line.
type Kind string

const KindClaude Kind = "claude"

type Agent struct {
	// Command is the agent executable on the box, resolved against the session PATH.
	Command string
	Kind    Kind
	// Args go before the arguments of every invocation.
	Args []string
}

type Link struct {
	// Opener is the laptop executable that opens a URL given as its only argument.
	Opener string
}

// DefaultOpener opens URLs in the default browser.
const DefaultOpener = "/usr/bin/open"

type Cmux struct {
	// Wrapper is cmux's claude wrapper script; it decides which hooks to inject.
	Wrapper string
}

type Config struct {
	Host string
	// BoxInstall names the remote-agents install on the box; see boxlayout.DefaultName.
	BoxInstall string
	Workdir    string
	Link       Link
	// Cmux is nil when cmux integration is not configured.
	Cmux   *Cmux
	Agents map[string]Agent
}

type rawAgent struct {
	Command *string   `toml:"command"`
	Kind    *string   `toml:"kind"`
	Args    *[]string `toml:"args"`
}

type rawLink struct {
	Opener *string `toml:"opener"`
}

type rawCmux struct {
	Wrapper *string `toml:"wrapper"`
}

type rawConfig struct {
	Host       *string             `toml:"host"`
	BoxInstall *string             `toml:"box_install"`
	Workdir    *string             `toml:"workdir"`
	Link       *rawLink            `toml:"link"`
	Cmux       *rawCmux            `toml:"cmux"`
	Agents     map[string]rawAgent `toml:"agents"`
}

// DefaultPath is ~/.config/remote-agents/config.toml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".config", "remote-agents", "config.toml"), nil
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

func Parse(data []byte) (Config, error) {
	var raw rawConfig
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return Config{}, fmt.Errorf("unknown keys:\n%s", strict.String())
		}
		return Config{}, fmt.Errorf("parsing toml: %w", err)
	}
	return raw.validate()
}

func (r rawConfig) validate() (Config, error) {
	if r.Host == nil || *r.Host == "" {
		return Config{}, errors.New("missing required key: host")
	}
	if r.Workdir == nil || *r.Workdir == "" {
		return Config{}, errors.New("missing required key: workdir")
	}
	if !filepath.IsAbs(*r.Workdir) {
		return Config{}, fmt.Errorf("workdir must be an absolute path on the box, got %q", *r.Workdir)
	}
	install := boxlayout.DefaultName
	if r.BoxInstall != nil {
		install = *r.BoxInstall
	}
	if err := boxlayout.ValidName(install); err != nil {
		return Config{}, fmt.Errorf("box_install: %w", err)
	}
	link, err := r.validateLink()
	if err != nil {
		return Config{}, err
	}
	cmux, err := r.validateCmux()
	if err != nil {
		return Config{}, err
	}
	agents, err := validateAgents(r.Agents)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Host:       *r.Host,
		BoxInstall: install,
		Workdir:    *r.Workdir,
		Link:       link,
		Cmux:       cmux,
		Agents:     agents,
	}, nil
}

func (r rawConfig) validateLink() (Link, error) {
	if r.Link == nil || r.Link.Opener == nil {
		return Link{Opener: DefaultOpener}, nil
	}
	if !filepath.IsAbs(*r.Link.Opener) {
		return Link{}, fmt.Errorf("link.opener must be an absolute path, got %q", *r.Link.Opener)
	}
	return Link{Opener: *r.Link.Opener}, nil
}

func (r rawConfig) validateCmux() (*Cmux, error) {
	if r.Cmux == nil {
		return nil, nil
	}
	if r.Cmux.Wrapper == nil || *r.Cmux.Wrapper == "" {
		return nil, errors.New("missing required key: cmux.wrapper")
	}
	return &Cmux{Wrapper: *r.Cmux.Wrapper}, nil
}

func validateAgents(raw map[string]rawAgent) (map[string]Agent, error) {
	if len(raw) == 0 {
		return nil, errors.New("no [agents.<name>] tables configured")
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	agents := make(map[string]Agent, len(raw))
	for _, name := range names {
		a := raw[name]
		if a.Command == nil || *a.Command == "" {
			return nil, fmt.Errorf("missing required key: agents.%s.command", name)
		}
		if a.Kind == nil || *a.Kind == "" {
			return nil, fmt.Errorf("missing required key: agents.%s.kind", name)
		}
		kind := Kind(*a.Kind)
		if kind != KindClaude {
			return nil, fmt.Errorf("agents.%s.kind: unsupported kind %q (supported: %q)", name, kind, KindClaude)
		}
		var args []string
		if a.Args != nil {
			if err := claudeargs.CheckDefaults(*a.Args); err != nil {
				return nil, fmt.Errorf("agents.%s.args: %w", name, err)
			}
			args = *a.Args
		}
		agents[name] = Agent{Command: *a.Command, Kind: kind, Args: args}
	}
	return agents, nil
}

func (c Config) Agent(name string) (Agent, error) {
	a, ok := c.Agents[name]
	if !ok {
		return Agent{}, fmt.Errorf("agent %q is not configured", name)
	}
	return a, nil
}
