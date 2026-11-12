package check

import "os/exec"

// group keeps exec's default on Windows: a cancel kills the command itself.
func group(*exec.Cmd) {}
