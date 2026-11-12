//go:build !windows

package check

import (
	"os/exec"
	"syscall"
)

// group starts cmd in its own process group; a cancel kills the whole group.
func group(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
