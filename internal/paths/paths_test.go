package paths

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func linuxSys() sys {
	return sys{
		goos: "linux", getenv: env(nil), uid: "1000", home: "/home/u",
		userConfig: "/home/u/.config",
	}
}

func TestLayout(t *testing.T) {
	withRun := linuxSys()
	withRun.runUser = "/run/user/1000"
	mac := sys{
		goos: "darwin", getenv: env(nil), uid: "501", home: "/Users/u",
		userConfig: "/Users/u/Library/Application Support",
	}
	win := sys{
		goos: "windows", getenv: env(nil), uid: "S-1-5-21-1",
		home: `C:\Users\u`, userConfig: `C:\Users\u\AppData\Roaming`,
		localAppData: `C:\Users\u\AppData\Local`,
	}
	winDir := filepath.Join(win.localAppData, "kwak")
	cases := []struct {
		name                string
		s                   sys
		config, state, sock string
	}{
		{"linux no run dir", linuxSys(), "/home/u/.config/kwak", "/home/u/.local/state/kwak", "/home/u/.cache/kwak/kwak.sock"},
		{"linux run dir", withRun, "/home/u/.config/kwak", "/home/u/.local/state/kwak", "/run/user/1000/kwak/kwak.sock"},
		{"darwin", mac, "/Users/u/Library/Application Support/kwak", "/Users/u/Library/Application Support/kwak", "/Users/u/Library/Caches/kwak/kwak.sock"},
		{"windows", win, winDir, winDir, filepath.Join(winDir, "kwak.sock")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check(t, "config", c.config, c.s.configDir)
			check(t, "state", c.state, c.s.stateDir)
			check(t, "socket", c.sock, c.s.socketPath)
		})
	}
}

func check(t *testing.T, what, want string, f func() (string, error)) {
	t.Helper()
	got, err := f()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

func TestOverrides(t *testing.T) {
	s := linuxSys()
	s.getenv = env(map[string]string{
		EnvSocket: "/x/s.sock", EnvStateDir: "/x/state/", EnvConfigDir: "/x/config",
	})
	check(t, "config", "/x/config", s.configDir)
	check(t, "state", "/x/state", s.stateDir)
	check(t, "socket", "/x/s.sock", s.socketPath)

	s.getenv = env(map[string]string{EnvSocket: "rel/s.sock", EnvStateDir: "rel", EnvConfigDir: "rel"})
	for _, f := range []func() (string, error){s.configDir, s.stateDir, s.socketPath} {
		if _, err := f(); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("relative override: err = %v", err)
		}
	}

	mac := sys{goos: "darwin", userConfig: "/Users/u/Library/Application Support"}
	mac.getenv = env(map[string]string{EnvConfigDir: "/x/config"})
	check(t, "darwin config", "/x/config", mac.configDir)
	check(t, "darwin state ignores the config override", "/Users/u/Library/Application Support/kwak", mac.stateDir)
	mac.getenv = env(map[string]string{EnvStateDir: "/x/state"})
	check(t, "darwin config ignores the state override", "/Users/u/Library/Application Support/kwak", mac.configDir)
	check(t, "darwin state", "/x/state", mac.stateDir)

	long := "/" + strings.Repeat("a", 200)
	s.getenv = env(map[string]string{EnvSocket: long})
	check(t, "long override kept", long, s.socketPath)
}

func TestMissing(t *testing.T) {
	s := sys{goos: "linux", getenv: env(nil)}
	for _, f := range []func() (string, error){s.configDir, s.stateDir, s.socketPath} {
		if _, err := f(); err == nil {
			t.Fatal("no error without HOME")
		}
	}
	w := sys{goos: "windows", getenv: env(nil), userConfig: `C:\roaming`}
	for _, f := range []func() (string, error){w.configDir, w.stateDir, w.socketPath} {
		if _, err := f(); err == nil || !strings.Contains(err.Error(), "LOCALAPPDATA") {
			t.Fatalf("windows without LOCALAPPDATA: err = %v", err)
		}
	}
}

func TestFallback(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		max := MaxSocketPath(goos)
		s := sys{goos: goos, uid: "1000"}
		exact := "/" + strings.Repeat("a", max-1)
		if got, err := s.fit(exact); err != nil || got != exact {
			t.Fatalf("%s: fit(%d bytes) = %q, %v", goos, len(exact), got, err)
		}
		got, err := s.fit(exact + "a")
		if err != nil || got != "/tmp/kwak-1000/kwak.sock" {
			t.Fatalf("%s: fit(%d bytes) = %q, %v", goos, max+1, got, err)
		}
		s.uid = ""
		if _, err := s.fit(exact + "a"); err == nil {
			t.Fatalf("%s: fallback without a uid", goos)
		}
	}

	s := linuxSys()
	s.home = "/" + strings.Repeat("h", 100)
	check(t, "linux long home", "/tmp/kwak-1000/kwak.sock", s.socketPath)
	mac := sys{goos: "darwin", getenv: env(nil), uid: "501", home: "/" + strings.Repeat("h", 90)}
	check(t, "darwin long home", "/tmp/kwak-501/kwak.sock", mac.socketPath)

	win := sys{goos: "windows", getenv: env(nil), uid: "S-1-5-21-1", localAppData: `C:\` + strings.Repeat("l", 100)}
	if p, err := win.socketPath(); err == nil {
		t.Fatalf("windows long path = %q, want an error", p)
	}
}

// TestMaxMatchesNet measures the longest path the net package accepts here.
func TestMaxMatchesNet(t *testing.T) {
	dir, err := os.MkdirTemp("", "k")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	max := MaxSocketPath(currentGOOS())
	pad := max - len(dir) - 1
	if pad < 1 {
		t.Skip("temp dir too long")
	}
	ok := filepath.Join(dir, strings.Repeat("s", pad))
	ln, err := net.Listen("unix", ok)
	if err != nil {
		t.Fatalf("listen on %d bytes: %v", len(ok), err)
	}
	ln.Close()
	if _, err := net.Listen("unix", ok+"s"); err == nil {
		t.Fatalf("listen on %d bytes worked; MaxSocketPath is too small", len(ok)+1)
	}
}

func TestCurrent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "local"))
	t.Setenv(EnvSocket, "")
	t.Setenv(EnvStateDir, "")
	t.Setenv(EnvConfigDir, "")
	for _, f := range []func() (string, error){ConfigDir, StateDir, SocketPath} {
		p, err := f()
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(p) || !strings.Contains(p, "kwak") {
			t.Fatalf("path %q", p)
		}
	}

	sock, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "TMPDIR", "TMP", "TEMP", "APPDATA", "USERPROFILE"} {
		t.Setenv(name, filepath.Join(t.TempDir(), "decoy-"+name))
	}
	if again, err := SocketPath(); err != nil || again != sock {
		t.Fatalf("socket path moved with the environment: %q, then %q, %v", sock, again, err)
	}
	t.Setenv(EnvSocket, filepath.Join(home, "x.sock"))
	if p, _ := SocketPath(); p != filepath.Join(home, "x.sock") {
		t.Fatalf("override ignored: %q", p)
	}
}
