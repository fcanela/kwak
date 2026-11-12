package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/secrets/secretstest"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

func TestScanTOML(t *testing.T) {
	src := `model = "x"
notes = """
[mcp_servers.kwak]
"""
list = [
  "[mcp_servers.kwak]",
]
[hooks.state."/h/hooks.json:session_start:0:0"]
trusted_hash = "sha256:aa"
[hooks.state.'/h/hooks.json:stop:0:0']
enabled = true
[hooks.state]
"/h/hooks.json:user_prompt_submit:0:0" = { trusted_hash = "sha256:bb" }
"/h/hooks.json:post_tool_use:1:0" = { enabled = false }
"/h/hooks.json:session_end:0:0".trusted_hash = 'sha256:cc'
"C:\\Users\\u\\.codex\\hooks.json:stop:0:0" = {trusted_hash="sha256:dd"}
[hooks]
state."/h/hooks.json:subagent_stop:0:0".trusted_hash = "sha256:ee"
`
	e := scanTOML(src)
	if hasKwakServer(e) {
		t.Fatal("found mcp_servers.kwak inside a string or an array")
	}
	got := trustedHooks(e)
	want := map[string]bool{
		"/h/hooks.json:session_start:0:0":       true,
		"/h/hooks.json:user_prompt_submit:0:0":  true,
		"/h/hooks.json:session_end:0:0":         true,
		`C:\Users\u\.codex\hooks.json:stop:0:0`: true,
		"/h/hooks.json:subagent_stop:0:0":       true,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("trusted %v\nwant %v", got, want)
	}
	if !hasKwakServer(scanTOML(configBlock(exe))) {
		t.Fatal("kwak's own block not found")
	}
}

type fixture struct {
	dir    string
	home   string
	cfgDir string
	keys   *secretstest.Memory
	status string
	callOK bool
	env    map[string]string
	path   map[string]string
	stat   map[string]bool
	goos   string
	dev    bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		dir:    dir,
		home:   filepath.Join(dir, "home"),
		cfgDir: filepath.Join(dir, "config"),
		keys:   secretstest.Mem(),
		callOK: true,
		env:    map[string]string{},
		path:   map[string]string{"kwak": exe},
		stat:   map[string]bool{},
		goos:   "linux",
	}
	f.status = wiretest.IPC[len(wiretest.IPC)-1].Reply
	for _, row := range wiretest.IPC {
		if row.Name == "status, syncing" {
			f.status = row.Reply
		}
	}
	write(t, filepath.Join(f.cfgDir, "config.json"), `{"version":1,"server":"https://kwak.test","server_name":"kwak.test","owner":"fcanela","machine":"laptop","pairing_id":"3f9a1c2e"}`)
	f.keys.Set(secrets.OwnerKeyName("kwak.test"), wiretest.OwnerKey)

	claude := filepath.Join(f.home, ".claude")
	write(t, filepath.Join(claude, "plugins", "installed_plugins.json"),
		`{"version":2,"plugins":{"kwak@kwak":[{"scope":"user","installPath":"/x/kwak/0.1.0","version":"0.1.0"}]}}`)
	write(t, filepath.Join(claude, "settings.json"),
		`{"enabledPlugins":{"kwak@kwak":true},"permissions":{"allow":["Bash(ls)","mcp__plugin_kwak_kwak__*"]}}`)

	codex := filepath.Join(f.home, ".codex")
	if code, _, errOut := runSetup(t, env{codexHome: codex, exe: exe, goos: "linux"}, "codex", "--write"); code != 0 {
		t.Fatal(errOut)
	}
	f.trustAll(t, codex)
	return f
}

// trustAll adds hooks.state entries where Codex (toml_edit) puts them: after the last table,
// before the comment that ends the file, which is kwak's end marker.
func (f *fixture) trustAll(t *testing.T, codex string) {
	cfg := filepath.Join(codex, "config.toml")
	write(t, cfg, trusted(t, read(t, cfg), filepath.Join(codex, "hooks.json")))
}

func trusted(t *testing.T, cfg, hooksPath string) string {
	t.Helper()
	keys := kwakHookKeys([]byte(read(t, hooksPath)), hooksPath)
	var b strings.Builder
	for _, e := range codexEvents {
		for _, k := range keys[e.Name] {
			fmt.Fprintf(&b, "\n[hooks.state.%s]\ntrusted_hash = \"sha256:%x\"\n", tomlString(k), sha256.Sum256([]byte(k)))
		}
	}
	i := strings.LastIndex(cfg, markerEnd+"\n")
	if i < 0 || i+len(markerEnd)+1 != len(cfg) {
		t.Fatalf("config.toml does not end with kwak's end marker:\n%s", cfg)
	}
	return cfg[:i] + b.String() + cfg[i:]
}

