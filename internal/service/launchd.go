package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Label is the launchd label of the listener agent.
const Label = "com.github.fcanela.kwak"

// launchd tears a booted-out job down after bootout returns; a bootstrap of the new plist before
// that fails ("5: Input/output error"), so it is retried.
const (
	bootstrapTries = 10
	bootstrapWait  = 500 * time.Millisecond
)

// Sleeper is an optional Runner method; a Runner without one sleeps with time.Sleep.
type Sleeper interface{ Sleep(time.Duration) }

func sleep(r Runner, d time.Duration) {
	if s, ok := r.(Sleeper); ok {
		s.Sleep(d)
		return
	}
	time.Sleep(d)
}

type launchd struct {
	r            Runner
	plist, log   string
	domain, name string // gui/<uid> and gui/<uid>/<label>
}

// Launchd manages ~/Library/LaunchAgents/com.github.fcanela.kwak.plist in the gui/<uid> domain,
// where the login keychain is unlocked (spec 7.2).
func Launchd(r Runner, home string, uid int) Manager {
	domain := "gui/" + strconv.Itoa(uid)
	return &launchd{
		r:      r,
		plist:  filepath.Join(home, "Library", "LaunchAgents", Label+".plist"),
		log:    filepath.Join(home, "Library", "Logs", "kwak.log"),
		domain: domain,
		name:   domain + "/" + Label,
	}
}

// Plist renders the agent for exe, logging to logPath.
func Plist(exe, logPath string) (string, error) {
	if err := checkExe(exe, unixAbs); err != nil {
		return "", err
	}
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>` + Label + `</string>
  <key>ProgramArguments</key><array><string>` + esc(exe) + `</string><string>listen</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>StandardOutPath</key><string>` + esc(logPath) + `</string>
  <key>StandardErrorPath</key><string>` + esc(logPath) + `</string>
</dict></plist>
`, nil
}

func (l *launchd) loaded() bool {
	_, err := l.r.Run("launchctl", "print", l.name)
	return err == nil
}

// Install writes the plist and loads it. A loaded agent is restarted with kickstart -k when the
// plist is unchanged. Otherwise it is unloaded first, since kickstart keeps the loaded plist, and
// the new plist is written only after that, so a failed bootout is retried on the next Install.
func (l *launchd) Install(exe string) error {
	p, err := Plist(exe, l.log)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.log), 0o755); err != nil {
		return err
	}
	tries := 1
	if l.loaded() {
		if old, err := os.ReadFile(l.plist); err == nil && bytes.Equal(old, []byte(p)) {
			return run(l.r, "launchctl", "kickstart", "-k", l.name)
		}
		if err := run(l.r, "launchctl", "bootout", l.name); err != nil {
			return err
		}
		tries = bootstrapTries
	}
	if err := writeFile(l.plist, []byte(p)); err != nil {
		return err
	}
	for i := 1; ; i++ {
		err = run(l.r, "launchctl", "bootstrap", l.domain, l.plist)
		if err == nil || i == tries {
			return err
		}
		sleep(l.r, bootstrapWait)
	}
}

func (l *launchd) Uninstall() error {
	if !exists(l.plist) {
		return ErrNotInstalled
	}
	if l.loaded() {
		if err := run(l.r, "launchctl", "bootout", l.name); err != nil {
			return err
		}
	}
	return os.Remove(l.plist)
}

func (l *launchd) Start() error { return run(l.r, "launchctl", "kickstart", l.name) }

// Stop sends SIGTERM. The listener exits 0, and KeepAlive restarts only a failed exit.
func (l *launchd) Stop() error { return run(l.r, "launchctl", "kill", "SIGTERM", l.name) }

// Status is the "state" launchctl print reports ("running", "not running", ...).
func (l *launchd) Status() (string, error) {
	if !exists(l.plist) {
		return "not installed", nil
	}
	out, err := l.r.Run("launchctl", "print", l.name)
	if err != nil {
		return "not loaded", nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.TrimSpace(k) == "state" {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("launchctl print %s: no state line", l.name)
}
