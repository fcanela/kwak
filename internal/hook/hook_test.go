package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
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
	d, err := os.MkdirTemp("", "kh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// listener is a tiny in-test listener: it records each request line and answers with reply. An
// empty reply holds the connection open, as a hung listener would.
type listener struct {
	path string
	mu   sync.Mutex
	got  []string
}

func serve(t *testing.T, reply func(line string) string) *listener {
	t.Helper()
	l := &listener{path: filepath.Join(shortDir(t), "s", "kwak.sock")}
	ln, err := ipc.Listen(l.path, ipc.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { ln.Close(); close(done) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := ipc.NewLineReader(c).Next()
				if err != nil {
					return
				}
				l.mu.Lock()
				l.got = append(l.got, string(line))
				l.mu.Unlock()
				r := reply(string(line))
				if r == "" {
					<-done
					return
				}
				c.Write([]byte(r + "\n"))
			}(c)
		}
	}()
	return l
}

func (l *listener) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.got...)
}

func js(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func canned(resp wire.HookResponse, err error) Caller {
	return func(context.Context, wire.HookRequest) (wire.HookResponse, error) { return resp, err }
}

const claudeStdin = `{"session_id":"` + wiretest.ClaudeSession + `","cwd":"/home/u/src/app","hook_event_name":"%s"}`

func TestOutputExact(t *testing.T) {
	text := "kwak: 1 message\n| a <b> & &lt;tag"
	jt := `"kwak: 1 message\n| a <b> & &lt;tag"`
	cases := []struct {
		h          wire.Harness
		event, hev string
		active     bool
		want       string
	}{
		{wire.HarnessClaude, wire.EventSessionStart, "SessionStart", false, `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":` + jt + "}}\n"},
		{wire.HarnessClaude, wire.EventPrompt, "UserPromptSubmit", false, `{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":` + jt + "}}\n"},
		{wire.HarnessClaude, wire.EventPostTool, "PostToolUse", false, `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":` + jt + "}}\n"},
		{wire.HarnessClaude, wire.EventPostTool, "PostToolUseFailure", false, `{"hookSpecificOutput":{"hookEventName":"PostToolUseFailure","additionalContext":` + jt + "}}\n"},
		{wire.HarnessClaude, wire.EventPostTool, "", false, `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":` + jt + "}}\n"},
		{wire.HarnessClaude, wire.EventPostTool, "Bogus", false, `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":` + jt + "}}\n"},
		{wire.HarnessClaude, wire.EventStop, "Stop", false, `{"hookSpecificOutput":{"hookEventName":"Stop","additionalContext":` + jt + "}}\n"},
		{wire.HarnessClaude, wire.EventStop, "Stop", true, ""},
		{wire.HarnessClaude, wire.EventSubagentStop, "SubagentStop", false, ""},
		{wire.HarnessClaude, wire.EventSessionEnd, "SessionEnd", false, ""},
		{wire.HarnessCodex, wire.EventSessionStart, "SessionStart", false, `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":` + jt + "}}\n"},
		{wire.HarnessCodex, wire.EventPrompt, "UserPromptSubmit", false, `{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":` + jt + "}}\n"},
		{wire.HarnessCodex, wire.EventPostTool, "PostToolUseFailure", false, `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":` + jt + "}}\n"},
		{wire.HarnessCodex, wire.EventStop, "Stop", false, `{"decision":"block","reason":` + jt + "}\n"},
		{wire.HarnessCodex, wire.EventStop, "Stop", true, ""},
		{wire.HarnessCodex, wire.EventSubagentStop, "SubagentStop", false, ""},
		{wire.HarnessCodex, wire.EventSessionEnd, "SessionEnd", false, ""},
	}
	for _, c := range cases {
		stdin := `{"session_id":"` + wiretest.ClaudeSession + `","cwd":"/x","hook_event_name":"` + c.hev + `","stop_hook_active":` +
			map[bool]string{true: "true", false: "false"}[c.active] + `}`
		var out bytes.Buffer
		code := Run(c.h, c.event, strings.NewReader(stdin), &out, canned(wire.HookResponse{V: 1, OK: true, Text: text}, nil), nil)
		if code != 0 {
			t.Errorf("%s %s: exit %d", c.h, c.event, code)
		}
		if out.String() != c.want {
			t.Errorf("%s %s %s active=%v:\n got %q\nwant %q", c.h, c.event, c.hev, c.active, out.String(), c.want)
		}
	}
}

func TestRunPrintsNothing(t *testing.T) {
	ok := wire.HookResponse{V: 1, OK: true, Text: "kwak: hello"}
	cases := []struct {
		name  string
		event string
		stdin string
		call  Caller
	}{
		{"empty inbox", wire.EventPrompt, `{"session_id":"x"}`, canned(wire.HookResponse{V: 1, OK: true}, nil)},
		{"agent_id with no binding", wire.EventPostTool, `{"session_id":"x","agent_id":"a1","tool_name":"Bash"}`, canned(wire.HookResponse{V: 1, OK: true}, nil)},
		{"refused", wire.EventPrompt, `{"session_id":"x"}`, canned(wire.HookResponse{V: 1, OK: false, Err: "bad_input", Text: "x"}, nil)},
		{"call error", wire.EventPrompt, `{"session_id":"x"}`, canned(ok, errors.New("down"))},
		{"stop active", wire.EventStop, `{"session_id":"x","stop_hook_active":true}`, canned(ok, nil)},
		{"not JSON", wire.EventPrompt, `hello`, canned(ok, nil)},
		{"array", wire.EventPrompt, `[]`, canned(ok, nil)},
		{"empty stdin", wire.EventPrompt, ``, canned(ok, nil)},
	}
	for _, c := range cases {
		var out bytes.Buffer
		if code := Run(wire.HarnessClaude, c.event, strings.NewReader(c.stdin), &out, c.call, nil); code != 0 {
			t.Errorf("%s: exit %d", c.name, code)
		}
		if out.Len() != 0 {
			t.Errorf("%s: printed %q", c.name, out.String())
		}
	}
}

func TestRunTolerantStdin(t *testing.T) {
	cases := []struct {
		name   string
		stdin  string
		check  func(wire.HookRequest) bool
		prints bool
	}{
		{"unknown fields", `{"session_id":"s","cwd":"/a","future":{"x":[1,2]},"model":"m","hook_event_name":"Stop"}`,
			func(r wire.HookRequest) bool { return r.SessionID == "s" && r.Cwd == "/a" }, true},
		{"source of the wrong type", `{"session_id":"s","source":7,"hook_event_name":"Stop"}`,
			func(r wire.HookRequest) bool { return r.SessionID == "s" && r.Source == "" }, true},
		{"stop_hook_active as a string", `{"session_id":"s","stop_hook_active":"false"}`,
			func(r wire.HookRequest) bool { return !r.StopHookActive }, true},
		{"stop_hook_active true as a string", `{"session_id":"s","stop_hook_active":"true"}`,
			func(r wire.HookRequest) bool { return !r.StopHookActive }, true},
		{"hook_event_name of the wrong type", `{"session_id":"s","hook_event_name":5}`,
			func(r wire.HookRequest) bool { return r.SessionID == "s" }, true},
	}
	for _, c := range cases {
		var got wire.HookRequest
		call := func(_ context.Context, r wire.HookRequest) (wire.HookResponse, error) {
			got = r
			return wire.HookResponse{V: 1, OK: true, Text: "t"}, nil
		}
		var out bytes.Buffer
		Run(wire.HarnessClaude, wire.EventStop, strings.NewReader(c.stdin), &out, call, nil)
		if !c.check(got) {
			t.Errorf("%s: request %+v", c.name, got)
		}
		if (out.Len() > 0) != c.prints {
			t.Errorf("%s: printed %q", c.name, out.String())
		}
	}
}

func TestToolInputOnlyForKwakTools(t *testing.T) {
	cases := []struct {
		h     wire.Harness
		stdin string
		want  string
	}{
		{wire.HarnessClaude, `{"tool_name":"Bash","tool_input":{"command":"ls"},"tool_response":"x"}`, "null"},
		{wire.HarnessClaude, `{"tool_name":"mcp__plugin_kwak_kwak__helper","tool_input":{"name":"tests"}}`, `{"name":"tests"}`},
		{wire.HarnessClaude, `{"tool_name":"mcp__kwak__helper","tool_input":{"name":"tests"}}`, "null"},
		{wire.HarnessCodex, `{"tool_name":"mcp__kwak__send","tool_input":{"text":"hi"}}`, `{"text":"hi"}`},
		{wire.HarnessCodex, `{"tool_name":"mcp__kwak__send","tool_input":"hi"}`, "null"},
	}
	for _, c := range cases {
		var got wire.HookRequest
		call := func(_ context.Context, r wire.HookRequest) (wire.HookResponse, error) {
			got = r
			return wire.HookResponse{V: 1, OK: true}, nil
		}
		Run(c.h, wire.EventPostTool, strings.NewReader(c.stdin), &bytes.Buffer{}, call, nil)
		b, _ := json.Marshal(got.ToolInput)
		if string(b) != c.want {
			t.Errorf("%s %s: tool_input %s, want %s", c.h, c.stdin, b, c.want)
		}
	}
}

func TestListenerDown(t *testing.T) {
	dir := shortDir(t)
	missing := filepath.Join(dir, "none", "kwak.sock")

	stale := filepath.Join(dir, "kwak.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()

	for _, sock := range []string{missing, stale} {
		for _, event := range []string{wire.EventPrompt, wire.EventStop, wire.EventSessionEnd} {
			start := time.Now()
			var out, errOut bytes.Buffer
			code := mainWith([]string{"claude", event}, strings.NewReader(`{"session_id":"`+wiretest.ClaudeSession+`"}`),
				&out, &errOut, func() (string, error) { return sock, nil }, nil)
			took := time.Since(start)
			if code != 0 || out.Len() != 0 || errOut.Len() != 0 {
				t.Errorf("%s %s: exit %d, stdout %q, stderr %q", sock, event, code, out.String(), errOut.String())
			}
			if took > 400*time.Millisecond {
				t.Errorf("%s %s: took %v", sock, event, took)
			}
		}
	}
}

func TestSocketPathError(t *testing.T) {
	var out bytes.Buffer
	code := mainWith([]string{"codex", "prompt"}, strings.NewReader(`{}`), &out, &bytes.Buffer{},
		func() (string, error) { return "", errors.New("no HOME") }, nil)
	if code != 0 || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q", code, out.String())
	}
}

func TestListenerHung(t *testing.T) {
	l := serve(t, func(string) string { return "" })
	start := time.Now()
	var out bytes.Buffer
	code := mainWith([]string{"claude", "session-end"}, strings.NewReader(`{"session_id":"s"}`), &out, &bytes.Buffer{},
		func() (string, error) { return l.path, nil }, nil)
	took := time.Since(start)
	if code != 0 || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q", code, out.String())
	}
	if took < EndTimeout-50*time.Millisecond || took > EndTimeout+500*time.Millisecond {
		t.Fatalf("session-end took %v, want about %v", took, EndTimeout)
	}
}