func (f *fixture) deps() DoctorDeps {
	return DoctorDeps{
		Preflight: func() error { return nil },
		Secrets:   f.keys,
		Call: func(_ context.Context, req, reply any) error {
			if h, ok := req.(wire.Head); !ok || h.Op != wire.OpStatus || h.V != 1 {
				return fmt.Errorf("unexpected request %#v", req)
			}
			if !f.callOK {
				return errors.New("dial unix: no such file")
			}
			return json.Unmarshal([]byte(f.status), reply)
		},
		Getenv: func(k string) string { return f.env[k] },
		Home:   f.home,
		GOOS:   f.goos,
		Exe:    exe,
		LookPath: func(n string) (string, error) {
			p, ok := f.path[n]
			if !ok {
				return "", errors.New("not found")
			}
			return p, nil
		},
		Stat: func(p string) (fs.FileInfo, error) {
			if f.stat[p] {
				return os.Stat(f.dir)
			}
			return os.Stat(p)
		},
		ConfigDir:  func() (string, error) { return f.cfgDir, nil },
		SocketPath: func() (string, error) { return "/run/user/1000/kwak/kwak.sock", nil },
		Dev:        f.dev,
	}
}

func (f *fixture) doctor(t *testing.T) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Doctor(f.deps())(nil, nil, &out, &errOut)
	if errOut.Len() > 0 {
		t.Fatalf("stderr %q", errOut.String())
	}
	return code, out.String()
}

