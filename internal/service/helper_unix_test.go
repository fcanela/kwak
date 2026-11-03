//go:build unix

package service_test

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func sessionLeader() string {
	sid, err := unix.Getsid(0)
	if err != nil {
		return "err"
	}
	return strconv.FormatBool(sid == os.Getpid())
}
