package oskeyring

import "os"

// Preflight refuses to go on when no session bus exists, so go-keyring never autolaunches one.
func Preflight() error {
	return preflight(os.Getenv, func(p string) bool { _, err := os.Stat(p); return err == nil }, os.Getuid())
}
