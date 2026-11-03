//go:build integration && linux

package e2e

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/wire"
)

// The test binary re-executes itself in three roles, chosen by envFake:
//
//	term:    the terminal a harness runs in; it runs the harness and waits, so each harness has
//	         its own parent (C[2] of a Claude hook) instead of the shared test process
//	1:       the fake harness; the test drives it over stdin and stdout, one JSON line each way
//	shell:   Codex's $SHELL -c; it runs its argv and waits (the runner has no sh)
//
// The fake harness runs the real kwak binary as `kwak hook` and `kwak mcp` children.
const (
	envFake     = "KWAK_FAKE_HARNESS"
	fakeHarness = "1"
	fakeTerm    = "term"
	fakeShell   = "shell"
	envFakeBin  = "KWAK_FAKE_KWAK" // the kwak binary the fake harness runs
)

// fakeReq is one command from the test to the fake harness.
type fakeReq struct {
	Op      string            `json:"op"` // hook, mcp_start, mcp_send, mcp_recv, mcp_stop, exit
	Harness wire.Harness      `json:"harness,omitempty"`
	Event   string            `json:"event,omitempty"`
	Stdin   []byte            `json:"stdin,omitempty"`
	Direct  bool              `json:"direct,omitempty"` // Codex hook without the shell layer
	Line    []byte            `json:"line,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Kill    bool              `json:"kill,omitempty"`
	Timeout time.Duration     `json:"timeout,omitempty"`
}

// fakeResp is the fake harness's answer.
type fakeResp struct {
	Err    string        `json:"err,omitempty"`
	PID    int           `json:"pid,omitempty"`
	Stdout []byte        `json:"stdout,omitempty"`
	Stderr string        `json:"stderr,omitempty"`
	Code   int           `json:"code"`
	Took   time.Duration `json:"took,omitempty"`
	Line   []byte        `json:"line,omitempty"`
}

func fakeTermMain() int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cmd := exec.Command(self)
	cmd.Env = withEnv(os.Environ(), envFake, fakeHarness)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return exitCode(cmd.Run())
}

func fakeShellMain(argv []string) int {
	if len(argv) == 0 {
		return 2
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = withEnv(os.Environ(), envFake, "")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return exitCode(cmd.Run())
}

func exitCode(err error) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	}
	fmt.Fprintln(os.Stderr, err)
	return 1
}

func withEnv(env []string, k, v string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, k+"=") {
			out = append(out, kv)
		}
	}
	if v != "" {
		out = append(out, k+"="+v)
	}
	return out
}

// fakeHarnessMain serves the test's commands until stdin closes.
func fakeHarnessMain() int {
	kwak := os.Getenv(envFakeBin)
	childEnv := withEnv(os.Environ(), envFake, "")
	out := json.NewEncoder(os.Stdout)
	out.Encode(fakeResp{PID: os.Getpid()})
	var mcp *mcpChild
	defer func() {
		if mcp != nil {
			mcp.cmd.Process.Kill()
			mcp.cmd.Wait()
		}
	}()
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64<<10), 32<<20)
	for in.Scan() {
		var req fakeReq
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			out.Encode(fakeResp{Err: err.Error()})
			continue
		}
		var resp fakeResp
		switch req.Op {
		case "hook":
			argv := []string{kwak, "hook", string(req.Harness), req.Event}
			if req.Harness == wire.HarnessCodex && !req.Direct {
				self, _ := os.Executable()
				argv = append([]string{self}, argv...)
			}
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Env = childEnv
			if req.Harness == wire.HarnessCodex && !req.Direct {
				cmd.Env = withEnv(childEnv, envFake, fakeShell)
			}
			cmd.Stdin = strings.NewReader(string(req.Stdin))
			var so, se strings.Builder
			cmd.Stdout, cmd.Stderr = &so, &se
			start := time.Now()
			err := cmd.Run()
			resp = fakeResp{Stdout: []byte(so.String()), Stderr: se.String(), Code: exitCode(err), Took: time.Since(start)}
		case "mcp_start":
			if mcp != nil {
				resp.Err = "mcp already running"
				break
			}
			c, err := startMCP(kwak, req.Harness, childEnv, req.Env)
			if err != nil {
				resp.Err = err.Error()
				break
			}
			mcp, resp.PID = c, c.cmd.Process.Pid
		case "mcp_send":
			if mcp == nil {
				resp.Err = "no mcp"
				break
			}
			if _, err := mcp.stdin.Write(append(req.Line, '\n')); err != nil {
				resp.Err = err.Error()
			}
		case "mcp_recv":
			if mcp == nil {
				resp.Err = "no mcp"
				break
			}
			select {
			case l, ok := <-mcp.lines:
				if !ok {
					resp.Err = "eof"
				}
				resp.Line = l
			case <-time.After(req.Timeout):
				resp.Err = "timeout"
			}
		case "mcp_stop":
			if mcp == nil {
				resp.Err = "no mcp"
				break
			}
			if req.Kill {
				mcp.cmd.Process.Kill()
			} else {
				mcp.stdin.Close()
			}
			resp.Code = exitCode(mcp.cmd.Wait())
			resp.Stderr = mcp.stderr.String()
			mcp = nil
		case "exit":
			return 0
		default:
			resp.Err = "unknown op " + req.Op
		}
		out.Encode(resp)
	}
	return 0
}

type mcpChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  <-chan []byte
	stderr *lockedBuf
}

func startMCP(kwak string, h wire.Harness, env []string, extra map[string]string) (*mcpChild, error) {
	cmd := exec.Command(kwak, "mcp", "--harness", string(h))
	cmd.Env = env
	for k, v := range extra {
		cmd.Env = withEnv(cmd.Env, k, v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &mcpChild{cmd: cmd, stdin: stdin, stderr: &lockedBuf{}}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c.lines = lines(stdout)
	return c, nil
}

// ---- the test side ----

// FakeHarness is one harness session, a Claude Code or Codex process as kwak sees it: a process
// with its own parent that runs `kwak hook` per event and one long-lived `kwak mcp`.
type FakeHarness struct {
	Owner     *Owner
	Harness   wire.Harness
	SessionID string
	Cwd       string
	PID       int // the fake harness process; C[1] of its hooks (C[2] for Codex under the shell)

	// Meta is sent as params._meta on tools/call. Codex harnesses get {"sessionId": SessionID}.
	Meta map[string]any
	// MCPEnv is added to the kwak mcp environment. Claude harnesses get CLAUDE_CODE_SESSION_ID.
	MCPEnv map[string]string

	w      *World
	term   *exec.Cmd
	in     io.WriteCloser
	out    <-chan []byte
	mu     sync.Mutex
	nextID int
	closed bool
}

// NewHarness starts a fake harness for owner, with a fresh session id (UUIDv4 for Claude, UUIDv7
// for Codex) and a fresh cwd. It neither runs SessionStart nor starts MCP; Begin does both.
func (o *Owner) NewHarness(t *testing.T, h wire.Harness) *FakeHarness {
	t.Helper()
	id := UUIDv4()
	if h == wire.HarnessCodex {
		id = UUIDv7()
	}
	cwd, err := os.MkdirTemp("", "cwd-")
	if err != nil {
		t.Fatal(err)
	}
	o.w.top.Cleanup(func() { os.RemoveAll(cwd) })
	return o.NewHarnessWith(t, h, id, cwd)
}

// NewHarnessWith starts a fake harness with the given session id and cwd.
func (o *Owner) NewHarnessWith(t *testing.T, h wire.Harness, sessionID, cwd string) *FakeHarness {
	t.Helper()
	w := o.w
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(withEnv(os.Environ(), envFake, fakeTerm),
		envFakeBin+"="+w.Kwak, "KWAK_SOCKET="+o.Socket)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = w.sink
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the fake harness: %v", err)
	}
	f := &FakeHarness{Owner: o, Harness: h, SessionID: sessionID, Cwd: cwd, w: w, term: cmd, in: in, out: lines(stdout), nextID: 1}
	switch h {
	case wire.HarnessCodex:
		f.Meta = map[string]any{"sessionId": sessionID}
	case wire.HarnessClaude:
		f.MCPEnv = map[string]string{"CLAUDE_CODE_SESSION_ID": sessionID}
	}
	w.top.Cleanup(f.Close)
	select {
	case l, ok := <-f.out:
		var r fakeResp
		if !ok || json.Unmarshal(l, &r) != nil || r.PID == 0 {
			t.Fatalf("the fake harness did not start: %q", l)
		}
		f.PID = r.PID
	case <-time.After(10 * time.Second):
		t.Fatal("the fake harness did not start")
	}
	return f
}

func (f *FakeHarness) do(t *testing.T, req fakeReq) fakeResp {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(req)
	if _, err := f.in.Write(append(b, '\n')); err != nil {
		t.Fatalf("fake harness %s: %v", req.Op, err)
	}
	select {
	case l, ok := <-f.out:
		if !ok {
			t.Fatalf("fake harness exited during %s", req.Op)
		}
		var r fakeResp
		if err := json.Unmarshal(l, &r); err != nil {
			t.Fatalf("fake harness reply %q: %v", l, err)
		}
		return r
	case <-time.After(60 * time.Second):
		t.Fatalf("fake harness did not answer %s", req.Op)
	}
	return fakeResp{}
}

// Begin runs SessionStart (source startup) and starts and initializes MCP, as a harness does at
// start. It checks SessionStart printed nothing and returned quickly.
func (f *FakeHarness) Begin(t *testing.T) {
	t.Helper()
	r := f.HookResult(t, wire.EventSessionStart, nil)
	if len(r.Stdout) != 0 {
		t.Fatalf("SessionStart of a new session printed %q", r.Stdout)
	}
	f.StartMCP(t)
}

// HookEventName is the harness's hook_event_name for a kwak event.
func HookEventName(event string) string {
	return map[string]string{
		wire.EventSessionStart: "SessionStart", wire.EventPrompt: "UserPromptSubmit",
		wire.EventPostTool: "PostToolUse", wire.EventStop: "Stop",
		wire.EventSubagentStop: "SubagentStop", wire.EventSessionEnd: "SessionEnd",
	}[event]
}

// Stdin is the hook stdin the harness would send for event, with fields merged over the defaults.
// A nil value in fields drops that key.
func (f *FakeHarness) Stdin(event string, fields map[string]any) []byte {
	m := map[string]any{
		"session_id":      f.SessionID,
		"cwd":             f.Cwd,
		"hook_event_name": HookEventName(event),
		"transcript_path": "/tmp/transcript-" + f.SessionID + ".jsonl",
	}
	if f.Harness == wire.HarnessCodex {
		m["model"] = "gpt-5-codex"
		m["permission_mode"] = "default"
	}
	switch event {
	case wire.EventSessionStart:
		m["source"] = "startup"
	case wire.EventPrompt:
		m["prompt"] = "carry on"
	case wire.EventPostTool:
		m["tool_name"] = "Bash"
		m["tool_input"] = map[string]any{"command": "ls"}
		m["tool_response"] = map[string]any{"stdout": "file\n", "exit_code": 0}
	case wire.EventStop:
		m["stop_hook_active"] = false
	case wire.EventSessionEnd:
		m["reason"] = "other"
	}
	for k, v := range fields {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	b, _ := json.Marshal(m)
	return b
}

// Hook runs `kwak hook <harness> <event>` from this harness and returns its stdout.
func (f *FakeHarness) Hook(t *testing.T, event string, fields map[string]any) []byte {
	t.Helper()
	return f.HookResult(t, event, fields).Stdout
}

// HookResult is Hook with the exit code, stderr and duration.
func (f *FakeHarness) HookResult(t *testing.T, event string, fields map[string]any) fakeResp {
	t.Helper()
	return f.HookRaw(t, event, f.Stdin(event, fields), false)
}

// HookRaw runs a hook with raw stdin. direct runs a Codex hook without the shell layer.
func (f *FakeHarness) HookRaw(t *testing.T, event string, stdin []byte, direct bool) fakeResp {
	t.Helper()
	r := f.do(t, fakeReq{Op: "hook", Harness: f.Harness, Event: event, Stdin: stdin, Direct: direct})
	if r.Err != "" {
		t.Fatalf("hook %s: %s", event, r.Err)
	}
	if r.Code != 0 {
		t.Fatalf("kwak hook %s exited %d: %s", event, r.Code, r.Stderr)
	}
	f.w.cap.add(string(f.Harness)+" hook "+event, r.Stdout)
	return r
}

// WaitHook runs the hook until it prints something, and returns that. Each empty run drains
// nothing, so polling is safe.
func (f *FakeHarness) WaitHook(t *testing.T, event string, fields map[string]any) []byte {
	t.Helper()
	var out []byte
	Eventually(t, "output from the "+event+" hook of "+f.SessionID, func() bool {
		out = f.Hook(t, event, fields)
		return len(out) > 0
	})
	return out
}

// HookText decodes a hook's additionalContext (or a Codex Stop reason).
func HookText(t *testing.T, out []byte) (event, text string) {
	t.Helper()
	var v struct {
		HSO struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("hook output is not JSON: %v: %q", err, out)
	}
	if v.Decision != "" {
		return "decision:" + v.Decision, v.Reason
	}
	return v.HSO.HookEventName, v.HSO.AdditionalContext
}

// StartMCP starts `kwak mcp --harness <h>` and initializes it the way the harness does.
func (f *FakeHarness) StartMCP(t *testing.T) {
	t.Helper()
	r := f.do(t, fakeReq{Op: "mcp_start", Harness: f.Harness, Env: f.MCPEnv})
	if r.Err != "" {
		t.Fatalf("mcp_start: %s", r.Err)
	}
	if f.Harness == wire.HarnessClaude {
		if e := f.RPCError(t, "server/discover", map[string]any{}); e == nil || e.Code != -32601 {
			t.Fatalf("server/discover: want -32601, got %+v", e)
		}
	}
	res := f.RPC(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "fake-" + string(f.Harness), "version": "0"},
	})
	var init struct {
		ServerInfo struct{ Name string } `json:"serverInfo"`
	}
	if json.Unmarshal(res, &init) != nil || init.ServerInfo.Name != "kwak" {
		t.Fatalf("initialize: %s", res)
	}
	f.Notify(t, "notifications/initialized", nil)
}

// StopMCP ends the MCP server: kill sends SIGKILL, otherwise stdin is closed as on harness exit.
func (f *FakeHarness) StopMCP(t *testing.T, kill bool) {
	t.Helper()
	if r := f.do(t, fakeReq{Op: "mcp_stop", Kill: kill}); r.Err != "" {
		t.Fatalf("mcp_stop: %s", r.Err)
	}
}

// RPCErr is a JSON-RPC error.
type RPCErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (f *FakeHarness) call(t *testing.T, method string, params any) (json.RawMessage, *RPCErr) {
	t.Helper()
	f.mu.Lock()
	id := f.nextID
	f.nextID++
	f.mu.Unlock()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	b, _ := json.Marshal(msg)
	if r := f.do(t, fakeReq{Op: "mcp_send", Line: b}); r.Err != "" {
		t.Fatalf("mcp_send: %s", r.Err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		r := f.do(t, fakeReq{Op: "mcp_recv", Timeout: time.Until(deadline)})
		if r.Err != "" {
			t.Fatalf("mcp %s: %s", method, r.Err)
		}
		f.w.cap.add(string(f.Harness)+" mcp", r.Line)
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *RPCErr         `json:"error"`
		}
		if err := json.Unmarshal(r.Line, &resp); err != nil {
			t.Fatalf("mcp line %q: %v", r.Line, err)
		}
		if string(resp.ID) == fmt.Sprint(id) {
			return resp.Result, resp.Error
		}
	}
	t.Fatalf("no answer to %s", method)
	return nil, nil
}

// RPC sends a request and returns its result; an error answer fails the test.
func (f *FakeHarness) RPC(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	res, e := f.call(t, method, params)
	if e != nil {
		t.Fatalf("%s: error %+v", method, e)
	}
	return res
}

// RPCError sends a request and returns its error answer, or nil.
func (f *FakeHarness) RPCError(t *testing.T, method string, params any) *RPCErr {
	t.Helper()
	_, e := f.call(t, method, params)
	return e
}

// Notify sends a notification; nothing answers it.
func (f *FakeHarness) Notify(t *testing.T, method string, params any) {
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	b, _ := json.Marshal(msg)
	if r := f.do(t, fakeReq{Op: "mcp_send", Line: b}); r.Err != "" {
		t.Fatalf("mcp_send: %s", r.Err)
	}
}

// Tool is a tools/call result.
type Tool struct {
	Text    string
	IsError bool
}

// Call runs a kwak tool through MCP, with f.Meta as _meta.
func (f *FakeHarness) Call(t *testing.T, tool string, args any) Tool {
	t.Helper()
	return f.CallMeta(t, tool, args, f.Meta)
}

// CallMeta is Call with an explicit _meta (nil sends none).
func (f *FakeHarness) CallMeta(t *testing.T, tool string, args any, meta map[string]any) Tool {
	t.Helper()
	p := map[string]any{"name": tool, "arguments": args}
	if meta != nil {
		p["_meta"] = meta
	}
	res := f.RPC(t, "tools/call", p)
	var r struct {
		Content []struct {
			Type, Text string
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &r); err != nil || len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatalf("tools/call %s: %s", tool, res)
	}
	return Tool{Text: r.Content[0].Text, IsError: r.IsError}
}

// MustCall is Call that fails the test on a tool error.
func (f *FakeHarness) MustCall(t *testing.T, tool string, args any) string {
	t.Helper()
	r := f.Call(t, tool, args)
	if r.IsError {
		t.Fatalf("%s %s: %s", f.SessionID, tool, r.Text)
	}
	return r.Text
}

// ToolName is the harness's name for a kwak tool (spec 6.4).
func (f *FakeHarness) ToolName(tool string) string { return wire.ToolPrefix(f.Harness) + tool }

// Kill SIGKILLs the fake harness process; its kwak mcp child sees stdin close.
func (f *FakeHarness) Kill(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(f.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the fake harness: %v", err)
	}
	f.Close()
}

// Close ends the fake harness (its MCP server too) and waits for it.
func (f *FakeHarness) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	f.mu.Unlock()
	f.in.Close()
	done := make(chan struct{})
	go func() { f.term.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		syscall.Kill(f.PID, syscall.SIGKILL)
		f.term.Process.Kill()
		<-done
	}
}

var sentIn = regexp.MustCompile(`in thread (\$[A-Za-z0-9_+/=:.-]+)\.`)

// ThreadOf returns the thread id a send result names.
func ThreadOf(t *testing.T, sendResult string) string {
	t.Helper()
	m := sentIn.FindStringSubmatch(sendResult)
	if m == nil {
		t.Fatalf("no thread id in %q", sendResult)
	}
	return m[1]
}
