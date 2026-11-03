//go:build !windows && !darwin

package service

// ForOS returns the systemd user manager.
func ForOS(r Runner, home string) Manager { return Systemd(r, home) }

func keepSupervising() func() bool { return nil }
