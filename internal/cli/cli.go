// Package cli holds the human commands: bare setup, pair, unpair, status, inbox and config
// (spec 4, 5.13), and Listen, which runs the daemon. The OS parts come in through Env.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/paths"
	"github.com/fcanela/kwak/internal/regclient"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/wire"
)

// Main is the shape of every kwak subcommand.
type Main func(args []string, stdin io.Reader, stdout, stderr io.Writer) int

// Service is the user service manager (service.Manager satisfies it).
type Service interface {
	Install(exe string) error
	Uninstall() error
	Start() error
	Stop() error
	Status() (string, error)
}

// Caller sends one IPC request line to the listener and decodes its one reply line into reply.
type Caller func(ctx context.Context, req, reply any) error

// Env carries the OS implementations. The first six are required; a nil optional field means the
// real thing.
type Env struct {
	Secrets      secrets.Secrets
	Service      Service
	Preflight    func() error // oskeyring.Preflight
	Call         Caller
	PrintHarness func(io.Writer, wire.Harness) error // setup.Print
	IsTerminal   func() bool                         // stdin and stdout are both a terminal

	// Server and Owner are the first-pairing defaults when --server and --owner are not given.
	// kwak reads them from EnvServer and EnvOwner, so whoever installs kwak (home-manager, say)
	// can set them. kwak itself has no built-in server. A paired machine ignores both: its
	// config.json already names its server and owner.
	Server string
	Owner  string

	HTTP       matrix.Doer                  // nil: matrix.NewHTTPClient()
	ConfigDir  func() (string, error)       // nil: paths.ConfigDir
	StateDir   func() (string, error)       // nil: paths.StateDir
	Hostname   func() (string, error)       // nil: os.Hostname
	Executable func() (string, error)       // nil: paths.Executable
	LookPath   func(string) (string, error) // nil: exec.LookPath
	Now        func() time.Time             // nil: time.Now
	Sleep      func(context.Context, time.Duration) error

	// InsecureHTTPForTests lets the registrar URL be http://. Production never sets it (spec 2.1).
	InsecureHTTPForTests bool
}

// The environment variables behind Env.Server and Env.Owner.
const (
	EnvServer = "KWAK_SERVER"
	EnvOwner  = "KWAK_OWNER"
)

// CLI holds the human commands. Each method is a Main.
type CLI struct {
	env Env
}

// New returns the human commands over env.
func New(env Env) *CLI {
	if env.HTTP == nil {
		env.HTTP = matrix.NewHTTPClient()
	}
	if env.ConfigDir == nil {
		env.ConfigDir = paths.ConfigDir
	}
	if env.StateDir == nil {
		env.StateDir = paths.StateDir
	}
	if env.Hostname == nil {
		env.Hostname = os.Hostname
	}
	if env.Executable == nil {
		env.Executable = executable
	}
	if env.LookPath == nil {
		env.LookPath = exec.LookPath
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Sleep == nil {
		env.Sleep = sleep
	}
	return &CLI{env: env}
}

func executable() (string, error) { return paths.Executable() }

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *CLI) registrar(server string, key func() (string, error)) *regclient.Client {
	return &regclient.Client{Base: server, Key: key, HTTP: c.env.HTTP, InsecureHTTPForTests: c.env.InsecureHTTPForTests}
}

func fixedKey(k string) func() (string, error) { return func() (string, error) { return k, nil } }

// Deadlines of IPC calls. reload waits for the listener's round trip to the registrar.
const (
	callWait   = 5 * time.Second
	reloadWait = 30 * time.Second
)

// call asks the listener with a short deadline.
func (c *CLI) call(ctx context.Context, op string, reply any) error {
	return c.callWait(ctx, op, reply, callWait)
}

func (c *CLI) callWait(ctx context.Context, op string, reply any, d time.Duration) error {
	if c.env.Call == nil {
		return errors.New("no listener caller")
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return c.env.Call(ctx, wire.Head{V: wire.ProtocolVersion, Op: op}, reply)
}

const listenerDown = wire.TextListenerDown

func fail(w io.Writer, format string, a ...any) int {
	fmt.Fprintf(w, "kwak: "+format+"\n", a...)
	return 1
}