func snapshot(t *testing.T, root string) string {
	var b strings.Builder
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, _ := d.Info()
		fmt.Fprintf(&b, "%s %v %d %v\n", p, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	return b.String()
}

func TestDoctorAllGood(t *testing.T) {
	f := newFixture(t)
	before := snapshot(t, f.dir)
	code, out := f.doctor(t)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, s := range []string{"ok    bus:", "ok    config: paired as fcanela on laptop", "ok    keyring: the owner key is there (not shown)",
		"ok    socket: /run/user/1000/kwak/kwak.sock, listener 0.1.0, syncing", "ok    PATH: " + exe,
		"ok    account: the listener syncs as " + wiretest.StandingUser + ", this pairing's own listener account",
		"ok    claude: the kwak plugin 0.1.0 is installed", "ok    codex: MCP server set"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "warn") || strings.Contains(out, "FAIL") || strings.Contains(out, wiretest.OwnerKey) {
		t.Errorf("output:\n%s", out)
	}
	if snapshot(t, f.dir) != before {
		t.Fatal("doctor changed a file")
	}
}

func TestDoctorProblems(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *fixture)
		code  int
		want  []string
		not   []string
	}{
		{"no bus", func(t *testing.T, f *fixture) {}, 1,
			[]string{"FAIL  bus: no session bus", "skip  keyring: not checked without a session bus"}, nil},
		{"not paired", func(t *testing.T, f *fixture) { os.Remove(filepath.Join(f.cfgDir, "config.json")) }, 1,
			[]string{"FAIL  config: " + TextNotPaired, "skip  keyring"}, nil},
		{"key missing", func(t *testing.T, f *fixture) { f.keys.Delete(secrets.OwnerKeyName("kwak.test")) }, 1,
			[]string{"FAIL  keyring: " + TextKeyMissing}, nil},
		{"own keyring item", func(t *testing.T, f *fixture) {
			write(t, filepath.Join(f.cfgDir, "config.json"), `{"version":1,"server":"https://kwak.test","server_name":"kwak.test",`+
				`"owner":"fcanela","machine":"laptop","pairing_id":"3f9a1c2e","keyring_user":"kwak.test/3f9a1c2e/owner-key","test":true}`)
			f.keys.Delete(secrets.OwnerKeyName("kwak.test"))
			f.keys.Set(secrets.PairingKeyName("kwak.test", "3f9a1c2e"), wiretest.OwnerKey)
		}, 0, []string{"ok    config: paired as fcanela on laptop, server https://kwak.test (a test pairing)",
			"ok    keyring: the owner key is there"}, nil},
		{"own keyring item missing", func(t *testing.T, f *fixture) {
			// The shared item holds another pairing's key; it does not count.
			write(t, filepath.Join(f.cfgDir, "config.json"), `{"version":1,"server":"https://kwak.test","server_name":"kwak.test",`+
				`"owner":"fcanela","machine":"laptop","pairing_id":"3f9a1c2e","keyring_user":"kwak.test/3f9a1c2e/owner-key"}`)
		}, 1, []string{"FAIL  keyring: " + TextKeyMissing}, nil},
		{"dev build", func(t *testing.T, f *fixture) { f.dev = true }, 0,
			[]string{"warn  build: a dev build (make dev). It uses its own test folder, config "}, nil},
		{"keyring locked", func(t *testing.T, f *fixture) { f.keys.Fail(secrets.ErrTimeout) }, 1,
			[]string{"FAIL  keyring: " + TextKeyringLocked}, nil},
		{"listener down", func(t *testing.T, f *fixture) { f.callOK = false }, 1,
			[]string{"FAIL  socket: " + TextNotRunning}, nil},
		{"listener older", func(t *testing.T, f *fixture) {
			f.status = `{"v":1,"ok":true,"kwak":"0.0.9","state":"syncing","caller_checks":"peer","resume_runs":0,"resume_active":0,"sessions":[]}`
		}, 0, []string{"warn  listener: " + TextListenerOlder}, nil},
		{"listener unpaired", func(t *testing.T, f *fixture) {
			for _, row := range wiretest.IPC {
				if row.Name == "status, waiting for the keyring" {
					f.status = row.Reply
				}
			}
		}, 0, []string{"warn  listener: waiting for the keyring (locked?)", "caller checks: claimed"}, []string{"account:"}},
		{"env overrides", func(t *testing.T, f *fixture) {
			f.env["KWAK_SOCKET"] = "/tmp/x.sock"
			f.env["KWAK_STATE_DIR"] = "/tmp/s"
		}, 0,
			[]string{"warn  env: KWAK_SOCKET is set to /tmp/x.sock", "warn  env: KWAK_STATE_DIR"}, nil},
		{"not on PATH", func(t *testing.T, f *fixture) { delete(f.path, "kwak") }, 1,
			[]string{"FAIL  PATH: kwak is not on PATH", "$(go env GOPATH)/bin"}, nil},
		{"another kwak on PATH", func(t *testing.T, f *fixture) { f.path["kwak"] = "/usr/bin/kwak" }, 0,
			[]string{"warn  PATH: kwak on PATH is /usr/bin/kwak, but this kwak is " + exe}, nil},
		{"plugin missing", func(t *testing.T, f *fixture) {
			os.Remove(filepath.Join(f.home, ".claude", "plugins", "installed_plugins.json"))
		}, 0, []string{"warn  claude: the kwak plugin is not installed"}, nil},
		{"plugin old, disabled, no allow rule", func(t *testing.T, f *fixture) {
			write(t, filepath.Join(f.home, ".claude", "plugins", "installed_plugins.json"), `{"plugins":{"kwak@kwak":[{"version":"0.0.1"}]}}`)
			write(t, filepath.Join(f.home, ".claude", "settings.json"), `{"enabledPlugins":{"kwak@kwak":false}}`)
		}, 0, []string{"is version 0.0.1, but kwak is 0.1.0", "is disabled", "asks before each kwak tool call"}, nil},
		{"no claude at all", func(t *testing.T, f *fixture) { os.RemoveAll(filepath.Join(f.home, ".claude")) }, 0,
			[]string{"skip  claude: no Claude Code config"}, nil},
		{"CLAUDE_CONFIG_DIR", func(t *testing.T, f *fixture) { f.env["CLAUDE_CONFIG_DIR"] = filepath.Join(f.dir, "elsewhere") }, 0,
			[]string{"skip  claude: no Claude Code config at ", "elsewhere"}, nil},
		{"codex untrusted", func(t *testing.T, f *fixture) {
			codex := filepath.Join(f.home, ".codex")
			os.Remove(filepath.Join(codex, "config.toml"))
			runSetup(t, env{codexHome: codex, exe: exe, goos: "linux"}, "codex", "--write")
		}, 0, []string{"warn  codex: " + TextCodexUntrust + " (no hooks.state entry for SessionStart, UserPromptSubmit, PostToolUse, Stop, SubagentStop, SessionEnd)"}, nil},
		{"codex hooks out of date", func(t *testing.T, f *fixture) {
			codex := filepath.Join(f.home, ".codex")
			runSetup(t, env{codexHome: codex, exe: "/old/kwak", goos: "linux"}, "codex", "--write")
		}, 0, []string{"differ from this kwak (SessionStart, UserPromptSubmit, PostToolUse, Stop, SubagentStop, SessionEnd)"}, []string{"no hooks.state entry"}},
		{"codex not set up", func(t *testing.T, f *fixture) {
			codex := filepath.Join(f.home, ".codex")
			os.Remove(filepath.Join(codex, "config.toml"))
			os.Remove(filepath.Join(codex, "hooks.json"))
		}, 0, []string{"has no [mcp_servers.kwak]", "has no kwak hooks"}, nil},
		{"CODEX_HOME", func(t *testing.T, f *fixture) { f.env[EnvCodexHome] = filepath.Join(f.dir, "nocodex") }, 0,
			[]string{"skip  codex: no Codex config at ", "nocodex"}, []string{"codex shell"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			c.setup(t, f)
			d := f.deps()
			if c.name == "no bus" {
				d.Preflight = func() error { return errors.New("no session bus; run under your desktop session or systemd --user") }
			}
			var out bytes.Buffer
			code := Doctor(d)(nil, nil, &out, &bytes.Buffer{})
			if code != c.code {
				t.Errorf("exit %d, want %d", code, c.code)
			}
			for _, s := range c.want {
				if !strings.Contains(out.String(), s) {
					t.Errorf("output lacks %q", s)
				}
			}
			for _, s := range c.not {
				if strings.Contains(out.String(), s) {
					t.Errorf("output has %q", s)
				}
			}
			if t.Failed() {
				t.Log(out.String())
			}
		})
	}
}

