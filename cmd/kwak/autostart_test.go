package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fcanela/kwak/internal/paths"
	"github.com/fcanela/kwak/internal/service"
	"github.com/fcanela/kwak/internal/service/servicetest"
)

// kwak mcp starts the listener's user service only when it is installed and is this kwak's
// listener (decision 46). Every test uses a recorder: nothing here starts a real service.
func TestAutostart(t *testing.T) {
	cases := []struct {
		name      string
		env       string // an override that is set
		dev       bool
		config    string // "" is no config.json
		installed bool
		want      string // "" is started
	}{
		{name: "installed", config: `{"version":1}`, installed: true},
		{name: "on in the file", config: `{"version":1,"listener_autostart":true}`, installed: true},
		{name: "off", config: `{"version":1,"listener_autostart":false}`, installed: true, want: "listener_autostart is off"},
		{name: "not installed", config: `{"version":1}`, want: "not installed as a user service; run kwak install"},
		{name: "not paired", installed: true, want: "not paired"},
		{name: "broken config", config: `{`, installed: true, want: "not valid"},
		{name: "dev build", dev: true, config: `{"version":1}`, installed: true, want: "a dev build never starts"},
		{name: "own socket", env: paths.EnvSocket, config: `{"version":1}`, installed: true, want: "KWAK_SOCKET is set"},
		{name: "own config folder", env: paths.EnvConfigDir, config: `{"version":1}`, installed: true, want: "KWAK_CONFIG_DIR is set"},
		{name: "own state folder", env: paths.EnvStateDir, config: `{"version":1}`, installed: true, want: "KWAK_STATE_DIR is set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, home := t.TempDir(), t.TempDir()
			if c.config != "" {
				os.WriteFile(filepath.Join(dir, "config.json"), []byte(c.config), 0o600)
			}
			if c.installed {
				unit := filepath.Join(home, ".config", "systemd", "user", service.UnitName)
				os.MkdirAll(filepath.Dir(unit), 0o700)
				os.WriteFile(unit, nil, 0o600)
			}
			rec, built := servicetest.New(), 0
			getenv := func(k string) string {
				if k == c.env {
					return "/tmp/x"
				}
				return ""
			}
			err := autostart(getenv, func() (string, error) { return dir, nil }, c.dev,
				func() service.Manager { built++; return service.Systemd(rec, home) })
			started := slices.Contains(rec.Calls(), servicetest.Line("systemctl", "--user", "start", service.UnitName))
			if c.want == "" {
				if err != nil || !started {
					t.Fatalf("not started: %v %v", err, rec.Calls())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) || started {
				t.Fatalf("got %v, started %v, want %q", err, started, c.want)
			}
			if (c.env != "" || c.dev) && built != 0 {
				t.Errorf("the service was built")
			}
		})
	}
}

// The wiring of kwak mcp's start: a send that finds no listener starts the installed service
// through sys, and a dev build never does (decision 46, I2). The rig's overrides stay set for
// paths, so kwak mcp dials the test socket; only autostart sees them unset. The recorder runs
// nothing real.
func TestMCPSendStartsTheService(t *testing.T) {
	const send = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send","arguments":{"text":"hi"}}}` + "\n"
	for _, dev := range []bool{false, true} {
		r := newRig(t)
		r.Dev = dev
		r.Getenv = func(k string) string {
			if k == paths.EnvSocket || k == paths.EnvConfigDir || k == paths.EnvStateDir {
				return ""
			}
			return os.Getenv(k)
		}
		cfg := filepath.Join(os.Getenv(paths.EnvConfigDir), "config.json")
		unit := filepath.Join(r.home, ".config", "systemd", "user", service.UnitName)
		for _, f := range []string{cfg, unit} {
			if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		os.WriteFile(cfg, []byte(`{"version":1}`), 0o600)
		os.WriteFile(unit, nil, 0o600)
		start := servicetest.Line("systemctl", "--user", "start", service.UnitName)
		// A failed start answers at once, so the send does not wait for a listener.
		r.rec.On(start, "", errors.New("test start"))
		code, out, errs := r.run(send, "mcp", "--harness", "claude")
		calls := r.rec.Calls()
		started := slices.Contains(calls, start)
		if dev {
			if code != 0 || !strings.Contains(out, "kwak did not start it: a dev build never starts the user service") ||
				len(calls) != 0 || r.built["service"] != 0 {
				t.Errorf("dev: exit %d, calls %v, built %v, out %q, stderr %q", code, calls, r.built, out, errs)
			}
			continue
		}
		if code != 0 || !strings.Contains(out, "kwak did not start it: systemctl --user start kwak.service: test start") || !started {
			t.Errorf("release: exit %d, calls %v, out %q, stderr %q", code, calls, out, errs)
		}
	}
}
