//go:build linux || darwin

package ipc

import (
	"errors"
	"net"
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

func owner(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// Lock takes an exclusive flock on path without waiting, creating the file with mode 0600.
// It returns ErrLocked when another holder has it. release drops it; the lock also dies with
// the process.
func Lock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { f.Close() }) }, nil
}

func control(c net.Conn, fn func(fd int) error) (bool, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return false, nil
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false, err
	}
	var ferr error
	if err := raw.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return false, err
	}
	if ferr != nil {
		return false, ferr
	}
	return true, nil
}
