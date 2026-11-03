package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type call struct {
	tool string
	args string
	meta Meta
}

// recorder is a Backend double that records calls and answers from a table.
type recorder struct {
	mu    sync.Mutex
	calls []call
	reply func(tool string) (string, bool)
}

func (r *recorder) Call(_ context.Context, tool string, args json.RawMessage, m Meta) (string, bool) {
	r.mu.Lock()
	r.calls = append(r.calls, call{tool, string(args), m})
	r.mu.Unlock()
	if r.reply != nil {
		return r.reply(tool)
	}
	return "ok " + tool, false
}

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// transcript feeds lines to Serve and returns every answer line.
func transcript(t *testing.T, b Backend, lines ...string) []msg {
	t.Helper()
	var out bytes.Buffer
	in := strings.Join(lines, "\n") + "\n"
	if err := Serve(context.Background(), strings.NewReader(in), &out, Tools(), b); err != nil {
		t.Fatal(err)
	}
	var msgs []msg
	for _, l := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if l == "" {
			continue
		}
		var m msg
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("answer %q: %v", l, err)
		}
		if m.JSONRPC != "2.0" {
			t.Fatalf("answer %q has no jsonrpc 2.0", l)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func byID(t *testing.T, msgs []msg, id string) msg {
	t.Helper()
	for _, m := range msgs {
		if string(m.ID) == id {
			return m
		}
	}
	t.Fatalf("no answer with id %s in %d answers", id, len(msgs))
	return msg{}
}

type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError"`
}

func resultOf(t *testing.T, m msg) toolResult {
	t.Helper()
	if m.Error != nil {
		t.Fatalf("error %+v", m.Error)
	}
	var r toolResult
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatalf("content %+v", r.Content)
	}
	return r
}

func TestDiscoverThenInitialize(t *testing.T) {
	msgs := transcript(t, &recorder{},
		`{"jsonrpc":"2.0","id":0,"method":"server/discover","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.311"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	)
	if len(msgs) != 3 {
		t.Fatalf("%d answers, want 3 (notifications are not answered)", len(msgs))
	}
	if m := byID(t, msgs, "0"); m.Error == nil || m.Error.Code != CodeMethodNotFound {
		t.Fatalf("server/discover: %+v", m)
	}
	var init initResult
	json.Unmarshal(byID(t, msgs, "1").Result, &init)
	want := initResult{ProtocolVersion: "2025-11-25", Capabilities: map[string]any{"tools": map[string]any{}},
		ServerInfo: serverInfo{"kwak", "0.1.0"}, Instructions: Instructions}
	if !reflect.DeepEqual(init, want) {
		t.Fatalf("initialize: %+v", init)
	}
	var list struct{ Tools []Tool }
	json.Unmarshal(byID(t, msgs, "2").Result, &list)
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "send,reply,thread,helper,volunteer,status" {
		t.Fatalf("tools %v", names)
	}
}

func TestProtocolVersions(t *testing.T) {
	for in, want := range map[string]string{
		`"2025-11-25"`: "2025-11-25", `"2025-06-18"`: "2025-06-18", `"2025-03-26"`: "2025-03-26",
		`"2024-11-05"`: "2024-11-05", `"2099-01-01"`: DefaultProtocol, `7`: DefaultProtocol, `null`: DefaultProtocol,
	} {
		msgs := transcript(t, &recorder{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":`+in+`}}`)
		var r initResult
		json.Unmarshal(msgs[0].Result, &r)
		if r.ProtocolVersion != want {
			t.Errorf("%s: got %q, want %q", in, r.ProtocolVersion, want)
		}
	}
}

func TestToolsTableMatchesSpec(t *testing.T) {
	tools := Tools()
	var raw []map[string]any
	if err := json.Unmarshal(toolsJSON, &raw); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(tools)
	var round []map[string]any
	json.Unmarshal(b, &round)
	if !reflect.DeepEqual(raw, round) {
		t.Fatal("tools/list drops or changes a field of tools.json")
	}
	for _, tl := range tools {
		var schema struct {
			Type                 string `json:"type"`
			AdditionalProperties *bool  `json:"additionalProperties"`
		}
		json.Unmarshal(tl.InputSchema, &schema)
		if schema.Type != "object" || schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("%s: schema %s", tl.Name, tl.InputSchema)
		}
	}
}

func TestIDsAndNotifications(t *testing.T) {
	msgs := transcript(t, &recorder{},
		`{"jsonrpc":"2.0","id":"abc-1","method":"ping"}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":"s-2","method":"tools/call","params":{"name":"status"}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"status"}}`,
		`{"jsonrpc":"2.0","id":99,"result":{}}`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		``,
	)
	if len(msgs) != 4 {
		t.Fatalf("%d answers, want 4", len(msgs))
	}
	if m := byID(t, msgs, `"abc-1"`); string(m.Result) != "{}" {
		t.Fatalf("ping: %s", m.Result)
	}
	if r := resultOf(t, byID(t, msgs, "7")); r.Content[0].Text != "ok status" || r.IsError {
		t.Fatalf("id 7: %+v", r)
	}
	resultOf(t, byID(t, msgs, `"s-2"`))
	byID(t, msgs, "null")
}

