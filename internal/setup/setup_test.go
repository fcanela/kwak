package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/fcanela/kwak/internal/wire"
)

const exe = "/home/u/go/bin/kwak"

func codexHome(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "codex")
}

func runSetup(t *testing.T, e env, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut, e)
	return code, out.String(), errOut.String()
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestCommandForms(t *testing.T) {
	cases := []struct {
		exe, goos, command, windows string
	}{
		{exe, "linux", exe + " hook codex prompt", ""},
		{"/Users/a b/go/bin/kwak", "darwin", `'/Users/a b/go/bin/kwak' hook codex prompt`, ""},
		{"/home/o'neil/bin/kwak", "linux", `'/home/o'\''neil/bin/kwak' hook codex prompt`, ""},
		{`C:\Users\u\go\bin\kwak.exe`, "windows", `C:\Users\u\go\bin\kwak.exe hook codex prompt`, `C:\Users\u\go\bin\kwak.exe hook codex prompt`},
		{`C:\Users\Ana Ruiz\go\bin\kwak.exe`, "windows", `& 'C:\Users\Ana Ruiz\go\bin\kwak.exe' hook codex prompt`, `& 'C:\Users\Ana Ruiz\go\bin\kwak.exe' hook codex prompt`},
		{`C:\Users\o'brien\go\bin\kwak.exe`, "windows", `& 'C:\Users\o''brien\go\bin\kwak.exe' hook codex prompt`, `& 'C:\Users\o''brien\go\bin\kwak.exe' hook codex prompt`},
		{`C:\Users\a$b&(c)\kwak.exe`, "windows", `& 'C:\Users\a$b&(c)\kwak.exe' hook codex prompt`, `& 'C:\Users\a$b&(c)\kwak.exe' hook codex prompt`},
	}
	for _, c := range cases {
		g := codexGroup(c.exe, c.goos, codexEvents[1])
		h := g.Hooks[0]
		if h.Command != c.command || h.CommandWindows != c.windows || h.Timeout != 10 || h.Type != "command" {
			t.Errorf("%s %s: %+v", c.exe, c.goos, h)
		}
		if !isKwakCommand(h.Command, "prompt") {
			t.Errorf("%q is not recognised as kwak's", h.Command)
		}
	}
	for _, cmd := range []string{"/usr/bin/notkwak hook codex prompt", "kwak hook claude prompt", "kwak hook codex prompt --x",
		"kwak hook codex stop", "python3 hook.py"} {
		if isKwakCommand(cmd, "prompt") {
			t.Errorf("%q recognised as kwak's prompt hook", cmd)
		}
	}
	if g := codexGroup(exe, "linux", codexEvents[2]); g.Matcher != "*" {
		t.Errorf("PostToolUse matcher %q", g.Matcher)
	}
	if g := codexGroup(exe, "linux", codexEvents[5]); g.Hooks[0].Timeout != 3 {
		t.Errorf("SessionEnd timeout %d", g.Hooks[0].Timeout)
	}
}

func TestHooksDocShape(t *testing.T) {
	want := `{"description":"kwak: messages from other agents",
 "hooks":{
  "SessionStart":    [{"hooks":[{"type":"command","command":"/home/u/go/bin/kwak hook codex session-start","timeout":10}]}],
  "UserPromptSubmit":[{"hooks":[{"type":"command","command":"/home/u/go/bin/kwak hook codex prompt","timeout":10}]}],
  "PostToolUse":     [{"matcher":"*","hooks":[{"type":"command","command":"/home/u/go/bin/kwak hook codex post-tool","timeout":10}]}],
  "Stop":            [{"hooks":[{"type":"command","command":"/home/u/go/bin/kwak hook codex stop","timeout":10}]}],
  "SubagentStop":    [{"hooks":[{"type":"command","command":"/home/u/go/bin/kwak hook codex subagent-stop","timeout":10}]}],
  "SessionEnd":      [{"hooks":[{"type":"command","command":"/home/u/go/bin/kwak hook codex session-end","timeout":3}]}]
 }}`
	var compact bytes.Buffer
	json.Compact(&compact, []byte(want))
	var got bytes.Buffer
	json.Compact(&got, hooksDoc(exe, "linux"))
	if got.String() != compact.String() {
		t.Fatalf("got  %s\nwant %s", got.String(), compact.String())
	}
}

