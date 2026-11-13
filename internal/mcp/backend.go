package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/wire"
)

// Tool error texts the MCP server writes itself.
const (
	TextNotRunning    = wire.TextListenerDown
	TextListenerOlder = wire.TextListenerOlder
	TextUnpaired      = wire.TextUnpaired
	TextBadInput      = "the kwak listener refused this call: one of its values failed the listener's checks"
	TextBadOp         = "the kwak listener does not know this request; run kwak install to restart it"
	TextInternal      = "the kwak listener failed on this call; see kwak status, or the listener log"
	TextTimeout       = "the kwak listener did not answer in time; try again"
	TextLost          = "the connection to the kwak listener closed during this call; try again"
	TextTooLarge      = "this call is too large for the kwak listener (over 1 MiB)"
	TextStarted       = "kwak started its listener, but it is not answering yet; send again in a moment. Details: kwak doctor"
)

// Timeouts of the IPC backend.
const (
	DialTimeout  = 1 * time.Second
	HelloTimeout = 3 * time.Second
	CallTimeout  = 25 * time.Second // under Codex's tool_timeout_sec of 30
	// StartWait is how long a send waits for a listener it just started to answer, and
	// StartPoll how often it tries in that time. Both stay well under CallTimeout.
	StartWait = 10 * time.Second
	StartPoll = 500 * time.Millisecond
)

var errClosed = errors.New("mcp: listener connection closed")

// IPC is the Backend over one long connection to the listener. It dials on the first call,
// sends mcp_hello first, and dials and says hello again on the next call after any error.
// Replies are matched to calls by id, so they may come in any order.
type IPC struct {
	Dial  func(ctx context.Context) (net.Conn, error)
	Hello func() wire.MCPHello
	Log   io.Writer // connection errors; nil discards
	// Start starts the listener when a send finds it down (decision 46, spec 5.6), at most once
	// per kwak mcp; an error says why it did not. nil never starts it. After is time.After when nil.
	Start func() error
	After func(time.Duration) <-chan time.Time

	mu      sync.Mutex
	conn    net.Conn
	next    int64
	pending map[int64]chan result
	started bool // Start was tried
}

type result struct {
	resp wire.ToolResponse
	err  error
}

func (b *IPC) logf(format string, a ...any) {
	if b.Log != nil {
		fmt.Fprintf(b.Log, "kwak mcp: "+format+"\n", a...)
	}
}

// Call implements Backend.
func (b *IPC) Call(ctx context.Context, tool string, args json.RawMessage, m Meta) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	id, ch, text := b.send(ctx, tool, args, m)
	if ch == nil {
		return text, true
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return TextLost, true
		}
		if !r.resp.OK {
			return errText(r.resp.Err), true
		}
		return r.resp.Text, r.resp.IsError
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return TextTimeout, true
	}
}

func errText(code string) string {
	switch code {
	case wire.IPCErrVersion:
		return TextListenerOlder
	case wire.IPCErrUnpaired:
		return TextUnpaired
	case wire.IPCErrBadInput:
		return TextBadInput
	case wire.IPCErrBadOp:
		return TextBadOp
	}
	return TextInternal
}

// send writes one tool request. It returns the reply channel, or nil and the tool error text.
func (b *IPC) send(ctx context.Context, tool string, args json.RawMessage, m Meta) (int64, chan result, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		text := b.connect(ctx)
		if text == TextNotRunning && tool == wire.ToolSend {
			text = b.startLocked(ctx)
		}
		if text != "" {
			return 0, nil, text
		}
	}
	b.next++
	id := b.next
	req := wire.ToolRequest{V: wire.ProtocolVersion, Op: wire.OpTool, ID: id, Name: tool, Args: args,
		MetaSession: m.SessionID, MetaThread: m.ThreadID}
	ch := make(chan result, 1)
	b.pending[id] = ch
	b.conn.SetWriteDeadline(time.Now().Add(HelloTimeout))
	if err := ipc.WriteLine(b.conn, req); err != nil {
		delete(b.pending, id)
		if errors.Is(err, ipc.ErrLineTooLong) {
			return 0, nil, TextTooLarge
		}
		b.logf("write: %v", err)
		b.dropLocked(b.conn, err)
		return 0, nil, TextLost
	}
	return id, ch, ""
}

// startLocked starts the listener once, then waits up to StartWait for it to answer. It returns
// the tool error text, or "" once connected. It holds b.mu.
func (b *IPC) startLocked(ctx context.Context) string {
	if b.Start == nil || b.started {
		return TextNotRunning
	}
	b.started = true
	if err := b.Start(); err != nil {
		b.logf("did not start the listener: %v", err)
		return TextNotRunning + ". kwak did not start it: " + err.Error()
	}
	b.logf("started the listener")
	after := b.After
	if after == nil {
		after = time.After
	}
	deadline := after(StartWait)
	for {
		select {
		case <-ctx.Done():
			return TextStarted
		case <-deadline:
			return TextStarted
		case <-after(StartPoll):
		}
		if text := b.connect(ctx); text != TextNotRunning {
			return text
		}
	}
}

// connect dials and says hello. It holds b.mu.
func (b *IPC) connect(ctx context.Context) string {
	dctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	c, err := b.Dial(dctx)
	if err != nil {
		b.logf("dial: %v", err)
		return TextNotRunning
	}
	c.SetDeadline(time.Now().Add(HelloTimeout))
	lr := ipc.NewLineReader(c)
	var ack wire.Ack
	err = ipc.WriteLine(c, b.Hello())
	if err == nil {
		var line []byte
		if line, err = lr.Next(); err == nil {
			err = wire.Decode(line, &ack)
		}
	}
	if err != nil {
		c.Close()
		b.logf("hello: %v", err)
		return TextNotRunning
	}
	if !ack.OK {
		c.Close()
		return errText(ack.Err)
	}
	if ack.Kwak != "" {
		if cmp, err := wire.CompareVersions(ack.Kwak, wire.Version); err == nil && cmp < 0 {
			c.Close()
			return TextListenerOlder
		}
	}
	c.SetDeadline(time.Time{})
	b.conn = c
	b.pending = map[int64]chan result{}
	go b.read(c, lr)
	return ""
}

func (b *IPC) read(c net.Conn, lr *ipc.LineReader) {
	for {
		line, err := lr.Next()
		if err != nil {
			b.mu.Lock()
			b.dropLocked(c, err)
			b.mu.Unlock()
			return
		}
		var resp wire.ToolResponse
		if wire.Decode(line, &resp) != nil {
			continue
		}
		b.mu.Lock()
		if b.conn == c {
			if ch, ok := b.pending[resp.ID]; ok {
				delete(b.pending, resp.ID)
				ch <- result{resp: resp}
			}
		}
		b.mu.Unlock()
	}
}

// dropLocked closes c and fails its calls in flight, once. It holds b.mu.
func (b *IPC) dropLocked(c net.Conn, err error) {
	c.Close()
	if b.conn != c {
		return
	}
	b.conn = nil
	for id, ch := range b.pending {
		ch <- result{err: errors.Join(errClosed, err)}
		delete(b.pending, id)
	}
}

// Close closes the connection, if any.
func (b *IPC) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		b.dropLocked(b.conn, errClosed)
	}
	return nil
}
