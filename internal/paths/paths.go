// Package paths gives the config dir, state dir and socket path per OS, following the table of
// spec 5.6. The socket path depends only on HOME, the uid and %LOCALAPPDATA% on Windows (and the
// KWAK_SOCKET override), because the Codex MCP server gets a reduced environment. The config and
// state dirs may also read XDG_CONFIG_HOME through os.UserConfigDir. Nothing here reads
// XDG_RUNTIME_DIR, XDG_CACHE_HOME, TMPDIR or D-Bus variables.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Test overrides. Production never sets them; kwak doctor warns when they are set.
const (
	EnvSocket    = "KWAK_SOCKET"
	EnvStateDir  = "KWAK_STATE_DIR"
	EnvConfigDir = "KWAK_CONFIG_DIR"
)

// SocketName is the file name of the socket inside its directory.
const SocketName = "kwak.sock"

// ConfigDir is the directory of config.json.
func ConfigDir() (string, error) { return current().configDir() }

// StateDir is the directory of state.json, listen.lock and resume logs.
func StateDir() (string, error) { return current().stateDir() }

// SocketPath is the listener's unix socket. On Linux and macOS a path too long for a unix socket
// falls back to /tmp/kwak-<uid>/kwak.sock, which ipc.Listen and ipc.Dial check because it is
// shared.
func SocketPath() (string, error) { return current().socketPath() }

// sys is what the rules read from the OS. current() fills it; tests build it by hand.
type sys struct {
	goos         string
	getenv       func(string) string
	uid          string // decimal uid, or the user SID on Windows
	home         string
	userConfig   string // os.UserConfigDir()
	localAppData string
	runUser      string // "/run/user/<uid>" when it exists, is a real dir and is ours; else ""
}

func (s sys) override(name string) (string, bool, error) {
	v := s.getenv(name)
	if v == "" {
		return "", false, nil
	}
	if !filepath.IsAbs(v) {
		return "", true, fmt.Errorf("paths: %s must be an absolute path, got %q", name, v)
	}
	return filepath.Clean(v), true, nil
}

func (s sys) configDir() (string, error) {
	if v, ok, err := s.override(EnvConfigDir); ok {
		return v, err
	}
	return s.defaultConfigDir()
}

func (s sys) defaultConfigDir() (string, error) {
	switch s.goos {
	case "windows":
		return s.local()
	default:
		if s.userConfig == "" {
			return "", errors.New("paths: no config dir; set HOME")
		}
		return filepath.Join(s.userConfig, "kwak"), nil
	}
}

func (s sys) stateDir() (string, error) {
	if v, ok, err := s.override(EnvStateDir); ok {
		return v, err
	}
	switch s.goos {
	case "windows":
		return s.local()
	case "darwin":
		return s.defaultConfigDir()
	default:
		if s.home == "" {
			return "", errors.New("paths: no state dir; set HOME")
		}
		return filepath.Join(s.home, ".local", "state", "kwak"), nil
	}
}

func (s sys) socketPath() (string, error) {
	if v, ok, err := s.override(EnvSocket); ok {
		return v, err
	}
	if s.goos == "windows" {
		dir, err := s.local()
		if err != nil {
			return "", err
		}
		p := filepath.Join(dir, SocketName)
		if len(p) > MaxSocketPath(s.goos) {
			return "", fmt.Errorf("paths: socket path %q is too long; set KWAK_SOCKET", p)
		}
		return p, nil
	}
	var dir string
	switch {
	case s.goos == "linux" && s.runUser != "":
		dir = filepath.Join(s.runUser, "kwak")
	case s.home == "":
		return "", errors.New("paths: no home dir for the socket; set HOME")
	case s.goos == "darwin":
		dir = filepath.Join(s.home, "Library", "Caches", "kwak")
	default:
		dir = filepath.Join(s.home, ".cache", "kwak")
	}
	return s.fit(filepath.Join(dir, SocketName))
}

func (s sys) local() (string, error) {
	if s.localAppData == "" {
		return "", errors.New("paths: %LOCALAPPDATA% is not set")
	}
	return filepath.Join(s.localAppData, "kwak"), nil
}

// fit falls back to the shared /tmp/kwak-<uid> when path does not fit in a sockaddr_un. It is
// literal /tmp, not TMPDIR, so every process of the user agrees on it.
func (s sys) fit(path string) (string, error) {
	if len(path) <= MaxSocketPath(s.goos) {
		return path, nil
	}
	if s.uid == "" {
		return "", fmt.Errorf("paths: socket path %q is too long and the uid is unknown", path)
	}
	return "/tmp/kwak-" + s.uid + "/" + SocketName, nil
}

// MaxSocketPath is the longest socket path Go's net package accepts on goos: sun_path less its
// NUL, 108-1 on Linux and Windows, 104-1 on macOS.
func MaxSocketPath(goos string) int {
	if goos == "darwin" {
		return 103
	}
	return 107
}

func baseSys(goos string) sys {
	s := sys{goos: goos, getenv: os.Getenv}
	s.home, _ = os.UserHomeDir()
	s.userConfig, _ = os.UserConfigDir()
	return s
}
