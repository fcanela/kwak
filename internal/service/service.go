// Package service installs the listener as a user service (spec 7): a systemd user unit on Linux,
// a launchd agent on macOS, the HKCU Run key on Windows. No admin rights. Commands go through
// Runner, so tests check the exact commands and files on any OS.
package service

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fcanela/kwak/internal/atomicfile"
	"github.com/fcanela/kwak/internal/paths"
)

// Runner runs an OS command and returns its combined output, also on error.
type Runner interface {
	Run(name string, args ...string) ([]byte, error)
}

// Manager installs, removes and controls the listener's user service.
type Manager interface {
	Install(exe string) error // idempotent: rewrites the service and (re)starts the listener
	Uninstall() error         // ErrNotInstalled when there was nothing to remove
	Start() error
	Stop() error
	Status() (string, error)
}

var (
	// ErrNotInstalled is returned by Uninstall when no service was installed.
	ErrNotInstalled = errors.New("no kwak user service is installed")
	// ErrNotRunning is returned by the shutdown op when no listener answers.
	ErrNotRunning = errors.New("the kwak listener is not running")
)

// ExecRunner is the production Runner.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// Executable is the kwak path the service starts: see paths.Executable.
func Executable() (string, error) { return paths.Executable() }

// run runs a command and folds its output into the error.
func run(r Runner, name string, args ...string) error {
	out, err := r.Run(name, args...)
	if err == nil {
		return nil
	}
	if msg := strings.Join(strings.Fields(string(out)), " "); msg != "" {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
}

// checkExe takes the absolute-path rule of the target OS, not of the OS running the code, so
// every manager renders on any OS.
func checkExe(exe string, abs func(string) bool) error {
	if !abs(exe) {
		return fmt.Errorf("the kwak path %q is not absolute", exe)
	}
	if strings.ContainsAny(exe, "\x00\n\r\"") {
		return fmt.Errorf("the kwak path %q holds a character a service file cannot carry", exe)
	}
	return nil
}

// writeFile writes data to path through a temp file and a rename, creating the directory.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	return atomicfile.Write(path, data, 0o644)
}

func unixAbs(p string) bool { return strings.HasPrefix(p, "/") }

// windowsAbs accepts a drive path (C:\...) or a UNC path (\\host\share\...).
func windowsAbs(p string) bool {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		c := p[0] | 0x20
		return c >= 'a' && c <= 'z'
	}
	return strings.HasPrefix(p, `\\`)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
