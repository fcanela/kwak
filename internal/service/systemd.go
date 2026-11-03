package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// UnitName is the systemd user unit.
const UnitName = "kwak.service"

type systemd struct {
	r    Runner
	path string
}

// Systemd manages ~/.config/systemd/user/kwak.service through systemctl --user (spec 7.1).
func Systemd(r Runner, home string) Manager {
	return &systemd{r: r, path: filepath.Join(home, ".config", "systemd", "user", UnitName)}
}

// Unit renders the unit file for exe. The condition skips the unit while exe is missing (a machine
// provisioned before the binary arrives, a removed binary), where Restart= would loop every 5 s.
func Unit(exe string) (string, error) {
	if err := checkExe(exe, unixAbs); err != nil {
		return "", err
	}
	if !unitSafe(exe) {
		return "", fmt.Errorf("the kwak path %q holds a quote, a backslash or a control character, which systemd refuses in a command path", exe)
	}
	return `[Unit]
Description=kwak listener
After=dbus.socket
Wants=dbus.socket
ConditionPathExists=` + strings.ReplaceAll(exe, "%", "%%") + `

[Service]
Type=simple
ExecStart=` + execArg(exe) + ` listen
Restart=on-failure
RestartSec=5
NoNewPrivileges=yes

[Install]
WantedBy=default.target
`, nil
}

// unitSafe follows systemd's string_is_safe, which every ExecStart command path must pass.
func unitSafe(p string) bool {
	for _, c := range p {
		if c < ' ' || c == 0x7f || c == '\\' || c == '\'' || c == '"' {
			return false
		}
	}
	return true
}

// execArg writes a unitSafe path as the ExecStart command: % starts a specifier and a space splits
// words. $ stays as is, since systemd does not expand variables in the command path.
func execArg(p string) string {
	p = strings.ReplaceAll(p, "%", "%%")
	if strings.Contains(p, " ") {
		return `"` + p + `"`
	}
	return p
}

func (s *systemd) ctl(args ...string) error {
	return run(s.r, "systemctl", append([]string{"--user"}, args...)...)
}

// Install writes the unit, reloads, enables and starts it, and restarts a listener that was
// already running so a new binary takes over (spec 7.4).
func (s *systemd) Install(exe string) error {
	unit, err := Unit(exe)
	if err != nil {
		return err
	}
	if err := writeFile(s.path, []byte(unit)); err != nil {
		return err
	}
	_, activeErr := s.r.Run("systemctl", "--user", "is-active", "--quiet", UnitName)
	if err := s.ctl("daemon-reload"); err != nil {
		return err
	}
	if err := s.ctl("enable", "--now", UnitName); err != nil {
		return err
	}
	if activeErr == nil {
		return s.ctl("restart", UnitName)
	}
	return nil
}

func (s *systemd) Uninstall() error {
	if !exists(s.path) {
		return ErrNotInstalled
	}
	if err := s.ctl("disable", "--now", UnitName); err != nil {
		return err
	}
	if err := os.Remove(s.path); err != nil {
		return err
	}
	return s.ctl("daemon-reload")
}

func (s *systemd) Start() error { return s.ctl("start", UnitName) }
func (s *systemd) Stop() error  { return s.ctl("stop", UnitName) }

// Status is systemd's word for the unit ("active", "inactive", "failed", ...).
func (s *systemd) Status() (string, error) {
	if !exists(s.path) {
		return "not installed", nil
	}
	out, err := s.r.Run("systemctl", "--user", "is-active", UnitName)
	if st := strings.TrimSpace(string(out)); st != "" && !strings.Contains(st, "\n") {
		return st, nil
	}
	if err != nil {
		return "", fmt.Errorf("systemctl --user is-active: %w", err)
	}
	return "unknown", nil
}
