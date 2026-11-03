// Package hook is `kwak hook <harness> <event>`: it reads the harness's hook stdin, asks the
// listener over the local socket, and prints the harness's JSON. It always exits 0, prints 0
// bytes when there is nothing to say, and prints nothing when the listener is down (spec 5.6,
// 6.2, 6.3). It never opens the keyring or the network (I2).
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/paths"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/wire"
)

// Client timeouts (spec 5.6).
const (
	DialTimeout     = 300 * time.Millisecond
	ExchangeTimeout = 3 * time.Second
	EndTimeout      = 1 * time.Second
)

// MaxStdin is the most hook stdin read. A larger stdin prints nothing; its items wait.
const MaxStdin = 16 << 20

// Caller sends one hook request to the listener and returns its answer.
type Caller func(ctx context.Context, req wire.HookRequest) (wire.HookResponse, error)

// MainFunc is the shape of a kwak subcommand.
type MainFunc func(args []string, stdin io.Reader, stdout, stderr io.Writer) int

// Run handles one hook call. It always returns 0. chain may be nil; it is set only on Windows.
func Run(h wire.Harness, event string, stdin io.Reader, stdout io.Writer, call Caller, chain func() []proc.ID) int {
	data, err := io.ReadAll(io.LimitReader(stdin, MaxStdin+1))
	if err != nil || len(data) > MaxStdin {
		return 0
	}
	req, hookEvent, err := wire.ParseHookStdin(h, event, data)
	if err != nil {
		return 0
	}
	if chain != nil {
		req.Chain = chain()
	}
	timeout := ExchangeTimeout
	if event == wire.EventSessionEnd {
		timeout = EndTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := call(ctx, req)
	if err != nil || !resp.OK || resp.Text == "" {
		return 0
	}
	if out := Output(h, event, hookEvent, req.StopHookActive, resp.Text); out != nil {
		stdout.Write(out)
	}
	return 0
}

type specific struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

type contextOutput struct {
	HookSpecificOutput specific `json:"hookSpecificOutput"`
}

type blockOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// Output is what the hook prints for the listener's text, or nil. hookEvent is stdin's
// hook_event_name; only Claude's post-tool reads it, to tell PostToolUseFailure apart.
func Output(h wire.Harness, event, hookEvent string, stopActive bool, text string) []byte {
	if text == "" {
		return nil
	}
	var v any
	switch event {
	case wire.EventSessionStart:
		v = contextOutput{specific{"SessionStart", text}}
	case wire.EventPrompt:
		v = contextOutput{specific{"UserPromptSubmit", text}}
	case wire.EventPostTool:
		name := "PostToolUse"
		if h == wire.HarnessClaude && hookEvent == "PostToolUseFailure" {
			name = hookEvent
		}
		v = contextOutput{specific{name, text}}
	case wire.EventStop:
		if stopActive {
			return nil
		}
		if h == wire.HarnessCodex {
			v = blockOutput{"block", text}
		} else {
			v = contextOutput{specific{"Stop", text}}
		}
	default:
		return nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return nil
	}
	return b.Bytes()
}

// Dial returns the Caller that talks to the listener at socket: one request and one reply per
// connection.
func Dial(socket string) Caller {
	return func(ctx context.Context, req wire.HookRequest) (wire.HookResponse, error) {
		var resp wire.HookResponse
		err := ipc.Call(ctx, socket, DialTimeout, req, &resp)
		return resp, err
	}
}

// NewMain returns `kwak hook <claude|codex> <event>`. procs is read only on Windows.
func NewMain(procs proc.Reader) MainFunc {
	chain := proc.ClientChain(runtime.GOOS, procs)
	return func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
		return mainWith(args, stdin, stdout, stderr, paths.SocketPath, chain)
	}
}

func mainWith(args []string, stdin io.Reader, stdout, stderr io.Writer, socket func() (string, error), chain func() []proc.ID) int {
	if len(args) != 2 || !wire.ValidHarness(wire.Harness(args[0])) || !wire.ValidEvent(args[1]) {
		fmt.Fprintln(stderr, "usage: kwak hook <claude|codex> <session-start|prompt|post-tool|stop|session-end|subagent-stop>")
		return 0
	}
	path, err := socket()
	call := Dial(path)
	if err != nil {
		call = func(context.Context, wire.HookRequest) (wire.HookResponse, error) {
			return wire.HookResponse{}, err
		}
	}
	return Run(wire.Harness(args[0]), args[1], stdin, stdout, call, chain)
}