func TestWriteFresh(t *testing.T) {
	home := codexHome(t)
	e := env{codexHome: home, exe: exe, goos: "linux"}
	code, out, errOut := runSetup(t, e, "codex", "--write")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	cfg, hooks := filepath.Join(home, "config.toml"), filepath.Join(home, "hooks.json")
	if got := read(t, cfg); got != configBlock(exe) {
		t.Fatalf("config.toml:\n%s", got)
	}
	if got := read(t, hooks); got != string(hooksDoc(exe, "linux")) {
		t.Fatalf("hooks.json:\n%s", got)
	}
	for _, s := range []string{"Added the kwak MCP server", "Added kwak hooks", "Trust all and continue"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	for _, p := range []string{cfg, hooks} {
		if fi, _ := os.Stat(p); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", p, fi.Mode())
		}
	}
	before := read(t, cfg) + read(t, hooks)
	code, out, _ = runSetup(t, e, "codex", "--write")
	if code != 0 || read(t, cfg)+read(t, hooks) != before {
		t.Fatal("second run changed the files")
	}
	if !strings.Contains(out, "already has the kwak MCP server") || !strings.Contains(out, "already has kwak's hooks") || strings.Contains(out, "Trust") {
		t.Fatalf("second run output:\n%s", out)
	}
}

const userHooks = `{
  "description": "my hooks",
  "hooks": {
    "PostToolUse": [
      {"matcher": "shell", "hooks": [{"type": "command", "command": "/usr/local/bin/audit --json", "timeout": 5}]}
    ],
    "Stop": [{"hooks": [{"type": "command", "command": "notify-send done"}]}],
    "PreToolUse": [{"hooks": [{"type": "command", "command": "guard"}]}]
  }
}
`

const userConfig = `model = "gpt-5"

[mcp_servers.other]
command = "other"
# a comment`

