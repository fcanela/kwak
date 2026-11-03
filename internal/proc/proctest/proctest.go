// Package proctest is a scripted proc.Reader for tests.
package proctest

import (
	"sync"

	"github.com/fcanela/kwak/internal/proc"
)

// Reader is a process table that tests edit while code under test reads it. It is safe for
// concurrent use. Unknown pids give proc.ErrNoProcess.
type Reader struct {
	mu    sync.Mutex
	procs map[int]entry
}

type entry struct {
	self, parent proc.ID
	err          error
}

// New returns an empty table.
func New() *Reader { return &Reader{procs: map[int]entry{}} }

// Add records self with the given parent, replacing any process with the same pid.
func (r *Reader) Add(self, parent proc.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.procs[self.PID] = entry{self: self, parent: parent}
}

// Line adds a parent chain: ids[0] is the child, each next id its parent, the last has none.
func (r *Reader) Line(ids ...proc.ID) {
	for i, id := range ids {
		var parent proc.ID
		if i+1 < len(ids) {
			parent = ids[i+1]
		}
		r.Add(id, parent)
	}
}

// Kill removes pid, so Lookup fails and Alive is false.
func (r *Reader) Kill(pid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.procs, pid)
}

// Fail makes Lookup of pid return err (for example a permission error).
func (r *Reader) Fail(pid int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.procs[pid] = entry{err: err}
}

// Lookup implements proc.Reader.
func (r *Reader) Lookup(pid int) (self, parent proc.ID, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.procs[pid]
	if !ok {
		return proc.ID{}, proc.ID{}, proc.ErrNoProcess
	}
	if e.err != nil {
		return proc.ID{}, proc.ID{}, e.err
	}
	return e.self, e.parent, nil
}

var _ proc.Reader = (*Reader)(nil)
