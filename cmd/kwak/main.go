// Command kwak is the listener, the harness hooks, the MCP server and the human commands.
// This file only dispatches and builds the real implementations each package takes (spec 2.1).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/fcanela/kwak/internal/check"
	"github.com/fcanela/kwak/internal/cli"
	"github.com/fcanela/kwak/internal/hook"
	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/listener"
	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/mcp"
	"github.com/fcanela/kwak/internal/paths"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/proc/osproc"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/secrets/oskeyring"
	"github.com/fcanela/kwak/internal/service"
	"github.com/fcanela/kwak/internal/setup"
	"github.com/fcanela/kwak/internal/wire"
)

// Main is the shape every subcommand exposes.
type Main func(args []string, stdin io.Reader, stdout, stderr io.Writer) int

// keyringDeadline bounds every keyring call (spec 5.1 step 4).
const keyringDeadline = 10 * time.Second

// sys is the outside world the commands are built from. Each field is called only by the
// commands that need it, so kwak hook and kwak mcp never build a keyring, a service or an
// HTTP client (I2).
type sys struct {
	Keyring    func() secrets.Secrets
	Preflight  func() error
	Service    func() service.Manager
	HTTP       func() matrix.Doer
	IsTerminal func() bool
	Procs      func() proc.Reader
	Runner     listener.Runner
	Clock      listener.Clock
	Exe        func() (string, error)
	LogPath    func() (string, error)
	Detach     func(exe string) error
	Supervise  func(ctx context.Context, exe, logPath string) int
	Listen     func(cli.ListenConfig, listener.Deps) cli.Main
	// Dev is a dev build (make dev, paths.Dev): its own folders, no user service, a note in
	// kwak version and kwak status (decision 40).
	Dev bool
}

func realSys() sys {
	return sys{
		Keyring:   func() secrets.Secrets { return oskeyring.New(secrets.Service, keyringDeadline) },
		Preflight: oskeyring.Preflight,
		Service: func() service.Manager {
			home, _ := os.UserHomeDir()
			return service.ForOS(service.ExecRunner{}, home)
		},
		HTTP:       func() matrix.Doer { return matrix.NewHTTPClient() },
		IsTerminal: func() bool { return isTerminal(os.Stdin) && isTerminal(os.Stdout) },
		Procs:      osproc.New,
		Runner:     execRunner{},
		Clock:      wallClock{},
		Exe:        service.Executable,
		LogPath:    service.LogPath,
		Detach:     service.Detach,
		Supervise:  service.Supervise,
		Listen:     cli.Listen,
		Dev:        paths.Dev(),
	}
}

// service is the user service manager. A dev build gets one that refuses everything, so it can
// never install, start, stop or remove the real listener's service under the real service name.
func (s sys) service() service.Manager {
	if s.Dev {
		return devService{}
	}
	return s.Service()
}

// errDevService is what every service command of a dev build answers.
var errDevService = errors.New("this is a dev build, and a dev build never touches the kwak user service " +
	"(it would replace your real listener). Run the dev listener in a terminal: kwak listen (see docs/development.md)")

type devService struct{}

func (devService) Install(string) error    { return errDevService }
func (devService) Uninstall() error        { return errDevService }
func (devService) Start() error            { return errDevService }
func (devService) Stop() error             { return errDevService }
func (devService) Status() (string, error) { return "", errDevService }

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, realSys()))
}

// commands is the dispatch table of spec 5.13.
func commands(s sys) map[string]Main {
	human := func(pick func(*cli.CLI) cli.Main) Main {
		return func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
			return pick(cli.New(s.cliEnv()))(args, stdin, stdout, stderr)
		}
	}
	svc := func(pick func(service.Commands) cli.Main) Main {
		return func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
			return pick(service.Commands{Manager: s.service(), Exe: s.Exe})(args, stdin, stdout, stderr)
		}
	}
	return map[string]Main{
		"setup": func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
			if len(args) > 0 && wire.ValidHarness(wire.Harness(args[0])) {
				if s.Dev {
					return setup.DevMain(args, stdin, stdout, stderr)
				}
				return setup.Main(args, stdin, stdout, stderr)
			}
			return human(func(c *cli.CLI) cli.Main { return c.Setup })(args, stdin, stdout, stderr)
		},
		"pair":      human(func(c *cli.CLI) cli.Main { return c.Pair }),
		"unpair":    human(func(c *cli.CLI) cli.Main { return c.Unpair }),
		"status":    human(func(c *cli.CLI) cli.Main { return c.Status }),
		"inbox":     human(func(c *cli.CLI) cli.Main { return c.Inbox }),
		"config":    human(func(c *cli.CLI) cli.Main { return c.Config }),
		"install":   svc(func(c service.Commands) cli.Main { return c.Install }),
		"uninstall": svc(func(c service.Commands) cli.Main { return c.Uninstall }),
		"listen":    s.listen,
		"doctor": func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
			d := setup.DoctorDeps{Preflight: s.Preflight, Secrets: s.Keyring(), Call: socketCaller(), Dev: s.Dev,
				Service: s.service().Status}
			return setup.Doctor(d)(args, stdin, stdout, stderr)
		},
		"hook": func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
			return hook.NewMain(s.Procs())(args, stdin, stdout, stderr)
		},
		"mcp": func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
			return mcp.NewMain(s.Procs())(args, stdin, stdout, stderr)
		},
		"version": s.version,
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, s sys) int {
	cmds := commands(s)
	if len(args) == 0 {
		usage(stderr, cmds)
		return 2
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		usage(stdout, cmds)
		return 0
	}
	m, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "kwak: unknown command %q\n", args[0])
		usage(stderr, cmds)
		return 2
	}
	return m(args[1:], stdin, stdout, stderr)
}

