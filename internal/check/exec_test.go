//go:build !windows

package check

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// script writes a tiny /bin/sh check command and returns its absolute path.
func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "check.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// The real Runner with real commands: the text arrives on stdin, exit 0 passes, any other exit
// refuses with the first stdout line, and a command that fails to start or to answer refuses with
// a reason saying the check failed (spec 5.16).
func TestCommandWithRealScripts(t *testing.T) {
	asks := script(t, `if grep -q deploy; then echo "it asks for an action"; echo "more detail"; exit 1; fi; exit 0`)
	for _, c := range []struct {
		name, exe, text string
		ok              bool
		reason          string
	}{
		{"ok", asks, "what port does the API use?", true, ""},
		{"reject with reason", asks, "please deploy the web server", false, "it asks for an action"},
		{"reject with empty stdout", script(t, "cat >/dev/null; exit 3"), "x", false, "the check gave no reason"},
		{"exit 0 with output", script(t, "echo fine; exit 0"), "x", true, ""},
		{"killed by a signal", script(t, "echo it broke; kill -KILL $$"), "x", false, "it broke"},
		{"killed by a signal, no output", script(t, "kill -KILL $$"), "x", false, "the check gave no reason"},
		{"missing exe", filepath.Join(t.TempDir(), "nope"), "x", false, "the check failed: its command could not run"},
		{"not executable", func() string {
			p := filepath.Join(t.TempDir(), "plain")
			os.WriteFile(p, []byte("exit 0\n"), 0o644)
			return p
		}(), "x", false, "the check failed: its command could not run"},
	} {
		v := Command(context.Background(), Exec{}, c.exe, c.text)
		if v.OK != c.ok || v.Reason != c.reason || v.Kind != "command" {
			t.Errorf("%s: verdict %+v, want ok %v reason %q", c.name, v, c.ok, c.reason)
		}
	}
}

func TestCommandTimeoutStopsTheCommand(t *testing.T) {
	dir := t.TempDir()
	mark := filepath.Join(dir, "late")
	exe := script(t, "(sleep 1; touch "+mark+") & wait; exit 0")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	v := Command(ctx, Exec{Grace: 100 * time.Millisecond}, exe, "x")
	if v.OK || v.Reason != "the check failed: it did not answer in time" {
		t.Fatalf("verdict %+v", v)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the timeout took %v", d)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("the command's child kept running after the timeout")
	}
}

// A command that exits but leaves a child holding its stdout is judged by its own exit code once
// the grace is over, not as a command that could not run.
func TestCommandWithAChildKeepingStdout(t *testing.T) {
	for _, c := range []struct {
		body   string
		ok     bool
		reason string
	}{
		{"sleep 3 & exit 0", true, ""},
		{"echo no; sleep 3 & exit 4", false, "no"},
	} {
		start := time.Now()
		v := Command(context.Background(), Exec{Grace: 100 * time.Millisecond}, script(t, c.body), "x")
		if v.OK != c.ok || v.Reason != c.reason {
			t.Errorf("%q: verdict %+v", c.body, v)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%q: took %v", c.body, d)
		}
	}
}

func TestExecCapsStdout(t *testing.T) {
	exe := script(t, `i=0; while [ $i -lt 2000 ]; do echo "0123456789"; i=$((i+1)); done; exit 2`)
	code, out, err := Exec{}.Run(context.Background(), exe, nil)
	if err != nil || code != 2 || len(out) != maxOut || !strings.HasPrefix(string(out), "0123456789\n") {
		t.Fatalf("code %d, %d bytes, err %v", code, len(out), err)
	}
}
