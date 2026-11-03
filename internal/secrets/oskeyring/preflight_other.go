//go:build !linux

package oskeyring

// Preflight has nothing to check outside Linux.
func Preflight() error { return nil }
