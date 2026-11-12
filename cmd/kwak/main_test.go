package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/cli"
	"github.com/fcanela/kwak/internal/listener"
	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/paths"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/proc/proctest"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/secrets/secretstest"
	"github.com/fcanela/kwak/internal/service"
	"github.com/fcanela/kwak/internal/service/servicetest"
	"github.com/fcanela/kwak/internal/setup"
	"github.com/fcanela/kwak/internal/wire"
)

// childEnv makes the test binary act as a background-resume child (TestExecRunner). A "wrapper"
// starts the test binary again, in the mode named by grandchildEnv, without exec, and waits.
const (
	childEnv      = "KWAK_CMD_TEST_CHILD"
	grandchildEnv = "KWAK_CMD_TEST_GRANDCHILD"
)

func TestMain(m *testing.M) {
	switch os.Getenv(childEnv) {
	case "pwd":
		wd, _ := os.Getwd()
		fmt.Println(wd, os.Args[len(os.Args)-1])
		os.Exit(0)
	case "sleep":
		fmt.Println("ready", os.Getpid())
		time.Sleep(time.Minute)
		os.Exit(0)
	case "stubborn":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready", os.Getpid())
		time.Sleep(time.Minute)
		os.Exit(0)
	case "wrapper":
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), childEnv+"="+os.Getenv(grandchildEnv))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if cmd.Run() != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// refuseDoer fails the test on any HTTP request.
type refuseDoer struct{ t *testing.T }

func (d refuseDoer) Do(r *http.Request) (*http.Response, error) {
	d.t.Errorf("unexpected HTTP request to %s", r.URL.Host)
	return nil, errors.New("no network in tests")
}

// rig is a sys with doubles. It counts how often each OS piece is built.
type rig struct {
	sys
	keyring  secrets.Secrets
	rec      *servicetest.Recorder
	home     string
	doer     refuseDoer
	built    map[string]int
	listened []cli.ListenConfig
	deps     []listener.Deps
	detached []string
	superv   []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	for name, sub := range map[string]string{
		paths.EnvConfigDir: "config", paths.EnvStateDir: "state", paths.EnvSocket: "run/kwak.sock",
		"HOME": "home", "XDG_CONFIG_HOME": "home/.config", "CLAUDE_CONFIG_DIR": "claude", setup.EnvCodexHome: "codex",
	} {
		t.Setenv(name, filepath.Join(dir, sub))
	}
	// Set on a machine whose home-manager installs kwak; a test must not inherit them.
	t.Setenv(cli.EnvServer, "")
	t.Setenv(cli.EnvOwner, "")
	r := &rig{keyring: secretstest.Mem(), rec: servicetest.New(), home: filepath.Join(dir, "home"),
		doer: refuseDoer{t}, built: map[string]int{}}
	r.sys = sys{
		Keyring:    func() secrets.Secrets { r.built["keyring"]++; return r.keyring },
		Preflight:  func() error { r.built["preflight"]++; return nil },
		Service:    func() service.Manager { r.built["service"]++; return service.Systemd(r.rec, r.home) },
		HTTP:       func() matrix.Doer { r.built["http"]++; return r.doer },
		IsTerminal: func() bool { return false },
		Procs:      func() proc.Reader { return proctest.New() },
		Runner:     execRunner{},
		Clock:      wallClock{},
		Exe:        func() (string, error) { return "/opt/kwak/bin/kwak", nil },
		LogPath:    func() (string, error) { return "/var/log/kwak.log", nil },
		Detach:     func(exe string) error { r.detached = append(r.detached, exe); return nil },
		Supervise: func(ctx context.Context, exe, logPath string) int {
			if ctx == nil {
				t.Error("Supervise got no context")
			}
			r.superv = append(r.superv, exe+" "+logPath)
			return 0
		},
		Listen: func(cfg cli.ListenConfig, d listener.Deps) cli.Main {
			r.listened = append(r.listened, cfg)
			r.deps = append(r.deps, d)
			return func(args []string, _ io.Reader, _, _ io.Writer) int {
				if args != nil {
					t.Errorf("cli.Listen's Main got args %q", args)
				}
				return 0
			}
		},
	}
	return r
}

