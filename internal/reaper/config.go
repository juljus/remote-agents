// Package reaper decides when a detached agent session on the box has gone
// idle, or has been detached too long, and should be ended.
package reaper

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	// IdleAfter ends a detached session once nothing has happened in it this long.
	IdleAfter time.Duration
	// WorkingCap ends a detached session this long after it was detached, working or not.
	WorkingCap time.Duration
	// Poll is how often the session is checked.
	Poll time.Duration
}

// Default is the policy unless the install's reaper.toml overrides it.
var Default = Config{IdleAfter: 5 * time.Minute, WorkingCap: 36 * time.Hour, Poll: 15 * time.Second}

type rawConfig struct {
	IdleAfter  *string `toml:"idle_after"`
	WorkingCap *string `toml:"working_cap"`
	Poll       *string `toml:"poll"`
}

// LoadConfig reads path; an absent file means Default.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("reading reaper config: %w", err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return Config{}, fmt.Errorf("reaper config %s: %w", path, err)
	}
	return cfg, nil
}

func ParseConfig(data []byte) (Config, error) {
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
	cfg := Default
	for _, f := range []struct {
		key string
		raw *string
		dst *time.Duration
	}{
		{"idle_after", raw.IdleAfter, &cfg.IdleAfter},
		{"working_cap", raw.WorkingCap, &cfg.WorkingCap},
		{"poll", raw.Poll, &cfg.Poll},
	} {
		if f.raw == nil {
			continue
		}
		d, err := time.ParseDuration(*f.raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", f.key, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("%s must be positive, got %s", f.key, d)
		}
		*f.dst = d
	}
	if cfg.Poll > cfg.IdleAfter {
		return Config{}, fmt.Errorf("poll %s is longer than idle_after %s", cfg.Poll, cfg.IdleAfter)
	}
	return cfg, nil
}