func TestWriteMergesAndRemoves(t *testing.T) {
	home := codexHome(t)
	cfg, hooks := filepath.Join(home, "config.toml"), filepath.Join(home, "hooks.json")
	write(t, hooks, userHooks)
	write(t, cfg, userConfig)
	e := env{codexHome: home, exe: exe, goos: "linux"}
	if code, _, errOut := runSetup(t, e, "codex", "--write"); code != 0 {
		t.Fatal(errOut)
	}
	if got := read(t, cfg); got != userConfig+"\n\n"+configBlock(exe) {
		t.Fatalf("config.toml:\n%s", got)
	}
	var doc struct {
		Description string
		Hooks       map[string][]struct {
			Matcher string
			Hooks   []struct{ Command string }
		}
	}
	if err := json.Unmarshal([]byte(read(t, hooks)), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Description != "my hooks" {
		t.Errorf("description %q", doc.Description)
	}
	post := doc.Hooks["PostToolUse"]
	if len(post) != 2 || post[0].Hooks[0].Command != "/usr/local/bin/audit --json" || post[1].Hooks[0].Command != exe+" hook codex post-tool" {
		t.Errorf("PostToolUse %+v", post)
	}
	if stop := doc.Hooks["Stop"]; len(stop) != 2 || stop[0].Hooks[0].Command != "notify-send done" {
		t.Errorf("Stop %+v", stop)
	}
	if len(doc.Hooks["PreToolUse"]) != 1 || len(doc.Hooks["SessionStart"]) != 1 {
		t.Errorf("hooks %+v", doc.Hooks)
	}
	merged := read(t, cfg) + read(t, hooks)
	runSetup(t, e, "codex", "--write")
	if read(t, cfg)+read(t, hooks) != merged {
		t.Fatal("second run changed the files")
	}

	code, out, errOut := runSetup(t, e, "codex", "--remove")
	if code != 0 {
		t.Fatal(errOut)
	}
	if got := read(t, cfg); got != userConfig+"\n" {
		t.Fatalf("config.toml after remove:\n%q", got)
	}
	if !sameJSON([]byte(read(t, hooks)), []byte(userHooks)) {
		t.Fatalf("hooks.json after remove:\n%s", read(t, hooks))
	}
	if !strings.Contains(out, "Removed the kwak MCP server") || !strings.Contains(out, "Removed kwak hooks") {
		t.Fatalf("output:\n%s", out)
	}
	code, out, _ = runSetup(t, e, "codex", "--remove")
	if code != 0 || !strings.Contains(out, "has no kwak MCP server") || !strings.Contains(out, "has no kwak hooks") {
		t.Fatalf("second remove: %d\n%s", code, out)
	}
}

func TestRemoveDeletesFilesKwakMade(t *testing.T) {
	home := codexHome(t)
	e := env{codexHome: home, exe: exe, goos: "linux"}
	runSetup(t, e, "codex", "--write")
	if code, _, errOut := runSetup(t, e, "codex", "--remove"); code != 0 {
		t.Fatal(errOut)
	}
	for _, f := range []string{"config.toml", "hooks.json"} {
		if exists(filepath.Join(home, f)) {
			t.Errorf("%s still there", f)
		}
	}
	empty := codexHome(t)
	if code, _, errOut := runSetup(t, env{codexHome: empty, exe: exe, goos: "linux"}, "codex", "--remove"); code != 0 {
		t.Fatal(errOut)
	}
	if exists(empty) {
		t.Fatal("--remove created the Codex home")
	}
}

func TestWriteChangedEntry(t *testing.T) {
	home := codexHome(t)
	cfg, hooks := filepath.Join(home, "config.toml"), filepath.Join(home, "hooks.json")
	old := env{codexHome: home, exe: "/old/bin/kwak", goos: "linux"}
	runSetup(t, old, "codex", "--write")
	write(t, hooks, strings.Replace(read(t, hooks), `"Stop": [`, `"Stop": [{"hooks":[{"type":"command","command":"say hi"}]},`, 1))
	code, out, _ := runSetup(t, env{codexHome: home, exe: exe, goos: "linux"}, "codex", "--write")
	if code != 0 || !strings.Contains(out, "Updated the kwak MCP server") || !strings.Contains(out, "Changed kwak hooks") ||
		!strings.Contains(out, "Modified") || strings.Contains(out, "Added kwak hooks") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Contains(read(t, cfg)+read(t, hooks), "/old/bin") {
		t.Fatal("old path left behind")
	}
	keys := kwakHookKeys([]byte(read(t, hooks)), hooks)
	if got := keys["Stop"]; len(got) != 1 || got[0] != hooks+":stop:1:0" {
		t.Fatalf("Stop key moved: %v", got)
	}
	if got := keys["SessionStart"]; len(got) != 1 || got[0] != hooks+":session_start:0:0" {
		t.Fatalf("SessionStart key: %v", got)
	}
}

func TestDuplicateKwakEntries(t *testing.T) {
	g := `{"hooks":[{"type":"command","command":"` + exe + ` hook codex stop","timeout":10}]}`
	in := `{"hooks":{"Stop":[` + g + `,` + g + `]}}`
	out, ch, err := mergeHooks([]byte(in), exe, "linux")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Hooks map[string][]json.RawMessage }
	json.Unmarshal(out, &doc)
	if len(doc.Hooks["Stop"]) != 1 || !contains(ch.Changed, "Stop") {
		t.Fatalf("Stop %s, changes %+v", doc.Hooks["Stop"], ch)
	}
}

func TestForeignServer(t *testing.T) {
	for _, cfgText := range []string{
		"[mcp_servers.kwak]\ncommand = \"/opt/kwak\"\n",
		"[mcp_servers.\"kwak\"]\ncommand = \"/opt/kwak\"\n",
		"[mcp_servers]\nkwak = { command = \"/opt/kwak\" }\n",
		"mcp_servers.kwak.command = \"/opt/kwak\"\n",
	} {
		home := codexHome(t)
		cfg := filepath.Join(home, "config.toml")
		write(t, cfg, cfgText)
		code, out, _ := runSetup(t, env{codexHome: home, exe: exe, goos: "linux"}, "codex", "--write")
		if code != 0 || read(t, cfg) != cfgText || !strings.Contains(out, "left it alone") {
			t.Errorf("%q: exit %d, file %q\n%s", cfgText, code, read(t, cfg), out)
		}
		_, out, _ = runSetup(t, env{codexHome: home, exe: exe, goos: "linux"}, "codex", "--remove")
		if read(t, cfg) != cfgText || !strings.Contains(out, "remove it by hand") {
			t.Errorf("%q: remove changed it\n%s", cfgText, out)
		}
	}
}