func (r *rig) run(stdin string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb, r.sys)
	return code, out.String(), errb.String()
}

func TestRun(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		code      int
		stdout    string
		stdoutHas string
		stderrHas []string
	}{
		{name: "no args", code: 2, stderrHas: []string{"usage: kwak <command>", "  version\n", "  listen\n"}},
		{name: "unknown", args: []string{"nope"}, code: 2, stderrHas: []string{`unknown command "nope"`, "usage: kwak <command>"}},
		{name: "version", args: []string{"version"}, code: 0, stdout: "kwak " + wire.Version + "\n"},
		{name: "version extra", args: []string{"version", "x"}, code: 2, stderrHas: []string{"usage: kwak version"}},
		{name: "help", args: []string{"help"}, code: 0, stdoutHas: "  doctor\n"},
		{name: "--help", args: []string{"--help"}, code: 0, stdoutHas: "usage: kwak <command>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, errs := newRig(t).run("", c.args...)
			if code != c.code {
				t.Errorf("code = %d, want %d", code, c.code)
			}
			if c.stdoutHas == "" && out != c.stdout {
				t.Errorf("stdout = %q, want %q", out, c.stdout)
			}
			if c.stdoutHas != "" && !strings.Contains(out, c.stdoutHas) {
				t.Errorf("stdout %q lacks %q", out, c.stdoutHas)
			}
			for _, s := range c.stderrHas {
				if !strings.Contains(errs, s) {
					t.Errorf("stderr %q lacks %q", errs, s)
				}
			}
			if len(c.stderrHas) == 0 && errs != "" {
				t.Errorf("stderr = %q, want empty", errs)
			}
		})
	}
}

func TestUsageSorted(t *testing.T) {
	v := sys{}.version
	cmds := map[string]Main{"b": v, "a": v, "c": v}
	var b bytes.Buffer
	usage(&b, cmds)
	want := "usage: kwak <command> [args]\ncommands:\n  a\n  b\n  c\n"
	if b.String() != want {
		t.Fatalf("usage = %q, want %q", b.String(), want)
	}
}

