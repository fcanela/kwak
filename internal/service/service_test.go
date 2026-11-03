package service_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fcanela/kwak/internal/service"
	"github.com/fcanela/kwak/internal/service/servicetest"
)

var errExit3 = errors.New("exit status 3")

func sameCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("calls:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const specUnit = `[Unit]
Description=kwak listener
After=dbus.socket
Wants=dbus.socket
ConditionPathExists=/home/u/go/bin/kwak

[Service]
Type=simple
ExecStart=/home/u/go/bin/kwak listen
Restart=on-failure
RestartSec=5
NoNewPrivileges=yes

[Install]
WantedBy=default.target
`

func TestUnit(t *testing.T) {
	got, err := service.Unit("/home/u/go/bin/kwak")
	if err != nil || got != specUnit {
		t.Fatalf("unit (%v):\n%s", err, got)
	}
	cases := map[string]string{
		"/home/a b/kwak":     `ExecStart="/home/a b/kwak" listen`,
		"/home/a b/%n/kwa":   `ConditionPathExists=/home/a b/%%n/kwa`,
		"/home/u/50%/kwak":   `ExecStart=/home/u/50%%/kwak listen`,
		"/home/u/$x/kwak":    `ExecStart=/home/u/$x/kwak listen`,
		"/home/a b$x/kwak":   `ExecStart="/home/a b$x/kwak" listen`,
		"/home/a b/%n/kwak":  `ExecStart="/home/a b/%%n/kwak" listen`,
		"/home/u/ünï/kwak":   `ExecStart=/home/u/ünï/kwak listen`,
		"/home/u/go/bin/kw;": `ExecStart=/home/u/go/bin/kw; listen`,
	}
	for exe, want := range cases {
		got, err := service.Unit(exe)
		if err != nil || !strings.Contains(got, "\n"+want+"\n") {
			t.Errorf("%q (%v): want line %s in\n%s", exe, err, want, got)
		}
	}
	bad := []string{"kwak", "go/bin/kwak", "/a\nb/kwak", `/a"b/kwak`, `C:\kwak.exe`,
		`/home/u/a\b/kwak`, "/home/it's/kwak", "/home/t\tab/kwak", "/home/u/\x7f/kwak", "/home/u/\x01/kwak"}
	for _, exe := range bad {
		if _, err := service.Unit(exe); err == nil {
			t.Errorf("%q: no error", exe)
		}
	}
}

func TestSystemdInstall(t *testing.T) {
	home := t.TempDir()
	rec := servicetest.New()
	m := service.Systemd(rec, home)
	unitPath := filepath.Join(home, ".config", "systemd", "user", "kwak.service")
	isActive := "systemctl --user is-active --quiet kwak.service"

	rec.On(isActive, "", errExit3)
	if err := m.Install("/home/u/go/bin/kwak"); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(),
		isActive,
		"systemctl --user daemon-reload",
		"systemctl --user enable --now kwak.service")
	if got := readFile(t, unitPath); got != specUnit {
		t.Fatalf("unit file:\n%s", got)
	}

	rec.On(isActive, "", nil)
	if err := m.Install("/home/u/go/bin/kwak"); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(),
		isActive,
		"systemctl --user daemon-reload",
		"systemctl --user enable --now kwak.service",
		"systemctl --user restart kwak.service")
}

func TestSystemdInstallFails(t *testing.T) {
	rec := servicetest.New()
	m := service.Systemd(rec, t.TempDir())
	rec.On("systemctl --user daemon-reload", "Failed to connect to bus: No medium found\n", errors.New("exit status 1"))
	err := m.Install("/home/u/go/bin/kwak")
	if err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("err = %v", err)
	}
	if strings.ContainsRune(err.Error(), '\n') {
		t.Fatalf("err has a newline: %q", err)
	}
	if err := m.Install("kwak"); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestSystemdUninstall(t *testing.T) {
	home := t.TempDir()
	rec := servicetest.New()
	m := service.Systemd(rec, home)
	if err := m.Uninstall(); !errors.Is(err, service.ErrNotInstalled) {
		t.Fatalf("err = %v", err)
	}
	sameCalls(t, rec.Calls())
	if err := m.Install("/home/u/go/bin/kwak"); err != nil {
		t.Fatal(err)
	}
	rec.Calls()
	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(),
		"systemctl --user disable --now kwak.service",
		"systemctl --user daemon-reload")
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", "kwak.service")); !os.IsNotExist(err) {
		t.Fatalf("unit file left: %v", err)
	}
}

func TestSystemdControl(t *testing.T) {
	rec := servicetest.New()
	m := service.Systemd(rec, t.TempDir())
	if st, err := m.Status(); err != nil || st != "not installed" {
		t.Fatalf("status = %q, %v", st, err)
	}
	if err := m.Install("/home/u/go/bin/kwak"); err != nil {
		t.Fatal(err)
	}
	rec.Calls()
	rec.On("systemctl --user is-active kwak.service", "inactive\n", errExit3)
	if st, err := m.Status(); err != nil || st != "inactive" {
		t.Fatalf("status = %q, %v", st, err)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(),
		"systemctl --user is-active kwak.service",
		"systemctl --user start kwak.service",
		"systemctl --user stop kwak.service")
}