func TestBadFilesWriteNothing(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"hooks.json not JSON":   {"hooks.json": "{nope"},
		"hooks.json array":      {"hooks.json": "[]"},
		"hooks not an object":   {"hooks.json": `{"hooks":[]}`},
		"event not a list":      {"hooks.json": `{"hooks":{"Stop":{}}}`},
		"start marker, no end":  {"config.toml": markerBegin + "\n[mcp_servers.kwak]\n"},
		"trailing data in JSON": {"hooks.json": `{} {}`},
	} {
		home := codexHome(t)
		for f, data := range files {
			write(t, filepath.Join(home, f), data)
		}
		code, _, errOut := runSetup(t, env{codexHome: home, exe: exe, goos: "linux"}, "codex", "--write")
		if code != 1 || errOut == "" {
			t.Errorf("%s: exit %d, stderr %q", name, code, errOut)
		}
		for _, f := range []string{"config.toml", "hooks.json"} {
			want, inFiles := files[f]
			got, err := os.ReadFile(filepath.Join(home, f))
			if inFiles && string(got) != want || !inFiles && err == nil {
				t.Errorf("%s: %s was written", name, f)
			}
		}
	}
}

func TestMainUsesCodexHome(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "codex-home")
	t.Setenv(EnvCodexHome, home)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	var out, errOut bytes.Buffer
	if code := Main([]string{"codex", "--write"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.HasPrefix(home, dir) || !exists(filepath.Join(home, "hooks.json")) || !exists(filepath.Join(home, "config.toml")) {
		t.Fatal("Main did not write into CODEX_HOME")
	}
	me, _ := executable()
	if !strings.Contains(read(t, filepath.Join(home, "config.toml")), tomlString(me)) {
		t.Fatal("config.toml does not name this binary")
	}
	if exists(filepath.Join(dir, "home", ".codex")) {
		t.Fatal("wrote to ~/.codex")
	}
}

func TestClaude(t *testing.T) {
	code, out, _ := runSetup(t, env{}, "claude")
	if code != 0 || !strings.Contains(out, "claude plugin marketplace add fcanela/kwak#v"+wire.Version+"\n") ||
		!strings.Contains(out, "claude plugin install kwak@kwak\n") || !strings.Contains(out, `"allow": ["mcp__plugin_kwak_kwak__*"]`) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, mode := range [][]string{{"--write"}, {"--remove"}, {"--exe", "/opt/kwak"}} {
		code, out, errOut := runSetup(t, env{}, append([]string{"claude"}, mode...)...)
		if code != 2 || out != "" || !strings.Contains(errOut, "never writes") {
			t.Errorf("%s: exit %d, %q %q", mode, code, out, errOut)
		}
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"vim"}, {"codex", "--force"}, {"codex", "--write", "--remove"},
		{"codex", "--write", "extra"}, {"codex", "--exe"}} {
		if code, _, errOut := runSetup(t, env{}, args...); code != 2 || !strings.Contains(errOut, "usage") {
			t.Errorf("%v: exit %d %q", args, code, errOut)
		}
	}
}

