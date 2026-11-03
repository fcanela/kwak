package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "km")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

const helloOK = `{"v":1,"ok":true,"kwak":"0.1.0"}`

// fakeListener is a tiny in-test listener for the MCP connection: it answers the first line with
// hello, and each tool line with tool (in its own goroutine, so answers may be out of order).
type fakeListener struct {
	path  string
	hello func(line string) string
	tool  func(req wire.ToolRequest, line string) string

	mu     sync.Mutex
	hellos []string
	tools  []string
	conns  []net.Conn
}

func listen(t *testing.T, fl *fakeListener) *fakeListener {
	t.Helper()
	if fl.path == "" {
		fl.path = filepath.Join(shortDir(t), "s", "kwak.sock")
	}
	if fl.hello == nil {
		fl.hello = func(string) string { return helloOK }
	}
	ln, err := ipc.Listen(fl.path, ipc.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); fl.drop() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fl.mu.Lock()
			fl.conns = append(fl.conns, c)
			fl.mu.Unlock()
			go fl.serve(c)
		}
	}()
	return fl
}

func (fl *fakeListener) serve(c net.Conn) {
	defer c.Close()
	lr := ipc.NewLineReader(c)
	line, err := lr.Next()
	if err != nil {
		return
	}
	fl.mu.Lock()
	fl.hellos = append(fl.hellos, string(line))
	fl.mu.Unlock()
	c.Write([]byte(fl.hello(string(line)) + "\n"))
	var wmu sync.Mutex
	for {
		line, err := lr.Next()
		if err != nil {
			return
		}
		fl.mu.Lock()
		fl.tools = append(fl.tools, string(line))
		fl.mu.Unlock()
		var req wire.ToolRequest
		json.Unmarshal(line, &req)
		go func(line string) {
			if r := fl.tool(req, line); r != "" {
				wmu.Lock()
				c.Write([]byte(r + "\n"))
				wmu.Unlock()
			}
		}(string(line))
	}
}

func (fl *fakeListener) drop() {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	for _, c := range fl.conns {
		c.Close()
	}
	fl.conns = nil
}

func (fl *fakeListener) seen() (hellos, tools []string) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return append([]string(nil), fl.hellos...), append([]string(nil), fl.tools...)
}

func backend(path string, hello wire.MCPHello) *IPC {
	return &IPC{
		Dial:  func(ctx context.Context) (net.Conn, error) { return ipc.Dial(ctx, path, nil) },
		Hello: func() wire.MCPHello { return hello },
	}
}

var claudeHello = wire.MCPHello{V: 1, Kwak: wire.Version, Op: wire.OpMCPHello, Harness: wire.HarnessClaude}

