package mcp

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/paths"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/wire"
)

// EnvClaudeSession is the variable Claude Code gives its MCP servers. It goes stale after
// /clear, so it is only a cross-check (spec 5.7).
const EnvClaudeSession = "CLAUDE_CODE_SESSION_ID"

// MainFunc is the shape of a kwak subcommand.
type MainFunc func(args []string, stdin io.Reader, stdout, stderr io.Writer) int

// NewMain returns `kwak mcp --harness <claude|codex>`. procs is read only on Windows, for the
// chain the client sends. start starts the listener at a send that finds it down (IPC.Start).
func NewMain(procs proc.Reader, start func() error) MainFunc {
	chain := proc.ClientChain(runtime.GOOS, procs)
	return func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
		return mainWith(args, stdin, stdout, stderr, os.Getenv, paths.SocketPath, chain, start)
	}
}

func mainWith(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string,
	socket func() (string, error), chain func() []proc.ID, start func() error) int {
	fs := flag.NewFlagSet("kwak mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	harness := fs.String("harness", "", "claude or codex")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	h := wire.Harness(*harness)
	if !wire.ValidHarness(h) || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: kwak mcp --harness <claude|codex>")
		return 2
	}
	b := &IPC{
		Dial: func(ctx context.Context) (net.Conn, error) {
			path, err := socket()
			if err != nil {
				return nil, err
			}
			return ipc.Dial(ctx, path, nil)
		},
		Hello: func() wire.MCPHello {
			hello := wire.MCPHello{V: wire.ProtocolVersion, Kwak: wire.Version, Op: wire.OpMCPHello, Harness: h}
			if h == wire.HarnessClaude {
				hello.EnvSession = getenv(EnvClaudeSession)
			}
			if chain != nil {
				hello.Chain = chain()
			}
			return hello
		},
		Log:   stderr,
		Start: start,
	}
	defer b.Close()
	if err := Serve(context.Background(), stdin, stdout, Tools(), b); err != nil {
		fmt.Fprintf(stderr, "kwak mcp: %v\n", err)
		return 1
	}
	return 0
}