// --exe is what a package manager passes so the config names its stable link, not the running
// binary. The order of the flags does not matter, and the path must be absolute.
func TestExeFlag(t *testing.T) {
	const profile = "/home/u/.nix-profile/bin/kwak"
	home := codexHome(t)
	e := env{codexHome: home, exe: "/nix/store/abc-kwak/bin/kwak", goos: "linux"}
	for _, args := range [][]string{{"codex", "--write", "--exe", profile}, {"codex", "--exe=" + profile, "--write"}} {
		if code, _, errOut := runSetup(t, e, args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
		if got := read(t, filepath.Join(home, "config.toml")); got != configBlock(profile) {
			t.Fatalf("%v: config.toml:\n%s", args, got)
		}
		if got := read(t, filepath.Join(home, "hooks.json")); got != string(hooksDoc(profile, "linux")) {
			t.Fatalf("%v: hooks.json:\n%s", args, got)
		}
	}
	code, out, _ := runSetup(t, e, "codex", "--exe", profile+"/")
	if code != 0 || out != codexSteps(profile, "linux") {
		t.Fatalf("printing with --exe: exit %d\n%s", code, out)
	}
	if code, _, errOut := runSetup(t, e, "codex", "--remove", "--exe", profile); code != 0 {
		t.Fatalf("remove: exit %d: %s", code, errOut)
	}
	if exists(filepath.Join(home, "config.toml")) {
		t.Fatal("remove left config.toml")
	}
	code, _, errOut := runSetup(t, e, "codex", "--write", "--exe", "bin/kwak")
	if code != 2 || !strings.Contains(errOut, "not an absolute path") {
		t.Fatalf("relative --exe: exit %d %q", code, errOut)
	}
	if exists(filepath.Join(home, "config.toml")) {
		t.Fatal("a refused --exe wrote config.toml")
	}
}

func TestPrint(t *testing.T) {
	var b bytes.Buffer
	if err := Print(&b, wire.HarnessClaude); err != nil || b.String() != claudeSteps() {
		t.Fatalf("claude: %v", err)
	}
	b.Reset()
	me, _ := executable()
	if err := Print(&b, wire.HarnessCodex); err != nil || !strings.Contains(b.String(), tomlString(me)) || !strings.Contains(b.String(), "hook codex stop") {
		t.Fatalf("codex: %v\n%s", err, b.String())
	}
	if err := Print(&b, "vim"); err == nil {
		t.Fatal("no error for an unknown harness")
	}
	code, out, _ := runSetup(t, env{exe: exe, goos: "linux"}, "codex")
	if code != 0 || out != codexSteps(exe, "linux") {
		t.Fatalf("kwak setup codex printed:\n%s", out)
	}
	if !strings.Contains(out, configBlock(exe)) || !strings.Contains(out, string(hooksDoc(exe, "linux"))) {
		t.Fatal("printed snippets differ from what --write writes")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(root, "go.mod")) {
		t.Fatalf("%s is not the repo root", root)
	}
	return root
}

// TestIntegrationFiles keeps integrations/codex equal to what kwak setup codex prints.
func TestIntegrationFiles(t *testing.T) {
	root := repoRoot(t)
	files := map[string]string{
		"hooks.json":         strings.ReplaceAll(string(hooksDoc("/home/YOU/go/bin/kwak", "linux")), "YOU", "<you>"),
		"hooks.windows.json": strings.ReplaceAll(string(hooksDoc(`C:\Users\YOU\go\bin\kwak.exe`, "windows")), "YOU", "<you>"),
		"config.toml":        strings.ReplaceAll(configBlock("/home/YOU/go/bin/kwak"), "YOU", "<you>"),
	}
	for name, want := range files {
		if got := read(t, filepath.Join(root, "integrations", "codex", name)); got != want {
			t.Errorf("integrations/codex/%s differs from the rendered snippet:\n%s\nwant:\n%s", name, got, want)
		}
	}
}

func parseJSON(t *testing.T, path string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(read(t, path)), &v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

func specJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestPluginFiles checks the Claude Code plugin files against spec 6.2.
func TestPluginFiles(t *testing.T) {
	root := repoRoot(t)
	cases := map[string]string{
		".claude-plugin/marketplace.json": `{"name":"kwak","owner":{"name":"fcanela"},
 "plugins":[{"name":"kwak","source":"./plugins/claude","description":"Talk to other agents on kwak (Matrix)."}]}`,
		"plugins/claude/.claude-plugin/plugin.json": `{"name":"kwak","version":"` + wire.Version + `",
 "description":"Agents talk to each other on kwak, and notice when they are addressed. Needs kwak on PATH and a paired listener.",
 "repository":"https://github.com/fcanela/kwak","license":"MIT"}`,
		"plugins/claude/.mcp.json": `{"mcpServers":{"kwak":{"command":"kwak","args":["mcp","--harness","claude"]}}}`,
		"plugins/claude/hooks/hooks.json": `{"description":"kwak: messages from other agents. Prints nothing when there is nothing for this session.",
 "hooks":{
  "SessionStart":    [{"hooks":[{"type":"command","command":"kwak","args":["hook","claude","session-start"],"timeout":10}]}],
  "UserPromptSubmit":[{"hooks":[{"type":"command","command":"kwak","args":["hook","claude","prompt"],"timeout":10}]}],
  "PostToolUse":     [{"hooks":[{"type":"command","command":"kwak","args":["hook","claude","post-tool"],"timeout":10}]}],
  "PostToolUseFailure":[{"hooks":[{"type":"command","command":"kwak","args":["hook","claude","post-tool"],"timeout":10}]}],
  "Stop":            [{"hooks":[{"type":"command","command":"kwak","args":["hook","claude","stop"],"timeout":10}]}],
  "SubagentStop":    [{"hooks":[{"type":"command","command":"kwak","args":["hook","claude","subagent-stop"],"timeout":10}]}],
  "SessionEnd":      [{"hooks":[{"type":"command","command":"kwak","args":["hook","claude","session-end"],"timeout":2}]}]
 }}`,
	}
	for rel, want := range cases {
		if got := parseJSON(t, filepath.Join(root, rel)); !reflect.DeepEqual(got, specJSON(t, want)) {
			t.Errorf("%s differs from spec 6.2", rel)
		}
	}
	var hooks struct {
		Hooks map[string][]struct {
			Hooks []struct{ Args []string }
		}
	}
	json.Unmarshal([]byte(read(t, filepath.Join(root, "plugins/claude/hooks/hooks.json"))), &hooks)
	for ev, groups := range hooks.Hooks {
		a := groups[0].Hooks[0].Args
		if len(a) != 3 || a[0] != "hook" || a[1] != "claude" || !wire.ValidEvent(a[2]) {
			t.Errorf("%s: args %v", ev, a)
		}
	}
}

// TestClaudePluginValidate runs `claude plugin validate` (no model call) with a throwaway config dir.
func TestClaudePluginValidate(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not on PATH; skipping claude plugin validate")
	}
	root := repoRoot(t)
	home := t.TempDir()
	for _, target := range []string{"./plugins/claude", "."} {
		cmd := exec.Command(claude, "plugin", "validate", "--json", target)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "HOME="+home, "CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("claude plugin validate %s: %v\n%s", target, err, out)
		}
		var rep struct {
			Success  bool
			Manifest struct {
				Errors   []any
				Warnings []struct{ Path string }
			}
			Contents []struct {
				File     string
				Errors   []any
				Warnings []struct{ Message string }
			}
		}
		if err := json.Unmarshal(out, &rep); err != nil {
			t.Fatalf("%s: %v\n%s", target, err, out)
		}
		if !rep.Success || len(rep.Manifest.Errors) > 0 {
			t.Fatalf("%s: validation failed:\n%s", target, out)
		}
		for _, w := range rep.Manifest.Warnings {
			if w.Path != "author" && w.Path != "description" && !strings.HasSuffix(w.Path, "author") {
				t.Errorf("%s: manifest warning on %s", target, w.Path)
			}
		}
		for _, c := range rep.Contents {
			if len(c.Errors)+len(c.Warnings) > 0 {
				t.Errorf("%s: %s: %+v %+v", target, c.File, c.Errors, c.Warnings)
			}
		}
	}
}

