package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/check"
)

// fakeClaude writes a claude double in a folder whose name needs quoting. It records its
// arguments, its stdin and its folder, then prints the answer file and exits with the code file.
func fakeClaude(t *testing.T) (exe, dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "it's a dir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	exe = filepath.Join(dir, "claude")
	q := shQuote(dir)
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + q + "/args\n" +
		"cat > " + q + "/stdin\n" +
		"pwd > " + q + "/cwd\n" +
		"cat " + q + "/answer\n" +
		"exit $(cat " + q + "/code)\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return exe, dir
}

// The default screener runs through the check command contract: OK passes, NO refuses with its
// reason, and anything else refuses (fail closed). The model gets the fence and the message.
func TestDefaultScreener(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the default screener is a shell script")
	}
	exe, fake := fakeClaude(t)
	cfg := t.TempDir()
	script, err := writeScreener(cfg, "outgoing", exe, false)
	if err != nil || script != filepath.Join(cfg, ScreenerFolder, "check-outgoing") {
		t.Fatalf("%q %v", script, err)
	}
	for _, f := range []struct {
		name string
		mode os.FileMode
	}{{"", 0o700}, {"check-outgoing", 0o700}, {"outgoing.md", 0o600}} {
		if fi, err := os.Stat(filepath.Join(cfg, ScreenerFolder, f.name)); err != nil || fi.Mode().Perm() != f.mode {
			t.Errorf("%s: %v %v", f.name, fi, err)
		}
	}
	run := func(answer, code string) check.Verdict {
		t.Helper()
		os.WriteFile(filepath.Join(fake, "answer"), []byte(answer), 0o600)
		os.WriteFile(filepath.Join(fake, "code"), []byte(code), 0o600)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return check.Command(ctx, check.Exec{}, script, "is the deploy on friday?")
	}
	for _, c := range []struct {
		answer, code string
		want         check.Verdict
	}{
		{"OK\n", "0", check.Verdict{OK: true, Kind: "command"}},
		{"NO: it asks for an action\n", "0", check.Verdict{Kind: "command", Reason: "it asks for an action"}},
		{"OK\n", "2", check.Verdict{Kind: "command", Reason: "the screener model did not answer"}},
		{"Sure, here is the answer\n", "0", check.Verdict{Kind: "command", Reason: "the screener model gave no OK or NO"}},
		{"", "0", check.Verdict{Kind: "command", Reason: "the screener model gave no OK or NO"}},
	} {
		if got := run(c.answer, c.code); got != c.want {
			t.Errorf("%q exit %s: %+v, want %+v", c.answer, c.code, got, c.want)
		}
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(fake, name))
		return string(b)
	}
	if got := read("stdin"); got != "is the deploy on friday?" {
		t.Errorf("stdin %q", got)
	}
	if got := read("cwd"); got != "/\n" {
		t.Errorf("folder %q, want / so no CLAUDE.md loads", got)
	}
	want := "-p\n" + screenerPrompts["outgoing"] + "\n--model\nhaiku\n--setting-sources\nproject\n--tools\n\n" +
		"--strict-mcp-config\n--mcp-config\n{\"mcpServers\":{}}\n--permission-mode\ndontAsk\n"
	// $(cat) drops the prompt's last newline.
	want = strings.Replace(want, screenerPrompts["outgoing"]+"\n", strings.TrimRight(screenerPrompts["outgoing"], "\n")+"\n", 1)
	if got := read("args"); got != want {
		t.Errorf("args:\n%s\nwant:\n%s", got, want)
	}

	// The user's prompt is read at each run, and kwak never writes over the user's files.
	prompt := filepath.Join(cfg, ScreenerFolder, "outgoing.md")
	os.WriteFile(prompt, []byte("my prompt"), 0o600)
	edited := []byte("#!/bin/sh\necho OK\n")
	other, _ := writeScreener(cfg, "incoming", exe, false)
	if err := os.WriteFile(other, edited, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := writeScreener(cfg, "outgoing", exe, false); err != nil {
		t.Fatal(err)
	}
	if _, err := writeScreener(cfg, "incoming", "/other/claude", false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(prompt); string(b) != "my prompt" {
		t.Errorf("the prompt was written over: %q", b)
	}
	if b, _ := os.ReadFile(other); string(b) != string(edited) {
		t.Errorf("the script was written over: %q", b)
	}
	run("OK", "0")
	if got := read("args"); !strings.HasPrefix(got, "-p\nmy prompt\n--model") {
		t.Errorf("the edited prompt did not reach the model: %q", got)
	}
}

func TestDefaultScreenerRefused(t *testing.T) {
	cfg := t.TempDir()
	for _, c := range []struct {
		claude  string
		windows bool
		want    string
	}{
		{"", false, "run kwak config resume-exe first"},
		{"/opt/claude", true, "Windows does not run"},
		{"claude", false, "not an absolute path"},
		{"/opt/cl\naude", false, "not an absolute path"},
	} {
		if _, err := writeScreener(cfg, "outgoing", c.claude, c.windows); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q windows %v: %v", c.claude, c.windows, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg, ScreenerFolder)); !os.IsNotExist(err) {
		t.Errorf("a refused screener made its folder: %v", err)
	}
}

// kwak config check-outgoing default records the screener kwak writes (decision 46); both
// screeners stay off until then.
func TestConfigCheckDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the default screener is a shell script")
	}
	r := newRig(t)
	r.paired()
	for _, way := range []string{"outgoing", "incoming"} {
		code, out, errs := r.run(r.cli.Config, "", "check-"+way, "default")
		script := filepath.Join(r.cfgDir, ScreenerFolder, "check-"+way)
		if code != 0 || r.config()["check_"+way] != script || !strings.Contains(out, "default screener") {
			t.Fatalf("%s: exit %d %s %s, config %v", way, code, out, errs, r.config()["check_"+way])
		}
		if b, _ := os.ReadFile(script); !strings.Contains(string(b), "'/opt/claude' -p") {
			t.Errorf("%s: the script does not run resume_exe's claude:\n%s", way, b)
		}
	}
	data, _ := os.ReadFile(filepath.Join(r.cfgDir, ConfigFile))
	data = []byte(strings.Replace(string(data), `"claude": "/opt/claude"`, `"claude": ""`, 1))
	if err := os.WriteFile(filepath.Join(r.cfgDir, ConfigFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := r.run(r.cli.Config, "", "check-outgoing", "default"); code != 1 || !strings.Contains(errs, "resume-exe") {
		t.Errorf("no claude path: exit %d %s", code, errs)
	}
}
