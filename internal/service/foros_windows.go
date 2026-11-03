package service

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/windows/registry"
)

// ForOS returns the Run key manager. home is unused on Windows.
func ForOS(r Runner, home string) Manager { return RunKey(r, runKeyRegistry{}, Shutdown) }

const runSubkey = `Software\Microsoft\Windows\CurrentVersion\Run`

// keepSupervising stops the supervisor once kwak uninstall removed the Run key value (spec 7.3).
func keepSupervising() func() bool { return KeepWhileInstalled(runKeyRegistry{}) }

type runKeyRegistry struct{}

func (runKeyRegistry) Get(name string) (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runSubkey, registry.QUERY_VALUE)
	if err != nil {
		return "", notExist(err)
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	return v, notExist(err)
}

func (runKeyRegistry) Set(name, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runSubkey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, value)
}

func (runKeyRegistry) Delete(name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runSubkey, registry.SET_VALUE)
	if err != nil {
		return notExist(err)
	}
	defer k.Close()
	return notExist(k.DeleteValue(name))
}

func notExist(err error) error {
	if errors.Is(err, registry.ErrNotExist) {
		return fs.ErrNotExist
	}
	return err
}