// SessionStart waits longer than the other hooks: the listener may be stopping the session's
// background run first (spec 5.6, 5.11).
func TestSessionStartWaitsForASlowListener(t *testing.T) {
	if StartTimeout < 8*time.Second || StartTimeout >= 10*time.Second {
		t.Fatalf("StartTimeout %v must stay under the plugin's 10 s hook timeout", StartTimeout)
	}
	l := serve(t, func(string) string {
		time.Sleep(ExchangeTimeout + time.Second)
		return `{"v":1,"ok":true,"kwak":"0.1.0","text":"late but shown"}`
	})
	var out bytes.Buffer
	code := mainWith([]string{"claude", "session-start"}, strings.NewReader(`{"session_id":"s","cwd":"/tmp","source":"resume"}`),
		&out, &bytes.Buffer{}, func() (string, error) { return l.path, nil }, nil)
	if code != 0 || !strings.Contains(out.String(), "late but shown") {
		t.Fatalf("exit %d, stdout %q", code, out.String())
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"claude"}, {"vim", "prompt"}, {"claude", "pre-tool"}, {"claude", "prompt", "x"}} {
		var out, errOut bytes.Buffer
		code := mainWith(args, strings.NewReader(""), &out, &errOut, func() (string, error) { return "/nonexistent", nil }, nil)
		if code != 0 || out.Len() != 0 || !strings.Contains(errOut.String(), "usage: kwak hook") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out.String(), errOut.String())
		}
	}
}

