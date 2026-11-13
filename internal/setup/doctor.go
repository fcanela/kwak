package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/paths"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/wire"
)

// DoctorDeps is what kwak doctor reads. Preflight, Secrets and Call come from the OS packages;
// a nil one is reported as not checked. The other fields default to the real OS when zero.
type DoctorDeps struct {
	Preflight func() error                                    // oskeyring.Preflight
	Secrets   secrets.Secrets                                 // the OS keyring
	Call      func(ctx context.Context, req, reply any) error // one IPC request and its reply

	Getenv     func(string) string
	Home       string
	GOOS       string
	Exe        string
	LookPath   func(string) (string, error)
	Stat       func(string) (fs.FileInfo, error)
	ConfigDir  func() (string, error)
	SocketPath func() (string, error)
	Dev        bool // a dev build (paths.Dev): doctor says so and names its folder
	// Service is the state of the listener's user service, "not installed" when there is none
	// (service.Manager.Status). nil: not checked.
	Service func() (string, error)
}

// Doctor-only texts.
const (
	TextNotPaired     = wire.TextNotPaired
	TextKeyMissing    = wire.TextKeyMissing
	TextKeyringLocked = "waiting for the keyring (locked?)"
	TextNotRunning    = "kwak listener is not running. Start it with: kwak install (or systemctl --user start kwak)"
	TextListenerOlder = wire.TextListenerOlder
	TextCodexUntrust  = "Codex hooks not trusted: open codex once and choose Trust all"
)

// Windows paths Codex falls back to when PowerShell is not on PATH (codex shell_detect.rs).
const (
	pwshFallback       = `C:\Program Files\PowerShell\7\pwsh.exe`
	powershellFallback = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
)

func (d *DoctorDeps) fill() {
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	if d.Home == "" {
		d.Home, _ = os.UserHomeDir()
	}
	if d.GOOS == "" {
		d.GOOS = runtime.GOOS
	}
	if d.Exe == "" {
		d.Exe, _ = executable()
	}
	if d.LookPath == nil {
		d.LookPath = exec.LookPath
	}
	if d.Stat == nil {
		d.Stat = os.Stat
	}
	if d.ConfigDir == nil {
		d.ConfigDir = paths.ConfigDir
	}
	if d.SocketPath == nil {
		d.SocketPath = paths.SocketPath
	}
}

type report struct {
	w     io.Writer
	fails int
}

func (r *report) line(level, what, format string, a ...any) {
	if level == "FAIL" {
		r.fails++
	}
	fmt.Fprintf(r.w, "%-4s  %s: %s\n", level, what, fmt.Sprintf(format, a...))
}

// Doctor returns `kwak doctor`. It exits 1 when a check fails, and writes nothing anywhere.
func Doctor(d DoctorDeps) MainFunc {
	d.fill()
	return func(args []string, _ io.Reader, stdout, stderr io.Writer) int {
		if len(args) != 0 {
			fmt.Fprintln(stderr, "usage: kwak doctor")
			return 2
		}
		r := &report{w: stdout}
		d.run(r)
		if r.fails > 0 {
			return 1
		}
		return 0
	}
}

func (d *DoctorDeps) run(r *report) {
	if d.Dev {
		dir, _ := d.ConfigDir()
		r.line("warn", "build", "a dev build (make dev). It uses its own test folder, config %s, never your real pairing's.", dir)
	}
	for _, name := range []string{paths.EnvSocket, paths.EnvStateDir, paths.EnvConfigDir} {
		if v := d.Getenv(name); v != "" {
			r.line("warn", "env", "%s is set to %s. It is a test override; production never sets it.", name, v)
		}
	}
	busOK := d.checkBus(r)
	keyName := d.checkConfig(r)
	d.checkKeyring(r, busOK, keyName)
	d.checkListener(r)
	d.checkResponder(r)
	d.checkPath(r)
	d.checkClaude(r)
	d.checkCodex(r)
}

func (d *DoctorDeps) checkBus(r *report) bool {
	if d.Preflight == nil {
		r.line("skip", "bus", "not checked")
		return true
	}
	if err := d.Preflight(); err != nil {
		r.line("FAIL", "bus", "%v", err)
		return false
	}
	r.line("ok", "bus", "the keyring can be reached")
	return true
}

func (d *DoctorDeps) checkConfig(r *report) string {
	dir, err := d.ConfigDir()
	if err != nil {
		r.line("FAIL", "config", "%v", err)
		return ""
	}
	path := filepath.Join(dir, "config.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		r.line("FAIL", "config", "%s", TextNotPaired)
		return ""
	}
	if err != nil {
		r.line("FAIL", "config", "%v", err)
		return ""
	}
	var c wire.MachineConfig
	if err := json.Unmarshal(b, &c); err != nil || c.ServerName == "" {
		r.line("FAIL", "config", "%s has no server_name; run kwak pair", path)
		return ""
	}
	kind := ""
	if c.Test {
		kind = " (a test pairing)"
	}
	r.line("ok", "config", "paired as %s on %s, server %s%s", c.Owner, c.Machine, c.Server, kind)
	return c.KeyName()
}