// The table of spec 5.13, no more and no less.
func TestCommandsMatchSpec(t *testing.T) {
	want := []string{"config", "doctor", "hook", "inbox", "install", "listen", "mcp", "pair",
		"setup", "status", "uninstall", "unpair", "version"}
	var got []string
	for n := range commands(newRig(t).sys) {
		got = append(got, n)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

// I2: hook and mcp only use the socket. Building the table builds nothing either.
func TestAdaptersBuildNoKeyringServiceOrHTTP(t *testing.T) {
	r := newRig(t)
	_ = commands(r.sys)
	if code, out, _ := r.run(`{"session_id":"x"}`, "hook", "claude", "prompt"); code != 0 || out != "" {
		t.Errorf("hook: exit %d, stdout %q", code, out)
	}
	if code, out, errs := r.run("", "mcp", "--harness", "codex"); code != 0 || out != "" {
		t.Errorf("mcp: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if code, _, errs := r.run("", "mcp"); code != 2 || !strings.Contains(errs, "usage: kwak mcp") {
		t.Errorf("mcp without harness: exit %d, %q", code, errs)
	}
	if code, out, _ := r.run("", "setup", "claude"); code != 0 || !strings.Contains(out, "claude plugin install kwak@kwak") {
		t.Errorf("setup claude: exit %d, %q", code, out)
	}
	if len(r.built) != 0 {
		t.Errorf("built OS pieces: %v", r.built)
	}
}

func TestBareSetupIsTheCLI(t *testing.T) {
	r := newRig(t)
	code, _, errs := r.run("", "setup", "--nope")
	if code != 2 || !strings.Contains(errs, "usage: kwak setup [--owner") {
		t.Fatalf("exit %d, %q", code, errs)
	}
}

func TestCLIEnvDefaults(t *testing.T) {
	r := newRig(t)
	if env := r.sys.cliEnv(); env.Server != "" || env.Owner != "" {
		t.Errorf("defaults with the variables unset: %q %q", env.Server, env.Owner)
	}
	t.Setenv(cli.EnvServer, "https://kwak.test")
	t.Setenv(cli.EnvOwner, "alice")
	if env := r.sys.cliEnv(); env.Server != "https://kwak.test" || env.Owner != "alice" {
		t.Errorf("defaults %q %q", env.Server, env.Owner)
	}
}

func TestPairRefusesWithoutTerminal(t *testing.T) {
	r := newRig(t)
	code, _, errs := r.run("", "pair", "--owner", "alice", "--server", "https://kwak.test")
	if code != 1 || !strings.Contains(errs, "needs a terminal") {
		t.Fatalf("exit %d, %q", code, errs)
	}
	if r.built["preflight"] != 0 {
		t.Error("pair checked the bus before the terminal")
	}
}

func TestStatusWithoutListener(t *testing.T) {
	code, _, errs := newRig(t).run("", "status")
	if code != 1 || !strings.Contains(errs, "kwak listener is not running") {
		t.Fatalf("exit %d, %q", code, errs)
	}
}

func TestDoctorWithoutListener(t *testing.T) {
	r := newRig(t)
	code, out, _ := r.run("", "doctor")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	for _, s := range []string{
		"ok    bus:",
		"FAIL  config: " + setup.TextNotPaired,
		"skip  keyring: not checked, because the machine is not paired",
		"FAIL  socket: " + setup.TextNotRunning,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("doctor output lacks %q:\n%s", s, out)
		}
	}
	if r.built["preflight"] != 1 {
		t.Errorf("preflight ran %d times", r.built["preflight"])
	}
}

func TestInstallAndUninstall(t *testing.T) {
	r := newRig(t)
	code, out, errs := r.run("", "install")
	if code != 0 || !strings.Contains(out, "/opt/kwak/bin/kwak") {
		t.Fatalf("install: exit %d, %q %q", code, out, errs)
	}
	unit, err := os.ReadFile(filepath.Join(r.home, ".config", "systemd", "user", service.UnitName))
	if err != nil || !strings.Contains(string(unit), "ExecStart=/opt/kwak/bin/kwak listen\n") {
		t.Fatalf("unit %q, %v", unit, err)
	}
	if calls := r.rec.Calls(); !slices.Contains(calls, servicetest.Line("systemctl", "--user", "daemon-reload")) {
		t.Errorf("calls %q", calls)
	}
	if code, out, errs := r.run("", "uninstall"); code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("uninstall: exit %d, %q %q", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(r.home, ".config", "systemd", "user", service.UnitName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit still there: %v", err)
	}
	if code, _, _ := r.run("", "install", "extra"); code != 2 {
		t.Errorf("install extra: exit %d", code)
	}
}

func TestListenFlags(t *testing.T) {
	cases := []struct {
		name               string
		args               []string
		code               int
		listen, det, super int
		stderrHas          string
	}{
		{name: "bare", code: 0, listen: 1},
		{name: "detach", args: []string{"--detach"}, code: 0, det: 1},
		{name: "detach single dash", args: []string{"-detach"}, code: 0, det: 1},
		{name: "supervise", args: []string{"--supervise"}, code: 0, super: 1},
		{name: "both", args: []string{"--detach", "--supervise"}, code: 2, stderrHas: "usage: kwak listen"},
		{name: "stray arg", args: []string{"now"}, code: 2, stderrHas: "usage: kwak listen"},
		{name: "unknown flag", args: []string{"--foreground"}, code: 2, stderrHas: "usage: kwak listen"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			code, _, errs := r.run("", append([]string{"listen"}, c.args...)...)
			if code != c.code {
				t.Errorf("exit %d, want %d (%s)", code, c.code, errs)
			}
			if len(r.listened) != c.listen || len(r.detached) != c.det || len(r.superv) != c.super {
				t.Errorf("listen %d, detach %v, supervise %v", len(r.listened), r.detached, r.superv)
			}
			if c.det == 1 && r.detached[0] != "/opt/kwak/bin/kwak" {
				t.Errorf("detach exe %q", r.detached[0])
			}
			if c.super == 1 && r.superv[0] != "/opt/kwak/bin/kwak /var/log/kwak.log" {
				t.Errorf("supervise %q", r.superv[0])
			}
			if !strings.Contains(errs, c.stderrHas) {
				t.Errorf("stderr %q lacks %q", errs, c.stderrHas)
			}
		})
	}
}

func TestListenDetachFails(t *testing.T) {
	r := newRig(t)
	r.Detach = func(string) error { return errors.New("no console") }
	if code, _, errs := r.run("", "listen", "--detach"); code != 1 || !strings.Contains(errs, "no console") {
		t.Fatalf("exit %d, %q", code, errs)
	}
}

func TestListenDeps(t *testing.T) {
	r := newRig(t)
	if code, _, errs := r.run("", "listen"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	cfg, d := r.listened[0], r.deps[0]
	if want := filepath.Join(os.Getenv(paths.EnvConfigDir), cli.ConfigFile); cfg.Listener.ConfigPath != want {
		t.Errorf("config path %q, want %q", cfg.Listener.ConfigPath, want)
	}
	if cfg.Preflight == nil || cfg.HTTP != matrix.Doer(r.doer) {
		t.Errorf("listen config %+v", cfg)
	}
	if d.Registrar != nil {
		t.Errorf("Registrar = %#v, want nil so cli.Listen picks ListenerRegistrar", d.Registrar)
	}
	if d.Secrets != r.keyring || d.Matrix == nil || d.Procs == nil || d.PeerPID == nil ||
		d.Runner == nil || d.Clock == nil || d.Log == nil {
		t.Errorf("deps %+v", d)
	}
}

// The wired Deps start a real, unpaired listener that answers on the socket and shuts down.
func TestListenRealUnpaired(t *testing.T) {
	r := newRig(t)
	r.Listen = cli.Listen
	short, err := os.MkdirTemp("", "kwcmd") // unix socket paths are short on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	t.Setenv(paths.EnvSocket, filepath.Join(short, "run", "kwak.sock"))
	done := make(chan int, 1)
	var errb bytes.Buffer
	go func() { done <- run([]string{"listen"}, strings.NewReader(""), io.Discard, &errb, r.sys) }()

	call := cli.SocketCaller(os.Getenv(paths.EnvSocket))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var st wire.StatusResponse
	for call(ctx, wire.Head{V: wire.ProtocolVersion, Op: wire.OpStatus}, &st) != nil {
		if ctx.Err() != nil {
			t.Fatalf("listener never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !st.OK || st.State != wire.StateUnpaired || st.Kwak != wire.Version {
		t.Errorf("status %+v", st)
	}
	var ack wire.Ack
	if err := call(ctx, wire.Head{V: wire.ProtocolVersion, Op: wire.OpShutdown}, &ack); err != nil || !ack.OK {
		t.Fatalf("shutdown: %v %+v", err, ack)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d: %s", code, errb.String())
		}
	case <-ctx.Done():
		t.Fatal("listener did not stop")
	}
	if r.built["preflight"] != 1 {
		t.Errorf("preflight ran %d times", r.built["preflight"])
	}
}

func TestMatrixFactory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, cli.ConfigFile)
	doer := refuseDoer{t}
	m := matrixFactory(path, doer)

	c, ok := m("tok-1").(*matrix.Client)
	if !ok || c.Base != "" || c.Token != "tok-1" {
		t.Fatalf("unpaired client %+v", c)
	}
	if _, err := c.Sync(context.Background(), "", "", 0); err == nil {
		t.Error("a client with no server synced")
	}

	conf := `{"version":1,"server":"https://kwak.test","server_name":"kwak.test","owner":"alice"}`
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	c = m("tok-2").(*matrix.Client)
	if c.Base != "https://kwak.test" || c.Token != "tok-2" || c.AllowHTTP || c.HTTP != matrix.Doer(doer) || c.AsUser != "" {
		t.Errorf("paired client %+v", c)
	}

	if err := os.WriteFile(path, []byte(`{"version":1,"server":"http://kwak.test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := m("tok-3").(*matrix.Client); c.Base != "" {
		t.Errorf("plain http server accepted: %q", c.Base)
	}
}

func TestExecRunner(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv(childEnv, "pwd")
	var log bytes.Buffer
	wait, err := execRunner{}.Start(context.Background(), dir, []string{exe, "hello"}, &log)
	if err != nil {
		t.Fatal(err)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	got := strings.TrimSpace(log.String())
	if got != dir+" hello" && got != real+" hello" {
		t.Errorf("child said %q, want %q", got, dir+" hello")
	}

	if _, err := (execRunner{}).Start(context.Background(), dir, nil, &log); err == nil {
		t.Error("empty argv started")
	}
}

func TestExecRunnerCancelKills(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(childEnv, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	wait, err := execRunner{}.Start(ctx, t.TempDir(), []string{exe}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a killed run reported success")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the run outlived its context")
	}
}

func TestIsTerminalOnFileAndPipe(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	for _, x := range []*os.File{f, pr, pw} {
		if isTerminal(x) {
			t.Errorf("%s is a terminal", x.Name())
		}
	}
}

func TestWallClock(t *testing.T) {
	var c listener.Clock = wallClock{}
	before := time.Now()
	<-c.After(time.Millisecond)
	if !c.Now().After(before) {
		t.Error("the clock did not move")
	}
}

func TestPTYPipeNames(t *testing.T) {
	for name, want := range map[string]bool{
		`\msys-1888ae32e00d56aa-pty0-from-master`:                true,
		`\cygwin-e022582115c10879-pty4-to-master`:                true,
		`\Device\NamedPipe\msys-dd50a72ab4668b33-pty2-to-master`: true,
		`\msys-1888ae32e00d56aa-pty0-from-master-nat`:            true,
		`\msys-1888ae32e00d56aa-pipe0-from-master`:               false,
		`\Winsock2\CatalogChangeListener-2a4-0`:                  false,
		`\claude-bash-tool-stdout`:                               false,
		`\msys--pty0-from-master`:                                false,
	} {
		if got := isPTYPipeName(name); got != want {
			t.Errorf("isPTYPipeName(%q) = %v", name, got)
		}
	}
}

// A dev build (make dev) says so in kwak version, and never touches the user service: install,
// uninstall and bare setup refuse, and no service manager is even built (decision 40).
func TestDevBuild(t *testing.T) {
	r := newRig(t)
	r.Dev = true
	code, out, _ := r.run("", "version")
	if code != 0 || !strings.HasPrefix(out, "kwak "+wire.Version+" (dev build)\n") ||
		!strings.Contains(out, "  config: "+os.Getenv(paths.EnvConfigDir)+"\n") || !strings.Contains(out, "  socket: ") {
		t.Fatalf("version: exit %d:\n%s", code, out)
	}
	for _, args := range [][]string{{"install"}, {"uninstall"}} {
		code, _, errs := r.run("", args...)
		if code != 1 || !strings.Contains(errs, "a dev build never touches the kwak user service") {
			t.Errorf("%v: exit %d: %s", args, code, errs)
		}
	}
	if code, _, errs := r.run("", "setup", "--owner", "fcanela"); code != 1 || !strings.Contains(errs, "bare kwak setup installs the user service") {
		t.Errorf("bare setup: exit %d: %s", code, errs)
	}
	if r.built["service"] != 0 || len(r.rec.Calls()) != 0 {
		t.Errorf("a dev build built a service manager (%d) or ran %v", r.built["service"], r.rec.Calls())
	}

	// A release build still installs.
	r = newRig(t)
	if code, out, errs := r.run("", "install"); code != 0 || !strings.Contains(out, "runs as a user service") {
		t.Fatalf("release install: exit %d: %s %s", code, out, errs)
	}
}
