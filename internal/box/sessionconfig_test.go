package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionConfigDefaultsWithoutAFile(t *testing.T) {
	cfg, err := LoadSessionConfig(filepath.Join(t.TempDir(), "session.toml"))
	if err != nil || cfg != DefaultSessionConfig || cfg.MemoryMax != 64<<30 {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
}

func TestSessionConfigMemoryMax(t *testing.T) {
	for value, want := range map[string]uint64{"512M": 512 << 20, "2G": 2 << 30, "1T": 1 << 40, "4096K": 4 << 20, "1073741824": 1 << 30} {
		cfg, err := ParseSessionConfig([]byte(`memory_max = "` + value + `"`))
		if err != nil || cfg.MemoryMax != want {
			t.Fatalf("memory_max %q = %d, %v; want %d", value, cfg.MemoryMax, err, want)
		}
	}
	if cfg, err := ParseSessionConfig(nil); err != nil || cfg != DefaultSessionConfig {
		t.Fatalf("empty file: %+v, %v", cfg, err)
	}
}

func TestSessionConfigErrors(t *testing.T) {
	for src, want := range map[string]string{
		`memory_max = "0G"`:        "memory_max",
		`memory_max = "1.5G"`:      "memory_max",
		`memory_max = "64GB"`:      "memory_max",
		`memory_max = "99999999T"`: "memory_max",
		`memory_max = 64`:          "MemoryMax",
		`bogus = 1`:                "bogus",
	} {
		if _, err := ParseSessionConfig([]byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v does not mention %q", src, err, want)
		}
	}
	path := filepath.Join(t.TempDir(), "session.toml")
	if err := os.WriteFile(path, []byte(`memory_max = "x"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionConfig(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("load error %v does not name the file", err)
	}
}
