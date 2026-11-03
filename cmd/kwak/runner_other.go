//go:build !windows

package main

import "os/exec"

// startTree starts cmd. On Unix the npm claude is a script that execs node in the same process,
// so cancelling cmd kills the run.
func startTree(cmd *exec.Cmd) (func() error, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Wait, nil
}
