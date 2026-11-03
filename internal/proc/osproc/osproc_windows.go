package osproc

import (
	"errors"
	"unsafe"

	"github.com/fcanela/kwak/internal/proc"
	"golang.org/x/sys/windows"
)

type reader struct{}

// New returns the Reader of this OS. On Windows the parent is PID 0 unless it is known to have
// started before the child.
func New() proc.Reader { return reader{} }

func (reader) Lookup(pid int) (self, parent proc.ID, err error) {
	if pid <= 0 {
		return proc.ID{}, proc.ID{}, proc.ErrNoProcess
	}
	ppid, err := parentPID(uint32(pid))
	if err != nil {
		return proc.ID{}, proc.ID{}, err
	}
	start, err := created(uint32(pid))
	if err != nil {
		return proc.ID{}, proc.ID{}, err
	}
	var pstart uint64
	if ppid > 0 {
		pstart, _ = created(uint32(ppid))
	}
	return proc.ID{PID: pid, Start: start}, olderParent(start, ppid, pstart), nil
}

func parentPID(pid uint32) (int, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == pid {
			return int(e.ParentProcessID), nil
		}
	}
	if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return 0, proc.ErrNoProcess
	}
	return 0, err
}

// created is the process creation time as a FILETIME. A process that exited but still has open
// handles keeps its pid, so an exit time counts as not running.
func created(pid uint32) (uint64, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return 0, proc.ErrNoProcess
		}
		return 0, err
	}
	defer windows.CloseHandle(h)
	var c, x, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &x, &k, &u); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err == nil && code != stillActive {
		return 0, proc.ErrNoProcess
	}
	return uint64(c.HighDateTime)<<32 | uint64(c.LowDateTime), nil
}

const stillActive = 259