func okReply(req wire.ToolRequest, text string) string {
	b, _ := json.Marshal(wire.ToolResponse{V: 1, ID: req.ID, OK: true, Text: text})
	return string(b)
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestNoListener(t *testing.T) {
	b := backend(filepath.Join(shortDir(t), "none", "kwak.sock"), claudeHello)
	start := time.Now()
	msgs := transcript(t, b,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send","arguments":{"text":"hi"}}}`,
	)
	if time.Since(start) > time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	if byID(t, msgs, "1").Error != nil || byID(t, msgs, "2").Error != nil {
		t.Fatal("initialize or tools/list failed without a listener")
	}
	if r := resultOf(t, byID(t, msgs, "3")); !r.IsError || r.Content[0].Text != TextNotRunning {
		t.Fatalf("tools/call: %+v", r)
	}
}

func TestHelloFirstAndReconnect(t *testing.T) {
	fl := listen(t, &fakeListener{tool: func(req wire.ToolRequest, _ string) string { return okReply(req, "ok "+req.Name) }})
	b := backend(fl.path, claudeHello)
	defer b.Close()
	if text, isErr := b.Call(ctx(t), "status", nil, Meta{}); text != "ok status" || isErr {
		t.Fatalf("first call: %q %v", text, isErr)
	}
	b.Call(ctx(t), "status", json.RawMessage(`{}`), Meta{})
	if h, _ := fl.seen(); len(h) != 1 {
		t.Fatalf("%d hellos on one connection", len(h))
	}
	fl.drop()
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock()
		gone := b.conn == nil
		b.mu.Unlock()
		if gone || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if text, _ := b.Call(ctx(t), "volunteer", json.RawMessage(`{"on":true}`), Meta{}); text != "ok volunteer" {
		t.Fatalf("after reconnect: %q", text)
	}
	hellos, tools := fl.seen()
	if len(hellos) != 2 || len(tools) != 3 {
		t.Fatalf("hellos %d, tools %d", len(hellos), len(tools))
	}
	want := `{"v":1,"kwak":"0.1.0","op":"mcp_hello","harness":"claude","env_session":"","chain":null}`
	for _, h := range hellos {
		if h != want {
			t.Fatalf("hello %s", h)
		}
	}
}

func TestConnectionLostDuringCall(t *testing.T) {
	var fl *fakeListener
	fl = listen(t, &fakeListener{tool: func(wire.ToolRequest, string) string { fl.drop(); return "" }})
	b := backend(fl.path, claudeHello)
	if text, isErr := b.Call(ctx(t), "status", nil, Meta{}); text != TextLost || !isErr {
		t.Fatalf("got %q %v", text, isErr)
	}
}

func TestOutOfOrderReplies(t *testing.T) {
	second := make(chan struct{})
	fl := listen(t, &fakeListener{tool: func(req wire.ToolRequest, _ string) string {
		var a struct{ Text string }
		json.Unmarshal(req.Args, &a)
		if a.Text == "first" {
			<-second
		} else {
			defer close(second)
		}
		return okReply(req, "reply to "+a.Text)
	}})
	b := backend(fl.path, claudeHello)
	defer b.Close()
	var wg sync.WaitGroup
	got := make([]string, 2)
	for i, text := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = b.Call(ctx(t), "send", json.RawMessage(`{"text":"`+text+`"}`), Meta{})
		}()
		if i == 0 {
			for {
				if _, tools := fl.seen(); len(tools) == 1 {
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
	}
	wg.Wait()
	if got[0] != "reply to first" || got[1] != "reply to second" {
		t.Fatalf("got %q", got)
	}
}

func TestTimeout(t *testing.T) {
	fl := listen(t, &fakeListener{tool: func(wire.ToolRequest, string) string { return "" }})
	b := backend(fl.path, claudeHello)
	defer b.Close()
	c, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if text, isErr := b.Call(c, "status", nil, Meta{}); text != TextTimeout || !isErr {
		t.Fatalf("got %q %v", text, isErr)
	}
	b.mu.Lock()
	n := len(b.pending)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d calls still pending", n)
	}
}

func TestTooLarge(t *testing.T) {
	fl := listen(t, &fakeListener{tool: func(req wire.ToolRequest, _ string) string { return okReply(req, "ok") }})
	b := backend(fl.path, claudeHello)
	defer b.Close()
	big := json.RawMessage(`{"text":"` + strings.Repeat("a", 2<<20) + `"}`)
	if text, isErr := b.Call(ctx(t), "send", big, Meta{}); text != TextTooLarge || !isErr {
		t.Fatalf("got %q %v", text, isErr)
	}
	if text, _ := b.Call(ctx(t), "status", nil, Meta{}); text != "ok" {
		t.Fatalf("after a large call: %q", text)
	}
	if h, _ := fl.seen(); len(h) != 1 {
		t.Fatalf("a large call dropped the connection: %d hellos", len(h))
	}
}

func TestListenerOlder(t *testing.T) {
	for _, hello := range []string{`{"v":1,"ok":true,"kwak":"0.0.9"}`, `{"v":1,"ok":false,"err":"version"}`} {
		fl := listen(t, &fakeListener{hello: func(string) string { return hello },
			tool: func(req wire.ToolRequest, _ string) string { return okReply(req, "ok") }})
		b := backend(fl.path, claudeHello)
		if text, isErr := b.Call(ctx(t), "status", nil, Meta{}); text != TextListenerOlder || !isErr {
			t.Fatalf("%s: got %q %v", hello, text, isErr)
		}
		if _, tools := fl.seen(); len(tools) != 0 {
			t.Fatalf("%s: a tool call went to an older listener", hello)
		}
	}
	fl := listen(t, &fakeListener{hello: func(string) string { return `{"v":1,"ok":true,"kwak":"0.2.0"}` },
		tool: func(req wire.ToolRequest, _ string) string { return okReply(req, "ok") }})
	if text, _ := backend(fl.path, claudeHello).Call(ctx(t), "status", nil, Meta{}); text != "ok" {
		t.Fatalf("newer listener: %q", text)
	}
}

func TestMainHello(t *testing.T) {
	fl := listen(t, &fakeListener{tool: func(req wire.ToolRequest, _ string) string { return okReply(req, "ok") }})
	env := map[string]string{EnvClaudeSession: wiretest.ClaudeSession}
	chain := []proc.ID{{PID: 5120, Start: 3}, {PID: 4400, Start: 2}}
	for _, c := range []struct {
		harness string
		chain   func() []proc.ID
		want    string
	}{
		{"claude", nil, `{"v":1,"kwak":"0.1.0","op":"mcp_hello","harness":"claude","env_session":"` + wiretest.ClaudeSession + `","chain":null}`},
		{"codex", func() []proc.ID { return chain }, `{"v":1,"kwak":"0.1.0","op":"mcp_hello","harness":"codex","env_session":"","chain":[{"pid":5120,"start":3},{"pid":4400,"start":2}]}`},
	} {
		in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status"}}` + "\n"
		var out, errOut bytes.Buffer
		code := mainWith([]string{"--harness", c.harness}, strings.NewReader(in), &out, &errOut,
			func(k string) string { return env[k] }, func() (string, error) { return fl.path, nil }, c.chain)
		if code != 0 || !strings.Contains(out.String(), `"text":"ok"`) {
			t.Fatalf("%s: exit %d, out %q, err %q", c.harness, code, out.String(), errOut.String())
		}
		hellos, _ := fl.seen()
		if got := hellos[len(hellos)-1]; got != c.want {
			t.Fatalf("%s hello:\n got %s\nwant %s", c.harness, got, c.want)
		}
	}
}

func TestMainUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"--harness", "vim"}, {"--harness"}, {"--harness", "claude", "x"}, {"--bogus"}} {
		var errOut bytes.Buffer
		code := mainWith(args, strings.NewReader(""), &bytes.Buffer{}, &errOut, os.Getenv,
			func() (string, error) { return "", errors.New("unused") }, nil)
		if code != 2 || errOut.Len() == 0 {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut.String())
		}
	}
}

