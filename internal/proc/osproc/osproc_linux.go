package osproc

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"syscall"

	"github.com/fcanela/kwak/internal/proc"
)

type reader struct{ root string }

// New returns the Reader of this OS. On Linux a zombie counts as not running.
func New() proc.Reader { return reader{root: "/proc"} }

func (r reader) Lookup(pid int) (self, parent proc.ID, err error) {
	if pid <= 0 {
		return proc.ID{}, proc.ID{}, proc.ErrNoProcess
	}
	b, err := os.ReadFile(r.root + "/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return proc.ID{}, proc.ID{}, proc.ErrNoProcess
		}
		return proc.ID{}, proc.ID{}, err
	}
	ppid, start, zombie, err := parseStat(b)
	if err != nil {
		return proc.ID{}, proc.ID{}, err
	}
	if zombie {
		return proc.ID{}, proc.ID{}, proc.ErrNoProcess
	}
	return proc.ID{PID: pid, Start: start}, proc.ID{PID: ppid}, nil
}