// checkKeyring looks for the owner key under keyName, the item config.json names (secrets.KeyName).
func (d *DoctorDeps) checkKeyring(r *report, busOK bool, keyName string) {
	switch {
	case d.Secrets == nil:
		r.line("skip", "keyring", "not checked")
		return
	case !busOK:
		r.line("skip", "keyring", "not checked without a session bus")
		return
	case keyName == "":
		r.line("skip", "keyring", "not checked, because the machine is not paired")
		return
	}
	_, err := d.Secrets.Get(keyName)
	switch {
	case err == nil:
		r.line("ok", "keyring", "the owner key is there (not shown)")
	case errors.Is(err, secrets.ErrNotFound):
		r.line("FAIL", "keyring", "%s", TextKeyMissing)
	case errors.Is(err, secrets.ErrTimeout):
		r.line("FAIL", "keyring", "%s", TextKeyringLocked)
	default:
		r.line("FAIL", "keyring", "%v", err)
	}
}

func (d *DoctorDeps) checkListener(r *report) {
	sock, err := d.SocketPath()
	if err != nil {
		r.line("FAIL", "socket", "%v", err)
		return
	}
	if d.Call == nil {
		r.line("skip", "socket", "%s, not checked", sock)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var st wire.StatusResponse
	if err := d.Call(ctx, wire.Head{V: wire.ProtocolVersion, Op: wire.OpStatus}, &st); err != nil || !st.OK {
		r.line("FAIL", "socket", "%s at %s", TextNotRunning, sock)
		return
	}
	r.line("ok", "socket", "%s, listener %s, %s", sock, st.Kwak, st.State)
	if st.ListenerAccount != "" {
		r.line("ok", "account", "the listener syncs as %s, this pairing's own listener account", st.ListenerAccount)
	}
	if cmp, err := wire.CompareVersions(st.Kwak, wire.Version); err == nil && cmp < 0 {
		r.line("warn", "listener", "%s", TextListenerOlder)
	}
	switch {
	case st.State == wire.StateSyncing:
	case st.Detail != "":
		r.line("warn", "listener", "%s", st.Detail)
	case st.SyncError != "":
		r.line("warn", "listener", "%s: %s", st.State, st.SyncError)
	default:
		r.line("warn", "listener", "state %s", st.State)
	}
	if st.CallerChecks == "claimed" {
		r.line("warn", "listener", "caller checks: claimed (the listener cannot read the caller's process on this OS)")
	}
}

// checkResponder shows the responder of this machine (decision 46) when it is on, and warns when
// no installed service keeps a listener running to start its runs.
func (d *DoctorDeps) checkResponder(r *report) {
	dir, err := d.ConfigDir()
	if err != nil {
		return
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	var c wire.MachineConfig
	if err != nil || json.Unmarshal(b, &c) != nil || !c.Responder {
		return
	}
	folder := c.ResponderDir
	if folder == "" {
		folder = filepath.Join(dir, "responder")
	}
	r.line("ok", "responder", "on, in %s", folder)
	if _, err := d.Stat(filepath.Join(folder, "AGENTS.md")); err != nil {
		r.line("warn", "responder", "%s has no AGENTS.md; run kwak config responder on to write the default one", folder)
	}
	if (len(c.ResponderCommand) == 0) != (len(c.ResponderContinue) == 0) {
		r.line("warn", "responder", "set both responder_command and responder_continue, or neither; no responder run starts")
	}
	if len(c.ResponderCommand) == 0 && c.ResumeExe[wire.HarnessClaude] == "" {
		r.line("warn", "responder", "no path of claude for its runs; run kwak config resume-exe in your own terminal")
	}
	switch {
	case d.Dev:
		r.line("skip", "responder", "a dev build has no service: its runs start only while kwak listen runs in a terminal")
	case d.Service == nil:
	default:
		if st, err := d.Service(); err == nil && st == "not installed" {
			r.line("warn", "responder", "the listener is not installed as a service, so no run starts while no kwak listen runs; run kwak install")
		}
	}
}

func (d *DoctorDeps) checkPath(r *report) {
	found, err := d.LookPath("kwak")
	if err != nil {
		hint := `export PATH="$PATH:$(go env GOPATH)/bin"`
		if d.GOOS == "windows" {
			hint = `add %USERPROFILE%\go\bin to your user Path`
		}
		r.line("FAIL", "PATH", "kwak is not on PATH, so the harnesses cannot start it. Add Go's bin dir: %s", hint)
		return
	}
	if d.Exe != "" && !samePath(found, d.Exe) {
		r.line("warn", "PATH", "kwak on PATH is %s, but this kwak is %s", found, d.Exe)
		return
	}
	r.line("ok", "PATH", "%s", found)
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

func (d *DoctorDeps) checkClaude(r *report) {
	dir := d.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(d.Home, ".claude")
	}
	if _, err := d.Stat(dir); err != nil {
		r.line("skip", "claude", "no Claude Code config at %s", dir)
		return
	}
	var installed struct {
		Plugins map[string][]struct {
			Version string `json:"version"`
		} `json:"plugins"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "plugins", "installed_plugins.json"))
	json.Unmarshal(b, &installed)
	entries := installed.Plugins["kwak@kwak"]
	if len(entries) == 0 {
		r.line("warn", "claude", "the kwak plugin is not installed in Claude Code; see kwak setup claude")
		return
	}
	var settings struct {
		EnabledPlugins map[string]any `json:"enabledPlugins"`
		Permissions    struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	b, _ = os.ReadFile(filepath.Join(dir, "settings.json"))
	json.Unmarshal(b, &settings)
	ok := true
	if v := entries[0].Version; v != wire.Version {
		r.line("warn", "claude", "the kwak plugin is version %s, but kwak is %s; reinstall the plugin from tag v%s (kwak setup claude)", v, wire.Version, wire.Version)
		ok = false
	}
	if on, set := settings.EnabledPlugins["kwak@kwak"].(bool); set && !on {
		r.line("warn", "claude", "the kwak plugin is disabled in %s", filepath.Join(dir, "settings.json"))
		ok = false
	}
	if !slices.Contains(settings.Permissions.Allow, AllowRule) && !slices.Contains(settings.Permissions.Allow, "mcp__plugin_kwak_kwak") {
		r.line("warn", "claude", "%s has no allow rule %q, so Claude Code asks before each kwak tool call", filepath.Join(dir, "settings.json"), AllowRule)
		ok = false
	}
	if ok {
		r.line("ok", "claude", "the kwak plugin %s is installed and enabled", entries[0].Version)
	}
}

func (d *DoctorDeps) checkCodex(r *report) {
	home := d.Getenv(EnvCodexHome)
	if home == "" {
		home = filepath.Join(d.Home, ".codex")
	}
	if _, err := d.Stat(home); err != nil {
		r.line("skip", "codex", "no Codex config at %s", home)
		return
	}
	cfgPath := filepath.Join(home, "config.toml")
	hooksPath := filepath.Join(home, "hooks.json")
	cfg, _ := os.ReadFile(cfgPath)
	hooks, _ := os.ReadFile(hooksPath)
	entries := scanTOML(string(cfg))
	ok := true
	if !hasKwakServer(entries) {
		r.line("warn", "codex", "%s has no [mcp_servers.kwak]; run kwak setup codex --write", cfgPath)
		ok = false
	}
	keys := kwakHookKeys(hooks, filepath.Join(codexKeyHome(home, d.Getenv(EnvCodexHome) != ""), "hooks.json"))
	if len(keys) == 0 {
		r.line("warn", "codex", "%s has no kwak hooks; run kwak setup codex --write", hooksPath)
		ok = false
	} else {
		if _, ch, err := mergeHooks(hooks, d.Exe, d.GOOS); err == nil && ch.any() {
			r.line("warn", "codex", "kwak's hooks in %s differ from this kwak (%s); run kwak setup codex --write, then trust them again",
				hooksPath, strings.Join(append(ch.Added, ch.Changed...), ", "))
			ok = false
		}
		trusted := trustedHooks(entries)
		var missing []string
		for _, e := range codexEvents {
			for _, k := range keys[e.Name] {
				if !trusted[k] {
					missing = appendOnce(missing, e.Name)
				}
			}
		}
		if len(missing) > 0 {
			r.line("warn", "codex", "%s (no hooks.state entry for %s)", TextCodexUntrust, strings.Join(missing, ", "))
			ok = false
		}
	}
	if ok {
		r.line("ok", "codex", "MCP server set, and every kwak hook has a trust entry")
	}
	if d.GOOS == "windows" {
		d.checkWindowsShell(r)
	}
}

// codexKeyHome is the Codex home as Codex names it in hooks.state keys: canonical when it
// comes from CODEX_HOME, as given for the default (codex utils/home-dir).
func codexKeyHome(home string, fromEnv bool) string {
	if !fromEnv {
		return home
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return home
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// windowsShell is the shell Codex runs hooks with on Windows: PowerShell when it finds one,
// else cmd (codex shell_detect.rs).
func (d *DoctorDeps) windowsShell() string {
	for _, s := range []struct{ name, fallback string }{{"pwsh", pwshFallback}, {"powershell", powershellFallback}} {
		if _, err := d.LookPath(s.name); err == nil {
			return s.name
		}
		if _, err := d.Stat(s.fallback); err == nil {
			return s.name
		}
	}
	return "cmd"
}

func (d *DoctorDeps) checkWindowsShell(r *report) {
	sh := d.windowsShell()
	switch {
	case sh != "cmd":
		r.line("ok", "codex shell", "Codex runs hooks with %s", sh)
	case !windowsBare.MatchString(d.Exe):
		r.line("FAIL", "codex shell", "Codex runs hooks with cmd, which cannot run the PowerShell form kwak writes for this path (%s). Install PowerShell, or put kwak in a path of only letters, digits, _ . - and \\, and run kwak setup codex --write.", d.Exe)
	default:
		r.line("warn", "codex shell", "Codex runs hooks with cmd, not PowerShell. kwak's hook lines work there, but this is not tested yet.")
	}
}