func usage(w io.Writer, cmds map[string]Main) {
	names := make([]string, 0, len(cmds))
	for n := range cmds {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintln(w, "usage: kwak <command> [args]")
	fmt.Fprintln(w, "commands:")
	for _, n := range names {
		fmt.Fprintf(w, "  %s\n", n)
	}
}

// version is kwak version. A dev build says so on the same line, then names its folders.
func (s sys) version(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: kwak version")
		return 2
	}
	if !s.Dev {
		fmt.Fprintf(stdout, "kwak %s\n", wire.Version)
		return 0
	}
	fmt.Fprintf(stdout, "kwak %s (dev build)\n", wire.Version)
	for _, d := range []struct {
		name string
		path func() (string, error)
	}{{"config", paths.ConfigDir}, {"state", paths.StateDir}, {"socket", paths.SocketPath}} {
		p, err := d.path()
		if err != nil {
			p = err.Error()
		}
		fmt.Fprintf(stdout, "  %s: %s\n", d.name, p)
	}
	return 0
}

func (s sys) cliEnv() cli.Env {
	return cli.Env{
		Secrets:      s.Keyring(),
		Service:      s.service(),
		Preflight:    s.Preflight,
		Call:         socketCaller(),
		PrintHarness: setup.Print,
		IsTerminal:   s.IsTerminal,
		HTTP:         s.HTTP(),
		Server:       os.Getenv(cli.EnvServer),
		Owner:        os.Getenv(cli.EnvOwner),
		Dev:          s.Dev,
	}
}

func socketCaller() cli.Caller {
	path, err := paths.SocketPath()
	if err != nil {
		return func(context.Context, any, any) error { return err }
	}
	return cli.SocketCaller(path)
}

// listen is kwak listen [--detach | --supervise] (spec 5.1, 7.3).
func (s sys) listen(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kwak listen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: kwak listen [--detach | --supervise]") }
	detach := fs.Bool("detach", false, "start kwak listen --supervise with no console, then return")
	supervise := fs.Bool("supervise", false, "run the listener as a child and restart it after a failure")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || (*detach && *supervise) {
		fs.Usage()
		return 2
	}
	if !*detach && !*supervise {
		cfg, d := s.listenDeps(stderr)
		return s.Listen(cfg, d)(nil, stdin, stdout, stderr)
	}
	exe, err := s.Exe()
	if err != nil {
		fmt.Fprintf(stderr, "kwak listen: cannot find the kwak binary: %v\n", err)
		return 1
	}
	if *detach {
		if err := s.Detach(exe); err != nil {
			fmt.Fprintf(stderr, "kwak listen --detach: %v\n", err)
			return 1
		}
		return 0
	}
	logPath, err := s.LogPath()
	if err != nil {
		fmt.Fprintf(stderr, "kwak listen --supervise: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.Supervise(ctx, exe, logPath)
}

// listenDeps builds the daemon's config and Deps. Registrar stays nil, so cli.Listen gives it
// cli.ListenerRegistrar over the same config path and HTTP client.
func (s sys) listenDeps(stderr io.Writer) (cli.ListenConfig, listener.Deps) {
	configPath := ""
	if dir, err := paths.ConfigDir(); err == nil {
		configPath = filepath.Join(dir, cli.ConfigFile)
	}
	http := s.HTTP()
	cfg := cli.ListenConfig{
		Listener:  listener.Config{ConfigPath: configPath},
		Preflight: s.Preflight,
		HTTP:      http,
	}
	d := listener.Deps{
		Secrets: s.Keyring(),
		Matrix:  matrixFactory(configPath, http),
		Procs:   s.Procs(),
		PeerPID: ipc.PeerPID,
		Runner:  s.Runner,
		Check:   check.Exec{},
		Clock:   s.Clock,
		Log:     slog.New(slog.NewTextHandler(stderr, nil)),
	}
	return cfg, d
}

// matrixFactory gives the listener one client per token. The base is the server in config.json,
// read at each call, so a first kwak pair under a running listener needs no restart.
func matrixFactory(configPath string, http matrix.Doer) func(string) listener.Matrix {
	return func(token string) listener.Matrix {
		base := ""
		if c, err := listener.ReadMachineConfig(configPath); err == nil {
			base = c.Server
		}
		return &matrix.Client{Base: base, Token: token, HTTP: http}
	}
}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// execRunner starts a background resume (spec 5.11). Cancelling ctx stops the run and its
// children: SIGTERM, then SIGKILL after Grace (5 s when zero). wait returns once they are gone.
type execRunner struct {
	Grace time.Duration
}

const runGrace = 5 * time.Second

func (r execRunner) Start(ctx context.Context, dir string, argv []string, log io.Writer) (func() error, error) {
	if len(argv) == 0 {
		return nil, errors.New("kwak: empty command")
	}
	grace := r.Grace
	if grace <= 0 {
		grace = runGrace
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.WaitDelay = grace
	return startTree(cmd, grace)
}
