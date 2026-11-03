package ipc

import (
	"net"

	"golang.org/x/sys/unix"
)

const (
	peerCredOpt = unix.LOCAL_PEERCRED
	peerPIDOpt  = unix.LOCAL_PEERPID
	maxPath     = 103
)

// PeerPID returns the peer's pid from LOCAL_PEERPID. ok is false for a conn that is not a unix
// socket.
func PeerPID(c net.Conn) (pid int, ok bool, err error) {
	ok, err = control(c, func(fd int) (err error) {
		pid, err = unix.GetsockoptInt(fd, unix.SOL_LOCAL, peerPIDOpt)
		return err
	})
	if !ok || err != nil {
		return 0, false, err
	}
	return pid, true, nil
}

// PeerUID returns the peer's uid from LOCAL_PEERCRED. ok is false for a conn that is not a unix
// socket.
func PeerUID(c net.Conn) (uid int, ok bool, err error) {
	ok, err = control(c, func(fd int) error {
		x, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, peerCredOpt)
		if err == nil {
			uid = int(x.Uid)
		}
		return err
	})
	if !ok || err != nil {
		return 0, false, err
	}
	return uid, true, nil
}
