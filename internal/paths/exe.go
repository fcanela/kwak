package paths

import (
	"os"
	"os/exec"
	"path/filepath"
)

// Executable is the path to write into files that start kwak later: the user service, Codex's
// config and hooks. When the kwak on PATH is this same binary it is that PATH entry, links kept,
// so a package manager's link (Nix's ~/.nix-profile/bin/kwak) survives an upgrade and a garbage
// collection. Otherwise it is the running binary with symlinks resolved.
func Executable() (string, error) { return executable(os.Executable, exec.LookPath) }

func executable(self func() (string, error), lookPath func(string) (string, error)) (string, error) {
	exe, err := self()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	found, err := lookPath("kwak")
	if err != nil {
		return exe, nil
	}
	if found, err = filepath.Abs(found); err != nil {
		return exe, nil
	}
	a, errA := os.Stat(found)
	b, errB := os.Stat(exe)
	if errA != nil || errB != nil || !os.SameFile(a, b) {
		return exe, nil
	}
	return found, nil
}