func TestTrustInsideBlockSurvives(t *testing.T) {
	home := codexHome(t)
	cfg, hooks := filepath.Join(home, "config.toml"), filepath.Join(home, "hooks.json")
	write(t, cfg, userConfig)
	old := env{codexHome: home, exe: "/old/bin/kwak", goos: "linux"}
	if code, _, errOut := runSetup(t, old, "codex", "--write"); code != 0 {
		t.Fatal(errOut)
	}
	text := trusted(t, read(t, cfg), hooks)
	sub := "\n[mcp_servers.kwak.tools.send]\napproval_mode = \"approve\"\n"
	i := strings.LastIndex(text, markerEnd)
	text = text[:i] + sub + text[i:]
	write(t, cfg, text)
	trust := scanTOML(text)
	if len(trustedHooks(trust)) != len(codexEvents) {
		t.Fatalf("fixture has %d trust entries", len(trustedHooks(trust)))
	}

	code, out, _ := runSetup(t, old, "codex", "--write")
	if code != 0 || read(t, cfg) != text || !strings.Contains(out, "already has the kwak MCP server") {
		t.Fatalf("same kwak rewrote config.toml (exit %d):\n%s\n%s", code, out, read(t, cfg))
	}

	code, out, _ = runSetup(t, env{codexHome: home, exe: exe, goos: "linux"}, "codex", "--write")
	got := read(t, cfg)
	if code != 0 || !strings.Contains(out, "Updated the kwak MCP server") || strings.Contains(got, "/old/bin") {
		t.Fatalf("exit %d:\n%s\n%s", code, out, got)
	}
	if got != strings.Replace(text, "/old/bin/kwak", exe, 1) {
		t.Fatalf("config.toml:\n%s", got)
	}
	entries := scanTOML(got)
	if fmt.Sprint(trustedHooks(entries)) != fmt.Sprint(trustedHooks(trust)) {
		t.Fatalf("trust entries lost:\n%s", got)
	}
	again := got
	if runSetup(t, env{codexHome: home, exe: exe, goos: "linux"}, "codex", "--write"); read(t, cfg) != again {
		t.Fatal("third run changed config.toml")
	}

	if code, _, errOut := runSetup(t, env{codexHome: home, exe: exe, goos: "linux"}, "codex", "--remove"); code != 0 {
		t.Fatal(errOut)
	}
	got = read(t, cfg)
	entries = scanTOML(got)
	if hasKwakServer(entries) || strings.Contains(got, "kwak") || fmt.Sprint(trustedHooks(entries)) != fmt.Sprint(trustedHooks(trust)) ||
		!strings.HasPrefix(got, userConfig+"\n") {
		t.Fatalf("config.toml after remove:\n%s", got)
	}

	edited := codexHome(t)
	write(t, filepath.Join(edited, "config.toml"), markerBegin+"\nmodel = \"x\"\n[mcp_servers.kwak]\n"+markerEnd+"\n")
	code, _, errOut := runSetup(t, env{codexHome: edited, exe: exe, goos: "linux"}, "codex", "--write")
	if code != 1 || !strings.Contains(errOut, "fix it by hand") || exists(filepath.Join(edited, "hooks.json")) {
		t.Fatalf("edited block: exit %d %q", code, errOut)
	}
}

