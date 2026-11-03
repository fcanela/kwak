package oskeyring

import (
	"errors"
	"strconv"
)

// ErrNoSessionBus is Preflight's answer when Linux has no session bus to reach the keyring on.
var ErrNoSessionBus = errors.New("no session bus; run under your desktop session or systemd --user")

// preflight is the Linux rule: a bus address in the environment, or the bus socket of
// /run/user/<uid>. "autolaunch:" counts as none, because godbus would autolaunch a stray bus.
func preflight(getenv func(string) string, exists func(string) bool, uid int) error {
	if a := getenv("DBUS_SESSION_BUS_ADDRESS"); a != "" && a != "autolaunch:" {
		return nil
	}
	if exists("/run/user/" + strconv.Itoa(uid) + "/bus") {
		return nil
	}
	return ErrNoSessionBus
}
