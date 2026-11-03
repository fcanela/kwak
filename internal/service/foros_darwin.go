package service

import "os"

// ForOS returns the launchd manager for the current user.
func ForOS(r Runner, home string) Manager { return Launchd(r, home, os.Getuid()) }

func keepSupervising() func() bool { return nil }