// TestSystemdAnalyzeVerify checks that systemd loads the unit and finds the binary, for plain
// paths and for paths with characters a unit file treats specially.
func TestSystemdAnalyzeVerify(t *testing.T) {
	sa, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is not installed")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	exes := []string{exe}
	for _, d := range []string{"d$x", "${HOME}", "a b", "a b$x", "50%n", "kw;", "ünï"} {
		p := filepath.Join(dir, d, "kwak")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		exes = append(exes, p)
	}
	for i, exe := range exes {
		unit, err := service.Unit(exe)
		if err != nil {
			t.Fatalf("%q: %v", exe, err)
		}
		p := filepath.Join(dir, fmt.Sprintf("kwakverify%d.service", i))
		if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(sa, "--user", "verify", p).CombinedOutput()
		if err != nil {
			t.Errorf("%q: systemd-analyze --user verify: %v\n%s", exe, err, out)
		}
	}
}

const specPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.github.fcanela.kwak</string>
  <key>ProgramArguments</key><array><string>/Users/u/go/bin/kwak</string><string>listen</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>StandardOutPath</key><string>/Users/u/Library/Logs/kwak.log</string>
  <key>StandardErrorPath</key><string>/Users/u/Library/Logs/kwak.log</string>
</dict></plist>
`

func TestPlist(t *testing.T) {
	got, err := service.Plist("/Users/u/go/bin/kwak", "/Users/u/Library/Logs/kwak.log")
	if err != nil || got != specPlist {
		t.Fatalf("plist (%v):\n%s", err, got)
	}
	got, err = service.Plist("/Users/a&b <c>/kwak", "/Users/a&b <c>/Library/Logs/kwak.log")
	if err != nil || !strings.Contains(got, "<string>/Users/a&amp;b &lt;c&gt;/kwak</string>") {
		t.Fatalf("escaping (%v):\n%s", err, got)
	}
}

func TestLaunchd(t *testing.T) {
	home := t.TempDir()
	rec := servicetest.New()
	m := service.Launchd(rec, home, 501)
	plist := filepath.Join(home, "Library", "LaunchAgents", "com.github.fcanela.kwak.plist")
	print := "launchctl print gui/501/com.github.fcanela.kwak"

	if err := m.Uninstall(); !errors.Is(err, service.ErrNotInstalled) {
		t.Fatalf("err = %v", err)
	}
	if st, _ := m.Status(); st != "not installed" {
		t.Fatalf("status = %q", st)
	}
	sameCalls(t, rec.Calls())

	rec.On(print, "Could not find service", errExit3)
	if err := m.Install("/Users/u/go/bin/kwak"); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(), print, servicetest.Line("launchctl", "bootstrap", "gui/501", plist))
	want, _ := service.Plist("/Users/u/go/bin/kwak", filepath.Join(home, "Library", "Logs", "kwak.log"))
	if got := readFile(t, plist); got != want {
		t.Fatalf("plist file:\n%s", got)
	}
	if fi, err := os.Stat(filepath.Join(home, "Library", "Logs")); err != nil || !fi.IsDir() {
		t.Fatalf("log dir: %v", err)
	}

	rec.On(print, "gui/501/com.github.fcanela.kwak = {\n\tactive count = 1\n\tstate = running\n}\n", nil)
	if err := m.Install("/Users/u/go/bin/kwak"); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(), print, "launchctl kickstart -k gui/501/com.github.fcanela.kwak")

	if err := m.Install("/Users/u/bin/kwak"); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(), print,
		"launchctl bootout gui/501/com.github.fcanela.kwak",
		servicetest.Line("launchctl", "bootstrap", "gui/501", plist))

	if st, err := m.Status(); err != nil || st != "running" {
		t.Fatalf("status = %q, %v", st, err)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(), print,
		"launchctl kickstart gui/501/com.github.fcanela.kwak",
		"launchctl kill SIGTERM gui/501/com.github.fcanela.kwak")

	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(), print, "launchctl bootout gui/501/com.github.fcanela.kwak")
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Fatalf("plist left: %v", err)
	}
}

func TestLaunchdInstallRetriesFailedBootout(t *testing.T) {
	home := t.TempDir()
	rec := servicetest.New()
	m := service.Launchd(rec, home, 501)
	plist := filepath.Join(home, "Library", "LaunchAgents", "com.github.fcanela.kwak.plist")
	print := "launchctl print gui/501/com.github.fcanela.kwak"
	bootout := "launchctl bootout gui/501/com.github.fcanela.kwak"
	bootstrap := servicetest.Line("launchctl", "bootstrap", "gui/501", plist)
	if err := m.Install("/Users/u/old/kwak"); err != nil {
		t.Fatal(err)
	}
	oldPlist := readFile(t, plist)
	rec.Calls()

	rec.On(bootout, "Boot-out failed: 5: Input/output error\n", errors.New("exit status 5"))
	if err := m.Install("/Users/u/new/kwak"); err == nil || !strings.Contains(err.Error(), "Boot-out failed") {
		t.Fatalf("err = %v", err)
	}
	sameCalls(t, rec.Calls(), print, bootout)
	if readFile(t, plist) != oldPlist {
		t.Fatal("the plist changed although the old agent is still loaded")
	}

	rec.On(bootout, "", nil)
	if err := m.Install("/Users/u/new/kwak"); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(), print, bootout, bootstrap)
	if !strings.Contains(readFile(t, plist), "<string>/Users/u/new/kwak</string>") {
		t.Fatal("the plist does not run the new binary")
	}
}

func TestLaunchdBootstrapRetriedAfterBootout(t *testing.T) {
	home := t.TempDir()
	rec := servicetest.New()
	m := service.Launchd(rec, home, 501)
	plist := filepath.Join(home, "Library", "LaunchAgents", "com.github.fcanela.kwak.plist")
	print := "launchctl print gui/501/com.github.fcanela.kwak"
	bootout := "launchctl bootout gui/501/com.github.fcanela.kwak"
	bootstrap := servicetest.Line("launchctl", "bootstrap", "gui/501", plist)
	if err := m.Install("/Users/u/old/kwak"); err != nil {
		t.Fatal(err)
	}
	rec.Calls()
	rec.Once(bootstrap, "Bootstrap failed: 5: Input/output error\n", errors.New("exit status 5"))
	rec.Once(bootstrap, "Bootstrap failed: 5: Input/output error\n", errors.New("exit status 5"))
	if err := m.Install("/Users/u/new/kwak"); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(), print, bootout, bootstrap, "sleep 500ms", bootstrap, "sleep 500ms", bootstrap)
}

func TestLaunchdUninstallNotLoaded(t *testing.T) {
	home := t.TempDir()
	rec := servicetest.New()
	m := service.Launchd(rec, home, 501)
	print := "launchctl print gui/501/com.github.fcanela.kwak"
	rec.On(print, "", errExit3)
	if err := m.Install("/Users/u/go/bin/kwak"); err != nil {
		t.Fatal(err)
	}
	rec.Calls()
	if st, _ := m.Status(); st != "not loaded" {
		t.Fatalf("status = %q", st)
	}
	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(), print, print)
}

func TestRunKey(t *testing.T) {
	rec := servicetest.New()
	m := service.RunKey(rec, rec, rec.Shutdown)
	exe := `C:\Users\u\go\bin\kwak.exe`
	value := `"C:\Users\u\go\bin\kwak.exe" listen --detach`
	if service.RunCommand(exe) != value {
		t.Fatalf("RunCommand = %s", service.RunCommand(exe))
	}

	if st, _ := m.Status(); st != "not installed" {
		t.Fatalf("status = %q", st)
	}
	if err := m.Start(); !errors.Is(err, service.ErrNotInstalled) {
		t.Fatalf("Start before install: %v", err)
	}
	rec.Calls()

	for range 2 {
		if err := m.Install(exe); err != nil {
			t.Fatal(err)
		}
		sameCalls(t, rec.Calls(),
			servicetest.Line("reg-set", "kwak", value),
			servicetest.Line(exe, "listen", "--detach"))
		if v, _ := rec.Value("kwak"); v != value {
			t.Fatalf("value = %s", v)
		}
	}
	if st, _ := m.Status(); st != "installed" {
		t.Fatalf("status = %q", st)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(),
		"reg-get kwak",
		"reg-get kwak",
		servicetest.Line(exe, "listen", "--detach"),
		"ipc-shutdown")

	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, rec.Calls(), "reg-delete kwak", "ipc-shutdown")
	if _, ok := rec.Value("kwak"); ok {
		t.Fatal("value left")
	}

	rec.ShutdownErr(service.ErrNotRunning)
	if err := m.Uninstall(); !errors.Is(err, service.ErrNotInstalled) {
		t.Fatalf("second Uninstall: %v", err)
	}
	if err := m.Install(exe); err != nil {
		t.Fatal(err)
	}
	if err := m.Uninstall(); err != nil {
		t.Fatalf("Uninstall with no listener running: %v", err)
	}

	for _, bad := range []string{`kwak.exe`, `bin\kwak.exe`, "/home/u/kwak"} {
		if err := m.Install(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := m.Install(`\\host\share\kwak.exe`); err != nil {
		t.Errorf("UNC path: %v", err)
	}
}

func TestRunKeyForeignValue(t *testing.T) {
	rec := servicetest.New()
	m := service.RunKey(rec, rec, rec.Shutdown)
	_ = rec.Set("kwak", `C:\other.exe`)
	if err := m.Start(); err == nil {
		t.Fatal("Start ran a value kwak did not write")
	}
}
