package osproc

import (
	"errors"

	"github.com/fcanela/kwak/internal/proc"
	"golang.org/x/sys/unix"
)

// sZomb is SZOMB of <sys/proc.h>.
const sZomb = 5

type reader struct{}

// New returns the Reader of this OS. On macOS a zombie counts as not running.
func New() proc.Reader { return reader{} }

func (reader) Lookup(pid int) (self, parent proc.ID, err error) {
	if pid <= 0 {
		return proc.ID{}, proc.ID{}, proc.ErrNoProcess
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// A vanished pid answers zero bytes, which x/sys reports as EIO.
		if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) {
			return proc.ID{}, proc.ID{}, proc.ErrNoProcess
		}
		return proc.ID{}, proc.ID{}, err
	}
	if int(k.Proc.P_pid) != pid || k.Proc.P_stat == sZomb {
		return proc.ID{}, proc.ID{}, proc.ErrNoProcess
	}
	t := k.Proc.P_starttime
	start := uint64(t.Sec)*1_000_000 + uint64(t.Usec)
	return proc.ID{PID: pid, Start: start}, proc.ID{PID: int(k.Eproc.Ppid)}, nil
}
