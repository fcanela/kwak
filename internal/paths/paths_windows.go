package paths

import (
	"os"
	"os/user"
)

func current() sys {
	s := baseSys("windows")
	s.localAppData = os.Getenv("LOCALAPPDATA")
	if u, err := user.Current(); err == nil {
		s.uid = u.Uid
	}
	return s
}
