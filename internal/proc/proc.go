// Package proc names processes by (pid, start time) and walks their ancestry. It has no OS code:
// the OS Reader lives in proc/osproc.
package proc

import (
	"errors"
	"os"
)

// ErrNoProcess is what a Reader returns for a pid that does not run.
var ErrNoProcess = errors.New("proc: no such process")

// ID names a process. PIDs are reused, so a process is always its pid plus its start time.
type ID struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"` // OS start time; compared only for equality
}

// Reader looks up a process and its parent. A parent of PID 0 means none is known. The parent's
// Start may be 0 when the Reader does not know it. On Windows the Reader returns PID 0 for a
// parent that started after its child (a reused pid).
type Reader interface {
	Lookup(pid int) (self, parent ID, err error)
}

// Chain returns [pid, parent, grandparent, ...], at most max entries, each with the start time
// from its own Lookup. Only a failing Lookup of pid itself is an error. The walk stops at a
// parent of PID 0, a pid already in the chain, an ancestor whose Lookup fails, or an ancestor
// whose start time differs from the one its child reported.
func Chain(r Reader, pid, max int) ([]ID, error) {
	if max <= 0 {
		return nil, nil
	}
	self, parent, err := r.Lookup(pid)
	if err != nil {
		return nil, err
	}
	chain := []ID{self}
	for len(chain) < max && parent.PID > 0 && !has(chain, parent.PID) {
		next, grand, err := r.Lookup(parent.PID)
		if err != nil || (parent.Start != 0 && parent.Start != next.Start) {
			break
		}
		chain = append(chain, next)
		parent = grand
	}
	return chain, nil
}

func has(chain []ID, pid int) bool {
	for _, id := range chain {
		if id.PID == pid {
			return true
		}
	}
	return false
}

// Alive reports whether id.PID still runs with the same start time.
func Alive(r Reader, id ID) bool {
	if id.PID <= 0 {
		return false
	}
	self, _, err := r.Lookup(id.PID)
	return err == nil && self == id
}

// ChainDepth is how many processes a caller chain holds, whether the listener reads it from the
// peer PID or a Windows client sends its own (spec 5.6, 5.7).
const ChainDepth = 8

// ClientChain returns the chain a client sends with its requests: its own ancestry on Windows,
// where the listener cannot read the peer PID, and nil elsewhere.
func ClientChain(goos string, r Reader) func() []ID {
	if goos != "windows" || r == nil {
		return nil
	}
	return func() []ID {
		c, err := Chain(r, os.Getpid(), ChainDepth)
		if err != nil {
			return nil
		}
		return c
	}
}
