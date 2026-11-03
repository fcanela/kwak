package ipc

import (
	"errors"
	"net"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

const (
	lockFlags = windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY
	maxPath   = 107
)

func owner(os.FileInfo) (int, bool) { return 0, false }

// PeerPID returns ok=false until manual check M8 shows SIO_AF_UNIX_GETPEERPID works.
func PeerPID(c net.Conn) (pid int, ok bool, err error) { return 0, false, nil }

// PeerUID returns ok=false: Windows relies on the directory ACL.
func PeerUID(c net.Conn) (uid int, ok bool, err error) { return 0, false, nil }

// Lock takes an exclusive LockFileEx lock on path without waiting, creating the file. It
// returns ErrLocked when another holder has it. release drops it.
func Lock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(h, lockFlags, 0, 1, 0, ol); err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrLocked
		}
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			windows.UnlockFileEx(h, 0, 1, 0, ol)
			f.Close()
		})
	}, nil
}
