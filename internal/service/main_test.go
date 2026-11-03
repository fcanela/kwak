package service_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/fcanela/kwak/internal/service"
	"github.com/fcanela/kwak/internal/service/servicetest"
)

func commands(t *testing.T, exe string, exeErr error) (service.Commands, *servicetest.Recorder) {
	rec := servicetest.New()
	m := service.Systemd(rec, t.TempDir())
	return service.Commands{Manager: m, Exe: func() (string, error) { return exe, exeErr }}, rec
}

func TestInstallMain(t *testing.T) {
	c, rec := commands(t, "/home/u/go/bin/kwak", nil)
	rec.On("systemctl --user is-active kwak.service", "active\n", nil)
	var out, errb bytes.Buffer
	if code := c.Install(nil, nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	want := "kwak install: the listener runs as a user service from /home/u/go/bin/kwak.\nService status: active.\n"
	if out.String() != want || errb.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", out.String(), errb.String())
	}

	out.Reset()
	if code := c.Install([]string{"extra"}, nil, &out, &errb); code != 2 || out.Len() != 0 {
		t.Fatalf("extra arg: exit %d", code)
	}

	c, _ = commands(t, "", errors.New("boom"))
	errb.Reset()
	if code := c.Install(nil, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "cannot find the kwak binary: boom") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}

	c, rec = commands(t, "/home/u/go/bin/kwak", nil)
	rec.On("systemctl --user enable --now kwak.service", "no user manager", errors.New("exit status 1"))
	errb.Reset()
	if code := c.Install(nil, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no user manager") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

func TestUninstallMain(t *testing.T) {
	c, rec := commands(t, "/home/u/go/bin/kwak", nil)
	var out, errb bytes.Buffer
	if code := c.Uninstall(nil, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "no kwak user service is installed") {
		t.Fatalf("exit %d, stdout %q", code, out.String())
	}
	if code := c.Install(nil, nil, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	if code := c.Uninstall(nil, nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	want := "kwak uninstall: the listener is stopped and its user service is removed.\n" +
		"The pairing and the keyring item stay. Run kwak unpair to remove them.\n"
	if out.String() != want {
		t.Fatalf("stdout %q", out.String())
	}
	if code := c.Install(nil, nil, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	rec.On("systemctl --user disable --now kwak.service", "", errors.New("exit status 1"))
	errb.Reset()
	if code := c.Uninstall(nil, nil, &out, &errb); code != 1 || !strings.HasPrefix(errb.String(), "kwak uninstall: ") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}
