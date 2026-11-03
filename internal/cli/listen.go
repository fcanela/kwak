package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/listener"
	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/paths"
	"github.com/fcanela/kwak/internal/regclient"
)

// ListenConfig is what kwak listen needs besides the listener's Deps. Empty paths mean the
// paths package defaults.
type ListenConfig struct {
	Listener  listener.Config // ConfigPath "": <config dir>/config.json
	StateDir  string
	Socket    string
	Policy    ipc.Policy
	Preflight func() error // the Linux session bus check (spec 5.1 step 3); nil skips it
	HTTP      matrix.Doer  // for the registrar when Deps.Registrar is nil; nil: matrix.NewHTTPClient()
}

// ListenerRegistrar is the registrar client of kwak listen. It reads the server from config.json
// at configPath for every request, so kwak pair under a running listener needs no restart. The
// listener gives it the key (listener.KeySetter).
func ListenerRegistrar(configPath string, http matrix.Doer) *regclient.Client {
	return &regclient.Client{HTTP: http, BaseFunc: func() (string, error) {
		cfg, err := loadConfigFile(configPath, false)
		if err != nil {
			return "", err
		}
		return cfg.Server, nil
	}}
}

// Listen returns the Main of kwak listen (spec 5.1): take listen.lock, check the session bus,
// remove a stale socket, listen, and run the listener until SIGTERM, SIGINT or the shutdown op.
// P6 parses listen's flags; this Main takes none. A nil d.Registrar gets ListenerRegistrar.
func Listen(cfg ListenConfig, d listener.Deps) Main {
	return func(args []string, _ io.Reader, _, stderr io.Writer) int {
		if len(args) != 0 {
			fmt.Fprintln(stderr, "usage: kwak listen")
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		code, err := listen(ctx, cfg, d, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "kwak listen: %v\n", err)
		}
		return code
	}
}

func listen(ctx context.Context, cfg ListenConfig, d listener.Deps, stderr io.Writer) (int, error) {
	var err error
	if cfg.StateDir == "" {
		if cfg.StateDir, err = paths.StateDir(); err != nil {
			return 1, err
		}
	}
	if cfg.Socket == "" {
		if cfg.Socket, err = paths.SocketPath(); err != nil {
			return 1, err
		}
	}
	if cfg.Listener.ConfigPath == "" {
		dir, err := paths.ConfigDir()
		if err != nil {
			return 1, err
		}
		cfg.Listener.ConfigPath = filepath.Join(dir, ConfigFile)
	}
	if d.Registrar == nil {
		d.Registrar = ListenerRegistrar(cfg.Listener.ConfigPath, cfg.HTTP)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return 1, err
	}
	release, err := ipc.Lock(filepath.Join(cfg.StateDir, "listen.lock"))
	if errors.Is(err, ipc.ErrLocked) {
		fmt.Fprintln(stderr, "kwak listen: already running")
		return 0, nil
	}
	if err != nil {
		return 1, err
	}
	defer release()
	if cfg.Preflight != nil {
		if err := cfg.Preflight(); err != nil {
			return 1, err
		}
	}
	if err := ipc.RemoveStale(cfg.Socket); err != nil {
		return 1, err
	}
	ln, err := ipc.Listen(cfg.Socket, cfg.Policy)
	if err != nil {
		return 1, err
	}
	defer os.Remove(cfg.Socket)
	defer ln.Close()
	l, err := listener.New(cfg.Listener, cfg.StateDir, d)
	if err != nil {
		return 1, err
	}
	if err := l.Run(ctx, ln); err != nil && !errors.Is(err, context.Canceled) {
		return 1, err
	}
	return 0, nil
}