// expectedStdout is what each hook row of the IPC golden table prints, given the table's reply.
// The table holds IPC replies, not harness stdout, so these are pinned here.
var expectedStdout = map[string]string{
	"claude session-start resume with waiting items (DV-5)":               `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":` + js(wiretest.DeliveryTwo) + "}}\n",
	"claude prompt with items":                                            `{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":` + js(wiretest.DeliveryTwo) + "}}\n",
	"claude post-tool after Bash, tool_input not forwarded, more waiting": `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":` + js(wiretest.DeliveryOneMore) + "}}\n",
	"claude PostToolUseFailure goes to post-tool":                         `{"hookSpecificOutput":{"hookEventName":"PostToolUseFailure","additionalContext":` + js(wiretest.DeliveryTwo) + "}}\n",
	"claude stop, not active":                                             `{"hookSpecificOutput":{"hookEventName":"Stop","additionalContext":` + js(wiretest.DeliveryTwo) + "}}\n",
	"codex prompt with an item":                                           `{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":` + js(wiretest.DeliveryCodex) + "}}\n",
	"codex post-tool after shell, tool_input not forwarded":               `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":` + js(wiretest.DeliveryCodex) + "}}\n",
	"codex stop, not active: fixed notice, no drain (DV-4)":               `{"decision":"block","reason":` + js(wiretest.CodexStopNotice) + "}\n",
}