func TestToolCallErrors(t *testing.T) {
	rec := &recorder{reply: func(tool string) (string, bool) { return "failed " + tool, true }}
	msgs := transcript(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send","arguments":{"text":"hi"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":"send"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":5}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"send","arguments":[1]}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call"}`,
		`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`,
		`hello`,
		`[{"jsonrpc":"2.0","id":8,"method":"ping"}]`,
		`{"jsonrpc":"2.0","id":9}`,
	)
	if r := resultOf(t, byID(t, msgs, "1")); !r.IsError || r.Content[0].Text != "failed send" {
		t.Fatalf("isError: %+v", r)
	}
	if r := resultOf(t, byID(t, msgs, "2")); !r.IsError || r.Content[0].Text != "kwak has no tool named nope" {
		t.Fatalf("unknown tool: %+v", r)
	}
	for _, id := range []string{"3", "4", "5", "6"} {
		if m := byID(t, msgs, id); m.Error == nil || m.Error.Code != CodeInvalidParams {
			t.Errorf("id %s: %+v", id, m)
		}
	}
	if m := byID(t, msgs, "7"); m.Error == nil || m.Error.Code != CodeMethodNotFound {
		t.Errorf("unknown method: %+v", m)
	}
	if m := byID(t, msgs, "9"); m.Error == nil || m.Error.Code != CodeInvalidRequest {
		t.Errorf("no method: %+v", m)
	}
	var parse, batch bool
	for _, m := range msgs {
		if string(m.ID) == "null" && m.Error != nil {
			parse = parse || m.Error.Code == CodeParse
			batch = batch || m.Error.Code == CodeInvalidRequest
		}
	}
	if !parse || !batch {
		t.Fatalf("parse error %v, batch refused %v", parse, batch)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("backend got %d calls, want 1", len(rec.calls))
	}
}

func TestMetaPassThrough(t *testing.T) {
	rec := &recorder{}
	transcript(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send","arguments":{"text":"hi","to":["agent.bob.0718a1b2c3d4e5f6"]},`+
			`"_meta":{"sessionId":"01a198dd-ad17-79f0-8261-713321e78fea","threadId":"019a2b3c-0000-7000-8000-000000000001","progressToken":3}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"status","_meta":{"sessionId":5}}}`,
	)
	got := map[string]call{}
	for _, c := range rec.calls {
		got[c.tool] = c
	}
	want := call{"send", `{"text":"hi","to":["agent.bob.0718a1b2c3d4e5f6"]}`,
		Meta{"01a198dd-ad17-79f0-8261-713321e78fea", "019a2b3c-0000-7000-8000-000000000001"}}
	if got["send"] != want {
		t.Fatalf("send: %+v", got["send"])
	}
	if got["status"] != (call{"status", "{}", Meta{}}) {
		t.Fatalf("status: %+v", got["status"])
	}
}

func TestLongLines(t *testing.T) {
	rec := &recorder{}
	big := strings.Repeat("a", 2<<20)
	huge := strings.Repeat("b", MaxLine+10)
	msgs := transcript(t, rec,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send","arguments":{"text":"`+big+`"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send","arguments":{"text":"`+huge+`"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	)
	resultOf(t, byID(t, msgs, "1"))
	byID(t, msgs, "3")
	if len(rec.calls) != 1 || len(rec.calls[0].args) != len(big)+11 {
		t.Fatalf("2 MB call not passed whole: %d calls", len(rec.calls))
	}
	var tooLong bool
	for _, m := range msgs {
		tooLong = tooLong || (m.Error != nil && m.Error.Code == CodeParse)
	}
	if !tooLong {
		t.Fatal("no parse error for a line over 8 MiB")
	}
}

// blocker is a Backend that waits until released.
type blocker struct{ release chan struct{} }

func (b blocker) Call(context.Context, string, json.RawMessage, Meta) (string, bool) {
	<-b.release
	return "done", false
}

func TestServeWaitsForCallsInFlight(t *testing.T) {
	b := blocker{make(chan struct{})}
	var out syncBuffer
	done := make(chan error)
	go func() {
		done <- Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status"}}`+"\n"), &out, Tools(), b)
	}()
	select {
	case <-done:
		t.Fatal("Serve returned with a call in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"text":"done"`) {
		t.Fatalf("output %q", out.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestNoHTMLEscaping(t *testing.T) {
	rec := &recorder{reply: func(string) (string, bool) { return "a <b> & &lt;c", false }}
	var out bytes.Buffer
	Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status"}}`+"\n"), &out, Tools(), rec)
	want := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"a <b> & &lt;c"}],"isError":false}}` + "\n"
	if out.String() != want {
		t.Fatalf("got  %s\nwant %s", out.String(), want)
	}
}

func TestReadError(t *testing.T) {
	err := Serve(context.Background(), io.MultiReader(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), errReader{}),
		io.Discard, Tools(), &recorder{})
	if err == nil {
		t.Fatal("no error")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }
