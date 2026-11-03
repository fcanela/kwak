//go:build !windows

package paths

import (
	"os"
	"runtime"
	"strconv"
	"syscall"
)

func current() sys {
	s := baseSys(runtime.GOOS)
	uid := os.Getuid()
	s.uid = strconv.Itoa(uid)
	if runtime.GOOS == "linux" {
		d := "/run/user/" + s.uid
		if ownedDir(d, uid) {
			s.runUser = d
		}
	}
	return s
}

// ownedDir reports whether path is a directory, not a symlink, owned by uid.
func ownedDir(path string, uid int) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.IsDir() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid
}
