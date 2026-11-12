package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/fcanela/kwak/internal/wire"
)

// textDevSetup is why bare kwak setup refuses in a dev build.
const textDevSetup = "this is a dev build, and bare kwak setup installs the user service, which a dev build never does. " +
	"Run kwak pair, then kwak listen in a terminal (see docs/development.md)"

// How long bare setup waits for the listener's sync, and how often it asks.
const (
	SetupWait = 30 * time.Second
	setupPoll = 500 * time.Millisecond
)

// Setup is bare kwak setup [--owner <name>] [--server <url>]: pair, install the service, wait for
// the sync, then print the harness steps (spec 5.13).
func (c *CLI) Setup(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kwak setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f pairFlags
	f.register(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: kwak setup [--owner <name>] [--server <url>], or kwak setup claude|codex")
		return 2
	}
	if c.env.Dev {
		// A dev build never installs the user service (decision 40): the service would run
		// this binary under the real service name, in place of the real listener.
		return fail(stderr, "%s", textDevSetup)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if _, err := c.pair(ctx, f, stdin, stdout); err != nil {
		return fail(stderr, "%v", err)
	}
	exe, err := c.env.Executable()
	if err != nil {
		return fail(stderr, "cannot find the kwak binary: %v", err)
	}
	if err := c.env.Service.Install(exe); err != nil {
		return fail(stderr, "could not install the user service: %v. Try kwak install, and kwak doctor", err)
	}
	fmt.Fprintln(stdout, "Installed the listener as a user service. Waiting for it to connect...")
	code := 0
	if st, ok := c.waitSync(ctx); ok {
		fmt.Fprintln(stdout, "The listener is connected.")
	} else {
		code = 1
		why := ""
		if st.Detail != "" {
			why = " (" + st.Detail + ")"
		} else if st.SyncError != "" {
			why = " (" + st.SyncError + ")"
		}
		fmt.Fprintf(stderr, "kwak: the listener did not connect within %d s%s. Check kwak status and kwak doctor.\n",
			int(SetupWait.Seconds()), why)
	}
	for _, h := range []wire.Harness{wire.HarnessClaude, wire.HarnessCodex} {
		fmt.Fprintln(stdout)
		if err := c.env.PrintHarness(stdout, h); err != nil {
			code = fail(stderr, "could not print the %s steps: %v", h, err)
		}
	}
	return code
}

// waitSync polls status until the listener reports a running sync, for up to SetupWait.
func (c *CLI) waitSync(ctx context.Context) (wire.StatusResponse, bool) {
	deadline := c.env.Now().Add(SetupWait)
	var last wire.StatusResponse
	for {
		var st wire.StatusResponse
		if err := c.call(ctx, wire.OpStatus, &st); err == nil && st.OK {
			last = st
			if st.State == wire.StateSyncing {
				return st, true
			}
		}
		if !c.env.Now().Before(deadline) {
			return last, false
		}
		if err := c.env.Sleep(ctx, setupPoll); err != nil {
			return last, false
		}
	}
}
