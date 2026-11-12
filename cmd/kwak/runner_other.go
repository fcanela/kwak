//go:build !windows

package main

import (
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

// startTree starts cmd in its own process group, so a run's children stop with it, also when
// claude is a wrapper script that does not exec (spec 5.11). A cancel sends SIGTERM to the group.
// Once the leader has exited, the rest of the group gets what is left of grace after a cancel
// (nothing after a normal end), then SIGKILL. A leader still running after grace is killed by
// exec's WaitDelay, which the caller sets to grace.
func startTree(cmd *exec.Cmd, grace time.Duration) (func() error, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var cancelled atomic.Int64
	cmd.Cancel = func() error {
		cancelled.CompareAndSwap(0, time.Now().UnixNano())
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pgid := cmd.Process.Pid
	return func() error {
		err := cmd.Wait()
		if at := cancelled.Load(); at != 0 {
			deadline := time.Unix(0, at).Add(grace)
			for time.Now().Before(deadline) && syscall.Kill(-pgid, 0) == nil {
				time.Sleep(20 * time.Millisecond)
			}
		}
		syscall.Kill(-pgid, syscall.SIGKILL)
		return err
	}, nil
}
