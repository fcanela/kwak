package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fcanela/kwak/internal/cli"
	"github.com/fcanela/kwak/internal/paths"
	"github.com/fcanela/kwak/internal/service"
	"github.com/fcanela/kwak/internal/wire"
)

// startListener is how kwak mcp starts the listener when a send finds it down (decision 46,
// spec 5.6). It only starts the installed user service, never a process of its own.
func (s sys) startListener() error {
	return autostart(s.Getenv, paths.ConfigDir, s.Dev, s.service)
}

// autostart starts the user service when all of these hold: no folder override is set (the
// service runs the default folders, so it may not be this kwak's listener), this is not a dev
// build, the machine is paired, listener_autostart is not off, and the service is installed.
// The error says which one did not hold.
func autostart(getenv func(string) string, configDir func() (string, error), dev bool, svc func() service.Manager) error {
	for _, v := range []string{paths.EnvSocket, paths.EnvConfigDir, paths.EnvStateDir} {
		if getenv(v) != "" {
			return fmt.Errorf("%s is set, so the user service is not this kwak's listener; run kwak listen", v)
		}
	}
	if dev {
		return errors.New("a dev build never starts the user service; run kwak listen in a terminal")
	}
	dir, err := configDir()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, cli.ConfigFile))
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("this machine is not paired; run kwak pair")
	}
	if err != nil {
		return err
	}
	var c wire.MachineConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("%s is not valid: %v", cli.ConfigFile, err)
	}
	if !c.Autostart() {
		return errors.New("listener_autostart is off in config.json")
	}
	m := svc()
	st, err := m.Status()
	if err != nil {
		return err
	}
	if st == "not installed" {
		return errors.New("it is not installed as a user service; run kwak install")
	}
	return m.Start()
}