func TestDoctorCanonicalCodexHome(t *testing.T) {
	for _, rel := range []bool{false, true} {
		f := newFixture(t)
		dir, err := filepath.EvalSymlinks(f.dir)
		if err != nil {
			t.Fatal(err)
		}
		real := filepath.Join(dir, "realcodex")
		if code, _, errOut := runSetup(t, env{codexHome: real, exe: exe, goos: "linux"}, "codex", "--write"); code != 0 {
			t.Fatal(errOut)
		}
		f.trustAll(t, real)
		if err := os.Symlink(real, filepath.Join(f.dir, "linkcodex")); err != nil {
			t.Skip("no symlinks here:", err)
		}
		f.env[EnvCodexHome] = filepath.Join(f.dir, "linkcodex")
		if rel {
			t.Chdir(f.dir)
			f.env[EnvCodexHome] = "linkcodex"
		}
		code, out := f.doctor(t)
		if code != 0 || !strings.Contains(out, "ok    codex: MCP server set, and every kwak hook has a trust entry") {
			t.Errorf("CODEX_HOME %s: exit %d\n%s", f.env[EnvCodexHome], code, out)
		}
	}
}

func TestDoctorWindowsShell(t *testing.T) {
	cases := []struct {
		path map[string]string
		stat []string
		exe  string
		code int
		want string
	}{
		{map[string]string{"kwak": `C:\k\kwak.exe`, "pwsh": `C:\p\pwsh.exe`}, nil, `C:\k\kwak.exe`, 0, "ok    codex shell: Codex runs hooks with pwsh"},
		{map[string]string{"kwak": `C:\k\kwak.exe`}, []string{pwshFallback}, `C:\k\kwak.exe`, 0, "with pwsh"},
		{map[string]string{"kwak": `C:\k\kwak.exe`, "powershell": "x"}, nil, `C:\k\kwak.exe`, 0, "with powershell"},
		{map[string]string{"kwak": `C:\k\kwak.exe`}, nil, `C:\k\kwak.exe`, 0, "warn  codex shell: Codex runs hooks with cmd"},
		{map[string]string{"kwak": `C:\A B\kwak.exe`}, nil, `C:\A B\kwak.exe`, 1, "FAIL  codex shell: Codex runs hooks with cmd"},
	}
	for _, c := range cases {
		f := newFixture(t)
		f.goos = "windows"
		f.path = c.path
		for _, p := range c.stat {
			f.stat[p] = true
		}
		d := f.deps()
		d.Exe = c.exe
		var out bytes.Buffer
		code := Doctor(d)(nil, nil, &out, &bytes.Buffer{})
		if !strings.Contains(out.String(), c.want) || code != c.code {
			t.Errorf("%v %v: exit %d\n%s", c.path, c.stat, code, out.String())
		}
	}
}

func TestDoctorUsage(t *testing.T) {
	var errOut bytes.Buffer
	if code := Doctor(DoctorDeps{})([]string{"x"}, nil, &bytes.Buffer{}, &errOut); code != 2 || !strings.Contains(errOut.String(), "usage") {
		t.Fatalf("exit %d %q", code, errOut.String())
	}
}
