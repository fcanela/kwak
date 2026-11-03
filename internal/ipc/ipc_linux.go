package ipc

import (
	"net"

	"golang.org/x/sys/unix"
)

const (
	peerCredOpt = unix.SO_PEERCRED
	maxPath     = 107
)

func ucred(c net.Conn) (*unix.Ucred, bool, error) {
	var cred *unix.Ucred
	ok, err := control(c, func(fd int) (err error) {
		cred, err = unix.GetsockoptUcred(fd, unix.SOL_SOCKET, peerCredOpt)
		return err
	})
	return cred, ok, err
}

// PeerPID returns the peer's pid from SO_PEERCRED. ok is false for a conn that is not a unix
// socket.
func PeerPID(c net.Conn) (pid int, ok bool, err error) {
	cred, ok, err := ucred(c)
	if !ok || err != nil {
		return 0, false, err
	}
	return int(cred.Pid), true, nil
}

// PeerUID returns the peer's uid from SO_PEERCRED. ok is false for a conn that is not a unix
// socket.
func PeerUID(c net.Conn) (uid int, ok bool, err error) {
	cred, ok, err := ucred(c)
	if !ok || err != nil {
		return 0, false, err
	}
	return int(cred.Uid), true, nil
}