// TestMirrorIPC drives each mcp_hello and tool row of the golden table through tools/call, against
// a socket server that replays the table's reply, and checks the line sent and the result.
func TestMirrorIPC(t *testing.T) {
	expect := map[string]struct {
		text    string
		isError bool
	}{
		"tool while unpaired":                         {wiretest.TextUnpaired, true},
		"tool before the session start was seen":      {wiretest.TextNotSeenYet, true},
		"tool while the registrar is down":            {wiretest.TextNoAgentYet, true},
		"tool with a meta_session that is not a UUID": {TextBadInput, true},
		"tool that failed inside the listener":        {TextInternal, true},
	}
	rows := 0
	for _, row := range wiretest.IPC {
		h, err := wire.ReadHead([]byte(row.Request))
		if err != nil && !errors.Is(err, wire.ErrVersion) {
			continue
		}
		switch h.Op {
		case wire.OpTool:
			rows++
			var want wire.ToolRequest
			json.Unmarshal([]byte(row.Request), &want)
			var reply wire.ToolResponse
			json.Unmarshal([]byte(row.Reply), &reply)
			fl := listen(t, &fakeListener{tool: func(req wire.ToolRequest, _ string) string {
				return strings.Replace(row.Reply, `"id":`+strconv.FormatInt(want.ID, 10)+`,`, `"id":`+strconv.FormatInt(req.ID, 10)+`,`, 1)
			}})
			b := backend(fl.path, claudeHello)
			params := map[string]any{"name": want.Name, "arguments": want.Args}
			if want.MetaSession != "" || want.MetaThread != "" {
				params["_meta"] = map[string]string{"sessionId": want.MetaSession, "threadId": want.MetaThread}
			}
			p, _ := json.Marshal(params)
			msgs := transcript(t, b, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+string(p)+`}`)
			b.Close()
			r := resultOf(t, byID(t, msgs, "1"))
			wantText, wantErr := reply.Text, reply.IsError
			if e, ok := expect[row.Name]; ok {
				wantText, wantErr = e.text, e.isError
			} else if !reply.OK {
				t.Errorf("%s: no expected text for a refusal", row.Name)
			}
			if r.Content[0].Text != wantText || r.IsError != wantErr {
				t.Errorf("%s: result %q %v, want %q %v", row.Name, r.Content[0].Text, r.IsError, wantText, wantErr)
			}
			_, tools := fl.seen()
			if len(tools) != 1 {
				t.Errorf("%s: %d tool lines", row.Name, len(tools))
				continue
			}
			var sent wire.ToolRequest
			json.Unmarshal([]byte(tools[0]), &sent)
			sent.ID = want.ID
			var buf bytes.Buffer
			ipc.WriteLine(&buf, sent)
			if got := strings.TrimSuffix(buf.String(), "\n"); got != row.CanonicalRequest() {
				t.Errorf("%s: sent\n %s\nwant\n %s", row.Name, got, row.CanonicalRequest())
			}
			if err := wiretest.SameShape([]byte(row.CanonicalRequest()), []byte(tools[0]), "op", "name", "meta_session", "meta_thread"); err != nil {
				t.Errorf("%s: %v", row.Name, err)
			}
		case wire.OpMCPHello:
			rows++
			var want wire.MCPHello
			json.Unmarshal([]byte(row.Request), &want)
			ours := want.V == wire.ProtocolVersion && want.Kwak == wire.Version
			hello := want
			hello.V, hello.Kwak = wire.ProtocolVersion, wire.Version
			fl := listen(t, &fakeListener{hello: func(string) string { return row.Reply },
				tool: func(req wire.ToolRequest, _ string) string { return okReply(req, "ok") }})
			b := backend(fl.path, hello)
			text, isErr := b.Call(ctx(t), "status", nil, Meta{})
			b.Close()
			var ack wire.Ack
			json.Unmarshal([]byte(row.Reply), &ack)
			wantText, wantErr := "ok", false
			if !ack.OK {
				wantText, wantErr = TextListenerOlder, true
			}
			if text != wantText || isErr != wantErr {
				t.Errorf("%s: %q %v, want %q %v", row.Name, text, isErr, wantText, wantErr)
			}
			hellos, _ := fl.seen()
			if ours && (len(hellos) != 1 || hellos[0] != row.CanonicalRequest()) {
				t.Errorf("%s: hello %v, want %s", row.Name, hellos, row.CanonicalRequest())
			}
		}
	}
	if rows < 19 {
		t.Fatalf("only %d mcp rows", rows)
	}
}
