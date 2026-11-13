package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/listener"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

func ipcRow(t *testing.T, name string) wiretest.IPCCase {
	t.Helper()
	for _, c := range wiretest.IPC {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no IPC row %q", name)
	return wiretest.IPCCase{}
}

func TestStatus(t *testing.T) {
	r := newRig(t)
	r.ln.replies[wire.OpStatus] = []string{ipcRow(t, "status, syncing").Reply}
	code, out, errs := r.run(r.cli.Status, "")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{
		"Listener: syncing, last sync 2026-11-02 10:41 UTC (kwak 0.1.0)",
		"Pairing: fcanela on laptop, pairing 3f9a1c2e, server https://kwak.test",
		"Listener account: " + wiretest.StandingUser, // decision 42
		"Key valid until: 2027-01-31",
		"Owner settings: question timeout 15m\n",
		"Background resume on this machine: replies, mentions (Claude Code sessions only); 1 runs, 0 running",
		"Responder on this machine: on, folder /home/u/kwak-responder\n", // decision 46
		"Responder account: " + wiretest.ResponderUser,
		"Loop ceiling: no more background runs for thread $Qx1abc after 50; a message from a person in it starts the count again",
		"Hourly limit: 300 background runs started in the last hour; the next may start at 2026-11-02 11:20 UTC",
		"Checks: secrets scan on; outgoing command off; incoming command /opt/kwak/check-in; 1 waiting for the incoming check",
		"  2026-11-02 10:30 UTC refused a reply by " + wiretest.ClaudeAgent + " in thread $Qx1abc (not sent): it looks like it holds a secret (a GitHub token)",
		"  2026-11-02 10:35 UTC held a message from " + wiretest.OtherAgent + " in thread $Qx1abc for " + wiretest.ClaudeAgent + ": it asks for an action",
		"Caller checks: peer",
		"Sessions: 2",
		"  claude:" + wiretest.ClaudeSession + " in /home/u/src/app",
		"agent " + wiretest.ClaudeAgent + "; helpers " + wiretest.HelperAgent + "; tools connected; inbox 2",
		"started 2026-11-02 09:00 UTC, last hook 2026-11-02 10:40 UTC",
		"tools connected; inbox 0",
		"no hook seen for session codex:" + wiretest.CodexSession,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if got := r.ln.ops; len(got) != 1 || got[0] != wire.OpStatus {
		t.Errorf("ops %v", got)
	}
}

// With responder_own_agents on, the responder line says so (decision 46).
func TestStatusResponderOwnAgents(t *testing.T) {
	r := newRig(t)
	reply := strings.Replace(ipcRow(t, "status, syncing").Reply, `{"v":1,`, `{"v":1,"responder_own_agents":true,`, 1)
	r.ln.replies[wire.OpStatus] = []string{reply}
	_, out, _ := r.run(r.cli.Status, "")
	if want := "Responder on this machine: on, folder /home/u/kwak-responder, answers your own agents too\n"; !strings.Contains(out, want) {
		t.Errorf("status lacks %q:\n%s", want, out)
	}
}

func TestStatusStates(t *testing.T) {
	cases := map[string][]string{
		"status, unpaired":                       {"Listener: not paired", wiretest.TextUnpaired, "Sessions: none"},
		"status, waiting for the keyring":        {"Listener: waiting for the keyring", "waiting for the keyring (locked?)", "Caller checks: claimed", "Responder on this machine: off\n"},
		"status, connecting after a state reset": {"Listener: connecting", "Last error: registrar unreachable, HTTP 502", "State reset: messages since the last good state may be missed."},
	}
	for row, wants := range cases {
		r := newRig(t)
		r.ln.replies[wire.OpStatus] = []string{ipcRow(t, row).Reply}
		code, out, _ := r.run(r.cli.Status, "")
		if code != 0 {
			t.Errorf("%s: exit %d", row, code)
		}
		for _, w := range wants {
			if !strings.Contains(out, w) {
				t.Errorf("%s: lacks %q:\n%s", row, w, out)
			}
		}
		// No POST /standing worked yet, so no listener account is named.
		if strings.Contains(out, "Listener account") {
			t.Errorf("%s: names a listener account:\n%s", row, out)
		}
	}
}

func TestStatusListenerDown(t *testing.T) {
	r := newRig(t)
	code, out, errs := r.run(r.cli.Status, "")
	if code != 1 || !strings.Contains(errs, listenerDown) || !strings.Contains(out, "not paired") {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	r.paired()
	_, out, _ = r.run(r.cli.Status, "")
	if !strings.Contains(out, "paired as fcanela on laptop (pairing 77c0de11") {
		t.Errorf("paired machine: %s", out)
	}
	if code, _, _ := r.run(r.cli.Status, "", "x"); code != 2 {
		t.Errorf("extra argument: exit %d", code)
	}
}

func TestInbox(t *testing.T) {
	r := newRig(t)
	r.ln.replies[wire.OpInbox] = []string{ipcRow(t, "inbox").Reply}
	code, out, _ := r.run(r.cli.Inbox, "")
	want := "3 messages for claude:" + wiretest.ClaudeSession + " (" + wiretest.ClaudeAgent + ", ended), in /home/u/src/app\n" +
		"  resume: claude --resume " + wiretest.ClaudeSession + " (run it in /home/u/src/app)\n"
	if code != 0 || out != want {
		t.Errorf("exit %d\n got %q\nwant %q", code, out, want)
	}

	r.ln.replies[wire.OpInbox] = []string{ipcRow(t, "inbox, empty").Reply}
	if _, out, _ := r.run(r.cli.Inbox, ""); out != "No messages are waiting.\n" {
		t.Errorf("empty: %q", out)
	}
	delete(r.ln.replies, wire.OpInbox)
	if code, _, errs := r.run(r.cli.Inbox, ""); code != 1 || !strings.Contains(errs, listenerDown) {
		t.Errorf("down: exit %d %s", code, errs)
	}
}

// kwak config resume shows and sets this machine's background resume triggers (decision 44): a
// comma list stored as written in full (all is replies and mentions), off removes the key, and
// open is refused with a pointer to the responder (decision 46). Nothing goes to the registrar or the listener.
func TestConfigResume(t *testing.T) {
	r := newRig(t)
	if code, _, errs := r.run(r.cli.Config, "", "resume", "replies"); code != 1 || !strings.Contains(errs, "not paired") {
		t.Errorf("unpaired: exit %d: %s", code, errs)
	}
	r.paired()
	code, out, _ := r.run(r.cli.Config, "", "resume")
	if code != 0 || !strings.Contains(out, "Background resume on this machine: replies.\n") || !strings.Contains(out, "mentions: ") {
		t.Errorf("show: exit %d:\n%s", code, out)
	}
	resume := func() string {
		on, ok := r.config()["resume"].([]any)
		if !ok {
			return "<none>"
		}
		return fmt.Sprint(on...)
	}
	for _, c := range []struct{ arg, want string }{
		{"mentions,replies", "repliesmentions"},
		{"mentions", "mentions"},
		{"all", "repliesmentions"},
		{" Replies , replies", "replies"},
		{"off", "<none>"},
	} {
		code, out, errs := r.run(r.cli.Config, "", "resume", c.arg)
		if code != 0 || !strings.Contains(out, "Background resume on this machine is now ") || !strings.Contains(out, "no restart is needed") {
			t.Errorf("%q: exit %d: %s %s", c.arg, code, out, errs)
		}
		if got := resume(); got != c.want {
			t.Errorf("%q: resume %s, want %s", c.arg, got, c.want)
		}
	}
	for _, bad := range []string{"open", "replies,open", "off,replies", "sometimes", "replies,"} {
		code, _, errs := r.run(r.cli.Config, "", "resume", bad)
		if code != 2 || resume() != "<none>" {
			t.Errorf("%q: exit %d, resume %s", bad, code, resume())
		}
		if strings.Contains(bad, "open") && !strings.Contains(errs, "answered by the responder") {
			t.Errorf("%q: %s", bad, errs)
		}
	}
	if code, _, _ := r.run(r.cli.Config, "", "resume", "replies", "mentions"); code != 2 {
		t.Errorf("two arguments: exit %d", code)
	}
	if len(r.reg.log()) != 0 || len(r.ln.ops) != 0 || r.config()["future_field"] == nil {
		t.Error("config resume called out or lost a field")
	}
	if code, _, _ := r.run(r.cli.Config, ""); code != 2 {
		t.Errorf("no setting: exit %d", code)
	}
	if code, _, _ := r.run(r.cli.Config, "", "colour"); code != 2 {
		t.Errorf("unknown setting: exit %d", code)
	}
}

// kwak config responder on|off (decision 46) sets "responder" in config.json, keeps unknown
// fields, and on makes the responder folder next to config.json with the default AGENTS.md,
// never writing over the user's. Nothing goes to the registrar or the listener.
func TestConfigResponder(t *testing.T) {
	r := newRig(t)
	if code, _, errs := r.run(r.cli.Config, "", "responder", "on"); code != 1 || !strings.Contains(errs, "not paired") {
		t.Errorf("unpaired: exit %d: %s", code, errs)
	}
	r.paired()
	dir, _ := r.cli.env.ConfigDir()
	folder := filepath.Join(dir, "responder")
	if code, out, _ := r.run(r.cli.Config, "", "responder"); code != 0 || !strings.Contains(out, "Responder on this machine: off. Its folder: "+folder) {
		t.Errorf("show: exit %d:\n%s", code, out)
	}
	code, out, errs := r.run(r.cli.Config, "", "responder", "on")
	if code != 0 || r.config()["responder"] != true || !strings.Contains(out, "is now on") || !strings.Contains(out, folder) ||
		strings.Contains(out, "run kwak config resume-exe") {
		t.Fatalf("on: exit %d: %s %s", code, out, errs)
	}
	if b, err := os.ReadFile(filepath.Join(folder, "AGENTS.md")); err != nil || string(b) != listener.ResponderAgents {
		t.Fatalf("AGENTS.md %q %v", b, err)
	}
	os.WriteFile(filepath.Join(folder, "AGENTS.md"), []byte("mine"), 0o600)
	r.run(r.cli.Config, "", "responder", "on")
	if b, _ := os.ReadFile(filepath.Join(folder, "AGENTS.md")); string(b) != "mine" {
		t.Fatalf("AGENTS.md written over: %q", b)
	}
	if code, out, _ := r.run(r.cli.Config, "", "responder", "off"); code != 0 || r.config()["responder"] != nil || !strings.Contains(out, "is now off") {
		t.Errorf("off: exit %d %s, config %v", code, out, r.config()["responder"])
	}
	for _, bad := range [][]string{{"responder", "maybe"}, {"responder", "on", "now"}} {
		if code, _, _ := r.run(r.cli.Config, "", bad...); code != 2 {
			t.Errorf("%q: exit %d", bad, code)
		}
	}
	if len(r.reg.log()) != 0 || len(r.ln.ops) != 0 || r.config()["future_field"] == nil {
		t.Error("config responder called out or lost a field")
	}
}

func TestConfigResumeExe(t *testing.T) {
	r := newRig(t)
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := r.run(r.cli.Config, "", "resume-exe"); code != 1 || !strings.Contains(errs, "not paired") {
		t.Errorf("unpaired: exit %d: %s", code, errs)
	}
	r.paired()
	var looked []string
	r.cli.env.LookPath = func(name string) (string, error) { looked = append(looked, name); return bin, nil }
	code, out, errs := r.run(r.cli.Config, "", "resume-exe")
	if code != 0 || !strings.Contains(out, "Recorded "+bin) {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	exe := r.config()["resume_exe"].(map[string]any)
	if exe["claude"] != bin || strings.Join(looked, ",") != "claude" {
		t.Errorf("resume_exe %v, looked %v", exe, looked)
	}
	if r.config()["future_field"] == nil {
		t.Error("unknown field lost")
	}

	code, _, _ = r.run(r.cli.Config, "", "resume-exe", "--harness", "codex", bin)
	if code != 0 || r.config()["resume_exe"].(map[string]any)["codex"] != bin {
		t.Errorf("codex: exit %d", code)
	}
	if code, _, _ := r.run(r.cli.Config, "", "resume-exe", filepath.Dir(bin)); code != 1 {
		t.Error("a directory was accepted")
	}
	plain := filepath.Join(t.TempDir(), "notexec")
	os.WriteFile(plain, nil, 0o644)
	if code, _, _ := r.run(r.cli.Config, "", "resume-exe", plain); code != 1 {
		t.Error("a file without the exec bit was accepted")
	}
	if code, _, _ := r.run(r.cli.Config, "", "resume-exe", "--harness", "gemini"); code != 1 {
		t.Error("unknown harness accepted")
	}
}

// The check commands (spec 5.16): absolute paths only, kept next to unknown fields, and "off"
// turns one off again.
func TestConfigCheck(t *testing.T) {
	r := newRig(t)
	bin := filepath.Join(t.TempDir(), "check.sh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := r.run(r.cli.Config, "", "check-outgoing", bin); code != 1 || !strings.Contains(errs, "not paired") {
		t.Errorf("unpaired: exit %d: %s", code, errs)
	}
	r.paired()
	for _, which := range []string{"check-outgoing", "check-incoming"} {
		key := strings.Replace(which, "-", "_", 1)
		code, out, errs := r.run(r.cli.Config, "", which, bin)
		if code != 0 || !strings.Contains(out, "Recorded "+bin) || r.config()[key] != bin {
			t.Fatalf("%s: exit %d: %s %s, config %v", which, code, out, errs, r.config()[key])
		}
		rel := filepath.Base(bin)
		if code, _, errs := r.run(r.cli.Config, "", which, rel); code != 1 || !strings.Contains(errs, "not an absolute path") {
			t.Errorf("%s: a relative path was accepted: exit %d %s", which, code, errs)
		}
		if code, _, _ := r.run(r.cli.Config, "", which, filepath.Dir(bin)); code != 1 {
			t.Errorf("%s: a directory was accepted", which)
		}
		if code, _, _ := r.run(r.cli.Config, "", which, filepath.Join(t.TempDir(), "missing")); code != 1 {
			t.Errorf("%s: a missing file was accepted", which)
		}
		if r.config()[key] != bin {
			t.Errorf("%s: a refused path changed the config: %v", which, r.config()[key])
		}
		if code, _, _ := r.run(r.cli.Config, "", which); code != 2 {
			t.Errorf("%s: no path: exit %d", which, code)
		}
	}
	if r.config()["future_field"] == nil || r.config()["resume_exe"] == nil {
		t.Error("other fields lost")
	}
	code, out, _ := r.run(r.cli.Config, "", "check-outgoing", "off")
	if _, set := r.config()["check_outgoing"]; code != 0 || set || !strings.Contains(out, "is off") {
		t.Errorf("off: exit %d %q, config %v", code, out, r.config())
	}
	if r.config()["check_incoming"] != bin {
		t.Error("turning the outgoing check off changed the incoming one")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := loadConfig(dir, false); err != ErrNotPaired {
		t.Errorf("missing: %v", err)
	}
	write(`{"version":2,"server":"https://kwak.test"}`)
	if _, err := loadConfig(dir, false); err == nil || !strings.Contains(err.Error(), "kwak is older than its config; go install a newer kwak") {
		t.Errorf("newer: %v", err)
	}
	write(`{"version":1,"server":"http://kwak.test"}`)
	if _, err := loadConfig(dir, false); err == nil {
		t.Error("http server accepted")
	}
	if _, err := loadConfig(dir, true); err != nil {
		t.Errorf("http with the test option: %v", err)
	}
	write(`{nope`)
	if _, err := loadConfig(dir, false); err == nil {
		t.Error("bad JSON accepted")
	}
	write(`{"server":"https://kwak.test","owner":"fcanela","x":[1]}`)
	c, err := loadConfig(dir, false)
	if err != nil || c.Version != 1 || c.ResumeMax != DefaultResumeMax {
		t.Fatalf("old file: %+v %v", c, err)
	}
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, ConfigFile))
	if !strings.Contains(string(data), `"x": [`) || !strings.Contains(string(data), `"version": 1`) {
		t.Errorf("saved %s", data)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left: %v", entries)
	}
}

func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "kwcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestSocketCaller(t *testing.T) {
	path := filepath.Join(shortDir(t), "s", "kwak.sock")
	ln, err := ipc.Listen(path, ipc.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := ipc.NewLineReader(c).Next()
		got <- string(line)
		c.Write([]byte(ipcRow(t, "inbox, empty").Reply + "\n"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var reply wire.InboxResponse
	if err := SocketCaller(path)(ctx, wire.Head{V: 1, Op: wire.OpInbox}, &reply); err != nil {
		t.Fatal(err)
	}
	if line := <-got; line != `{"v":1,"op":"inbox"}` {
		t.Errorf("sent %s", line)
	}
	if !reply.OK || reply.Sessions == nil {
		t.Errorf("reply %+v", reply)
	}

	ln.Close()
	if err := SocketCaller(path)(ctx, wire.Head{V: 1, Op: wire.OpStatus}, &reply); err == nil {
		t.Error("no error with the listener gone")
	}
}

func TestRunnable(t *testing.T) {
	dir := t.TempDir()
	file := func(name string, mode os.FileMode) (os.FileInfo, string) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, mode); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi, p
	}
	for _, c := range []struct {
		name    string
		mode    os.FileMode
		windows bool
		want    bool
	}{
		{"claude", 0o755, false, true},
		{"notes.txt", 0o644, false, false},
		{"claude.exe", 0o644, true, true},
		{"claude.CMD", 0o644, true, true},
		{"claude.bat", 0o644, true, true},
		{"notes.txt", 0o644, true, false},
		{"claude", 0o755, true, false},
	} {
		fi, p := file(c.name, c.mode)
		if got := runnable(fi, p, c.windows); got != c.want {
			t.Errorf("%s windows=%v: %v, want %v", c.name, c.windows, got, c.want)
		}
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runnable(fi, dir+".exe", true) || runnable(fi, dir, false) {
		t.Error("a directory is runnable")
	}
}

// resume-per-hour (decision 38, spec 5.11): a whole number of 1 or more, kept next to the other
// fields; a refused value changes nothing.
func TestConfigResumePerHour(t *testing.T) {
	r := newRig(t)
	if code, _, errs := r.run(r.cli.Config, "", "resume-per-hour", "50"); code != 1 || !strings.Contains(errs, "not paired") {
		t.Errorf("unpaired: exit %d: %s", code, errs)
	}
	r.paired()
	code, out, errs := r.run(r.cli.Config, "", "resume-per-hour", "50")
	if code != 0 || !strings.Contains(out, "Recorded 50 background resume runs per hour") || r.config()["resume_per_hour"] != 50.0 {
		t.Fatalf("exit %d: %s %s, config %v", code, out, errs, r.config()["resume_per_hour"])
	}
	if r.config()["future_field"] == nil || r.config()["thread_ceiling"] != 7.0 || r.config()["resume_max"] != 3.0 {
		t.Error("other fields lost")
	}
	for _, bad := range [][]string{{"0"}, {"-3"}, {"many"}, {"1.5"}, {}, {"5", "6"}} {
		code, _, errs := r.run(r.cli.Config, "", append([]string{"resume-per-hour"}, bad...)...)
		if code != 2 || !strings.Contains(errs, "usage: kwak config resume-per-hour <n>") {
			t.Errorf("%q: exit %d: %s", bad, code, errs)
		}
	}
	if r.config()["resume_per_hour"] != 50.0 {
		t.Errorf("a refused value changed the config: %v", r.config()["resume_per_hour"])
	}
	if code, _, _ := r.run(r.cli.Config, "", "resume-per-hour", "1"); code != 0 || r.config()["resume_per_hour"] != 1.0 {
		t.Errorf("1: exit %d, config %v", code, r.config()["resume_per_hour"])
	}
}

// kwak config autostart shows and sets listener_autostart (decision 46); on is the default.
func TestConfigAutostart(t *testing.T) {
	r := newRig(t)
	r.paired()
	if code, out, _ := r.run(r.cli.Config, "", "autostart"); code != 0 || !strings.Contains(out, "Listener autostart on this machine: on.") {
		t.Errorf("show: exit %d:\n%s", code, out)
	}
	if code, out, _ := r.run(r.cli.Config, "", "autostart", "off"); code != 0 || r.config()["listener_autostart"] != false ||
		!strings.Contains(out, "is now off") {
		t.Errorf("off: exit %d %s, config %v", code, out, r.config()["listener_autostart"])
	}
	if code, out, _ := r.run(r.cli.Config, "", "autostart"); code != 0 || !strings.Contains(out, "on this machine: off.") {
		t.Errorf("show off: exit %d:\n%s", code, out)
	}
	if code, _, _ := r.run(r.cli.Config, "", "autostart", "on"); code != 0 || r.config()["listener_autostart"] != true {
		t.Errorf("on: exit %d, config %v", code, r.config()["listener_autostart"])
	}
	if r.config()["future_field"] == nil {
		t.Error("other fields lost")
	}
	for _, bad := range [][]string{{"autostart", "maybe"}, {"autostart", "on", "now"}} {
		if code, _, _ := r.run(r.cli.Config, "", bad...); code != 2 {
			t.Errorf("%q: exit %d", bad, code)
		}
	}
}
