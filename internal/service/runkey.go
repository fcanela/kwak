package service

import (
	"errors"
	"io/fs"
	"strings"
)

// RunValue is the name of kwak's value under HKCU\Software\Microsoft\Windows\CurrentVersion\Run,
// where Windows starts programs at logon, per user, without admin.
const RunValue = "kwak"

// Registry is the Run key: string values by name. Get and Delete return fs.ErrNotExist for a
// missing value.
type Registry interface {
	Get(name string) (string, error)
	Set(name, value string) error
	Delete(name string) error
}

type runKey struct {
	r        Runner
	reg      Registry
	shutdown func() error
}

// RunKey manages the Windows Run key value (spec 7.3). The listener starts through
// "kwak.exe listen --detach"; shutdown sends the shutdown op over the socket (ErrNotRunning when
// none answers).
func RunKey(r Runner, reg Registry, shutdown func() error) Manager {
	return &runKey{r: r, reg: reg, shutdown: shutdown}
}

// KeepWhileInstalled is a Supervisor.Keep that says stop once the Run key value is gone.
func KeepWhileInstalled(reg Registry) func() bool {
	return func() bool {
		_, err := reg.Get(RunValue)
		return !errors.Is(err, fs.ErrNotExist)
	}
}

// RunCommand is the Run key value for exe.
func RunCommand(exe string) string { return `"` + exe + `" listen --detach` }

// Install sets the value and starts the listener. A listener already running keeps running: the
// new one cannot take listen.lock and exits 0, and Windows cannot replace a running kwak.exe
// anyway (spec 7.4).
func (k *runKey) Install(exe string) error {
	if err := checkExe(exe, windowsAbs); err != nil {
		return err
	}
	if err := k.reg.Set(RunValue, RunCommand(exe)); err != nil {
		return err
	}
	return run(k.r, exe, "listen", "--detach")
}

func (k *runKey) Uninstall() error {
	err := k.reg.Delete(RunValue)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return err
	}
	err = k.shutdown()
	if errors.Is(err, ErrNotRunning) {
		if missing {
			return ErrNotInstalled
		}
		return nil
	}
	return err
}

func (k *runKey) Start() error {
	v, err := k.reg.Get(RunValue)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotInstalled
	}
	if err != nil {
		return err
	}
	exe, ok := strings.CutSuffix(v, `" listen --detach`)
	if !ok || !strings.HasPrefix(exe, `"`) {
		return errors.New("the kwak Run key value is not one kwak wrote; run kwak install")
	}
	return run(k.r, exe[1:], "listen", "--detach")
}

func (k *runKey) Stop() error { return k.shutdown() }

// Status says whether the Run key value exists.
func (k *runKey) Status() (string, error) {
	_, err := k.reg.Get(RunValue)
	switch {
	case err == nil:
		return "installed", nil
	case errors.Is(err, fs.ErrNotExist):
		return "not installed", nil
	}
	return "", err
}