func TestInlineMCPServers(t *testing.T) {
	for _, c := range []struct {
		cfg  string
		code int
		out  string
	}{
		{"mcp_servers = { other = { command = \"x\" } }\n", 1, "line 1 as a value"},
		{"model = \"m\"\nmcp_servers = {}\n", 1, "line 2 as a value"},
		{"[[mcp_servers]]\ncommand = \"x\"\n", 1, "as a value"},
		{"mcp_servers = { kwak = { command = \"/opt/kwak\" } }\n", 0, "left it alone"},
		{"mcp_servers = { other = { kwak = 1 }, \"kwak\".command = \"/opt/kwak\" }\n", 0, "left it alone"},
		{"mcp_servers.other = { command = \"x\" }\n", 0, "Added the kwak MCP server"},
		{"[profiles.p]\nmcp_servers = { other = { command = \"x\" } }\n", 0, "Added the kwak MCP server"},
	} {
		home := codexHome(t)
		cfg := filepath.Join(home, "config.toml")
		write(t, cfg, c.cfg)
		code, out, errOut := runSetup(t, env{codexHome: home, exe: exe, goos: "linux"}, "codex", "--write")
		if code != c.code || !strings.Contains(out+errOut, c.out) {
			t.Errorf("%q: exit %d\n%s%s", c.cfg, code, out, errOut)
		}
		if c.code == 1 {
			if read(t, cfg) != c.cfg || exists(filepath.Join(home, "hooks.json")) || !strings.Contains(errOut, inlineServer(exe)) {
				t.Errorf("%q: wrote something, or no entry to paste: %q", c.cfg, errOut)
			}
		}
	}
	if got := inlineKeys(`{ a = 1, "b.c" = { d = 2 }, 'e'.f = [1, {g = 3}], h="x,{i=1" }`); fmt.Sprint(got) != "[a b.c e h]" {
		t.Fatalf("inlineKeys %v", got)
	}
}

func TestWriteFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "codex")
	dots := filepath.Join(dir, "dotfiles")
	write(t, filepath.Join(dots, "config.toml"), "model = \"x\"\n")
	write(t, filepath.Join(dots, "hooks.json"), "{}\n")
	os.MkdirAll(home, 0o700)
	for _, f := range []string{"config.toml", "hooks.json"} {
		if err := os.Symlink(filepath.Join("..", "dotfiles", f), filepath.Join(home, f)); err != nil {
			t.Skip("no symlinks here:", err)
		}
	}
	isLink := func(f string) bool {
		fi, err := os.Lstat(filepath.Join(home, f))
		return err == nil && fi.Mode()&os.ModeSymlink != 0
	}
	e := env{codexHome: home, exe: exe, goos: "linux"}
	if code, _, errOut := runSetup(t, e, "codex", "--write"); code != 0 {
		t.Fatal(errOut)
	}
	if !isLink("config.toml") || !isLink("hooks.json") {
		t.Fatal("--write replaced a link with a file")
	}
	if read(t, filepath.Join(dots, "config.toml")) != "model = \"x\"\n\n"+configBlock(exe) ||
		!strings.Contains(read(t, filepath.Join(dots, "hooks.json")), "hook codex stop") {
		t.Fatal("the link targets did not get kwak's config")
	}
	if fi, _ := os.Stat(filepath.Join(dots, "config.toml")); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o644 {
		t.Errorf("target mode %v", fi.Mode())
	}
	if code, _, errOut := runSetup(t, e, "codex", "--remove"); code != 0 {
		t.Fatal(errOut)
	}
	if !isLink("config.toml") || !isLink("hooks.json") || read(t, filepath.Join(dots, "config.toml")) != "model = \"x\"\n" ||
		strings.Contains(read(t, filepath.Join(dots, "hooks.json")), "kwak hook") {
		t.Fatal("--remove broke a link or left kwak behind")
	}

	write(t, filepath.Join(dots, "config.toml"), configBlock(exe))
	write(t, filepath.Join(dots, "hooks.json"), string(hooksDoc(exe, "linux")))
	if code, _, errOut := runSetup(t, e, "codex", "--remove"); code != 0 {
		t.Fatal(errOut)
	}
	if !isLink("config.toml") || !isLink("hooks.json") || read(t, filepath.Join(dots, "config.toml")) != "" ||
		strings.Contains(read(t, filepath.Join(dots, "hooks.json")), "kwak hook") {
		t.Fatal("--remove of files kwak made broke a link")
	}

	dangling := codexHome(t)
	os.MkdirAll(dangling, 0o700)
	os.Symlink(filepath.Join(dir, "missing", "config.toml"), filepath.Join(dangling, "config.toml"))
	code, _, errOut := runSetup(t, env{codexHome: dangling, exe: exe, goos: "linux"}, "codex", "--write")
	if code != 1 || !strings.Contains(errOut, "link kwak cannot follow") {
		t.Fatalf("dangling link: exit %d %q", code, errOut)
	}
}

// A dev build writes or removes Codex's config only in a CODEX_HOME that was set, never in the
// default ~/.codex of the real kwak (decision 40). Printing the steps still works.
func TestDevBuildCodexHome(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".codex")
	for _, mode := range []string{"--write", "--remove"} {
		code, _, errOut := runSetup(t, env{codexHome: home, exe: exe, goos: "linux", dev: true}, "codex", mode)
		if code != 1 || !strings.Contains(errOut, "CODEX_HOME you set") {
			t.Fatalf("%s: exit %d: %s", mode, code, errOut)
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("the default Codex home was touched: %v", err)
	}
	if code, out, _ := runSetup(t, env{codexHome: home, exe: exe, goos: "linux", dev: true}, "codex"); code != 0 || !strings.Contains(out, "[mcp_servers.kwak]") {
		t.Fatalf("print: exit %d: %s", code, out)
	}
	code, _, errOut := runSetup(t, env{codexHome: home, exe: exe, goos: "linux", dev: true, homeSet: true}, "codex", "--write")
	if code != 0 {
		t.Fatalf("explicit CODEX_HOME: exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); err != nil {
		t.Fatal(err)
	}
}
