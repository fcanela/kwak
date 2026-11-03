// Package osproc is the OS proc.Reader: /proc on Linux, sysctl on macOS, Toolhelp32 on Windows.
package osproc

import (
	"bytes"
	"errors"
	"strconv"

	"github.com/fcanela/kwak/internal/proc"
)

// parseStat reads /proc/<pid>/stat. The command name sits in parentheses and may hold spaces and
// ')', so the fields are counted after the last ')'. zombie is true for state Z or X.
func parseStat(b []byte) (ppid int, start uint64, zombie bool, err error) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, 0, false, errors.New("osproc: stat has no command name")
	}
	f := bytes.Fields(b[i+1:])
	if len(f) < 20 {
		return 0, 0, false, errors.New("osproc: stat has too few fields")
	}
	if ppid, err = strconv.Atoi(string(f[1])); err != nil {
		return 0, 0, false, errors.New("osproc: bad ppid in stat")
	}
	if start, err = strconv.ParseUint(string(f[19]), 10, 64); err != nil {
		return 0, 0, false, errors.New("osproc: bad start time in stat")
	}
	return ppid, start, string(f[0]) == "Z" || string(f[0]) == "X", nil
}

// olderParent applies the Windows rule: Windows keeps a dead parent's pid in its children, and
// the pid may since belong to a newer process. A parent counts only when it is known to have
// started before the child (both times are FILETIMEs; 0 means unknown).
func olderParent(childStart uint64, ppid int, parentStart uint64) proc.ID {
	if ppid <= 0 || childStart == 0 || parentStart == 0 || parentStart >= childStart {
		return proc.ID{}
	}
	return proc.ID{PID: ppid, Start: parentStart}
}
