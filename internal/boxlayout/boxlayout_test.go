package boxlayout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayoutPathsAreUnderTheInstallName(t *testing.T) {
	l := Layout{Home: "/home/agent", Name: "remote-agents-e2e"}
	cases := map[string]string{
		l.Binary():           "/home/agent/.local/share/remote-agents-e2e/bin/remote-agents-box",
		l.Shims():            "/home/agent/.local/share/remote-agents-e2e/shims",
		l.HookBin():          "/home/agent/.local/share/remote-agents-e2e/hookbin",
		l.Shpool():           "/home/agent/.local/share/remote-agents-e2e/bin/shpool",
		l.ShpoolConfig():     "/home/agent/.config/remote-agents-e2e/shpool.toml",
		l.ShpoolSocket():     "/home/agent/.local/state/remote-agents-e2e/shpool.sock",
		l.LaptopSocket():     "/home/agent/.local/state/remote-agents-e2e/laptop.sock",
		l.EndedMarker("r"):   "/home/agent/.local/state/remote-agents-e2e/ended/r.json",
		l.ClaudeBin():        "/home/agent/.local/bin",
		l.HookState("s"):     "/home/agent/.local/state/remote-agents-e2e/activity/s.hooks.json",
		l.Status("s"):        "/home/agent/.local/state/remote-agents-e2e/activity/s.status.json",
		l.Reaped("c"):        "/home/agent/.local/state/remote-agents-e2e/reaped/c.json",
		l.ReapedSession("s"): "/home/agent/.local/state/remote-agents-e2e/reaped-sessions/s.json",
		l.ReaperLog():        "/home/agent/.local/state/remote-agents-e2e/reaper.log",
		l.ReaperConfig():     "/home/agent/.config/remote-agents-e2e/reaper.toml",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
}

func TestBinaryRelMatchesLayout(t *testing.T) {
	l := Layout{Home: "/h", Name: DefaultName}
	if got := "/h/" + BinaryRel(DefaultName); got != l.Binary() {
		t.Fatalf("BinaryRel = %q, layout binary = %q", got, l.Binary())
	}
}

// install creates a box binary for name under home and returns the path the kernel reports for it.
func install(t *testing.T, home, name string) string {
	t.Helper()
	bin := filepath.Join(home, BinaryRel(name))
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	exe, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestFromExecutableTakesTheNameFromTheInstallDir(t *testing.T) {
	home := t.TempDir()
	l, err := FromExecutable(home, install(t, home, "remote-agents-e2e"))
	if err != nil {
		t.Fatal(err)
	}
	if l != (Layout{Home: home, Name: "remote-agents-e2e"}) {
		t.Fatalf("layout = %+v", l)
	}
}

func TestFromExecutableFollowsASymlinkedLocalDir(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	disk := filepath.Join(root, "disk", "local")
	if err := os.MkdirAll(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(disk, filepath.Join(home, ".local")); err != nil {
		t.Fatal(err)
	}
	l, err := FromExecutable(home, install(t, home, DefaultName))
	if err != nil {
		t.Fatal(err)
	}
	if l != (Layout{Home: home, Name: DefaultName}) {
		t.Fatalf("layout = %+v", l)
	}
}

func TestFromExecutableRefusesABinaryOutsideAnInstall(t *testing.T) {
	home := t.TempDir()
	other := t.TempDir()
	cases := map[string]struct{ exe, want string }{
		"not the box binary":  {filepath.Join(home, ".local/share/remote-agents/bin/other"), "not the box binary"},
		"another home":        {install(t, other, DefaultName), "not installed under"},
		"invalid name":        {filepath.Join(home, ".local/share/Bad_Name/bin/remote-agents-box"), "install name"},
		"missing install dir": {filepath.Join(home, ".local/share/absent/bin/remote-agents-box"), "not installed under"},
	}
	for name, tc := range cases {
		l, err := FromExecutable(home, tc.exe)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: layout %+v, err %v, want %q", name, l, err, tc.want)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"remote-agents", "remote-agents-e2e", "ra2"} {
		if err := ValidName(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"", "-x", "a/b", "..", "A", "a b", `a"b`, "a$b"} {
		if err := ValidName(name); err == nil || !strings.Contains(err.Error(), "install name") {
			t.Errorf("%q: err = %v", name, err)
		}
	}
}
