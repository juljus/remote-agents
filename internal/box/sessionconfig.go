package box

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"regexp"
	"strconv"

	"github.com/pelletier/go-toml/v2"
)

// SessionConfig is how the box runs each session.
type SessionConfig struct {
	// MemoryMax caps the memory of each session's processes, in bytes.
	MemoryMax uint64
}

// DefaultSessionConfig applies unless the install's session.toml overrides it.
var DefaultSessionConfig = SessionConfig{MemoryMax: 64 << 30}

type rawSessionConfig struct {
	MemoryMax *string `toml:"memory_max"`
}

// LoadSessionConfig reads path; an absent file means DefaultSessionConfig.
func LoadSessionConfig(path string) (SessionConfig, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultSessionConfig, nil
	}
	if err != nil {
		return SessionConfig{}, fmt.Errorf("reading session config: %w", err)
	}
	cfg, err := ParseSessionConfig(data)
	if err != nil {
		return SessionConfig{}, fmt.Errorf("session config %s: %w", path, err)
	}
	return cfg, nil
}

func ParseSessionConfig(data []byte) (SessionConfig, error) {
	var raw rawSessionConfig
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return SessionConfig{}, fmt.Errorf("unknown keys:\n%s", strict.String())
		}
		return SessionConfig{}, fmt.Errorf("parsing toml: %w", err)
	}
	cfg := DefaultSessionConfig
	if raw.MemoryMax != nil {
		var err error
		if cfg.MemoryMax, err = parseSize(*raw.MemoryMax); err != nil {
			return SessionConfig{}, fmt.Errorf("memory_max: %w", err)
		}
	}
	return cfg, nil
}

var (
	sizePattern = regexp.MustCompile(`^([0-9]+)([KMGT]?)$`)
	sizeShifts  = map[string]uint{"": 0, "K": 10, "M": 20, "G": 30, "T": 40}
)

// parseSize reads a positive byte count with an optional K, M, G or T suffix
// in powers of 1024, as systemd reads MemoryMax.
func parseSize(s string) (uint64, error) {
	m := sizePattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not a byte count such as 64G", s)
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("%q must be positive", s)
	}
	shift := sizeShifts[m[2]]
	if n > math.MaxUint64>>shift {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return n << shift, nil
}