// stdinFor rebuilds a hook stdin for a table row that has none, from the row's request.
func stdinFor(r wire.HookRequest) string {
	m := map[string]any{"session_id": r.SessionID, "cwd": r.Cwd, "stop_hook_active": r.StopHookActive}
	b, _ := json.Marshal(m)
	return string(b)
}

// TestMirrorIPC sends every hook request of the golden table to a socket server that answers with
// the table's reply, and checks the exact line sent and the exact bytes printed.
func TestMirrorIPC(t *testing.T) {
	seen := map[string]bool{}
	for _, row := range wiretest.IPC {
		h, err := wire.ReadHead([]byte(row.Request))
		if (err != nil && !errors.Is(err, wire.ErrVersion)) || h.Op != wire.OpHook {
			continue
		}
		var req wire.HookRequest
		if err := json.Unmarshal([]byte(row.Request), &req); err != nil {
			t.Fatalf("%s: %v", row.Name, err)
		}
		// Only requests this kwak can produce are compared byte for byte.
		ours := req.V == wire.ProtocolVersion && req.Kwak == wire.Version
		stdin := row.Stdin
		if stdin == "" {
			stdin = stdinFor(req)
		}
		var chain func() []proc.ID
		if req.Chain != nil {
			chain = func() []proc.ID { return req.Chain }
		}
		reply := row.Reply
		l := serve(t, func(string) string { return reply })
		var out bytes.Buffer
		code := Run(req.Harness, req.Event, strings.NewReader(stdin), &out, Dial(l.path), chain)
		if code != 0 {
			t.Errorf("%s: exit %d", row.Name, code)
		}
		got := l.lines()
		if len(got) != 1 {
			t.Errorf("%s: %d requests", row.Name, len(got))
			continue
		}
		if ours && got[0] != row.CanonicalRequest() {
			t.Errorf("%s: sent\n %s\nwant\n %s", row.Name, got[0], row.CanonicalRequest())
		}
		if want := expectedStdout[row.Name]; out.String() != want {
			t.Errorf("%s: printed\n %q\nwant\n %q", row.Name, out.String(), want)
		}
		seen[row.Name] = true
	}
	for name := range expectedStdout {
		if !seen[name] {
			t.Errorf("expected stdout for %q, which is not a hook row of the table", name)
		}
	}
	if len(seen) < 20 {
		t.Errorf("only %d hook rows", len(seen))
	}
}
