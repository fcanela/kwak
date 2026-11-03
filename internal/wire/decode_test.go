package wire

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestReadHead(t *testing.T) {
	cases := []struct {
		line string
		want Head
		err  error
	}{
		{`{"v":1,"op":"status"}`, Head{1, "status"}, nil},
		{`{"op":"hook","v":1,"extra":{"a":[1]}}`, Head{1, "hook"}, nil},
		{`{"v":2,"op":"status"}`, Head{2, "status"}, ErrVersion},
		{`{"v":99}`, Head{99, ""}, ErrVersion},
		{`{"op":"status"}`, Head{0, "status"}, ErrBadMessage},
		{`{"v":0,"op":"status"}`, Head{0, "status"}, ErrBadMessage},
		{`not json`, Head{}, ErrBadMessage},
		{`{"v":"1"}`, Head{}, ErrBadMessage},
		{`[]`, Head{}, ErrBadMessage},
	}
	for _, c := range cases {
		got, err := ReadHead([]byte(c.line))
		if got != c.want || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Errorf("ReadHead(%s) = %+v, %v; want %+v, %v", c.line, got, err, c.want, c.err)
		}
	}
}

func TestRefuse(t *testing.T) {
	b, err := json.Marshal(Refuse(IPCErrVersion))
	if err != nil || string(b) != `{"v":1,"ok":false,"err":"version"}` {
		t.Fatalf("Refuse = %s, %v", b, err)
	}
}

func TestDecodeTolerant(t *testing.T) {
	var r ToolRequest
	err := Decode([]byte(`{"v":1,"op":"tool","id":3,"name":"status","args":{},"future":true,"meta_thread":"x"}`), &r)
	if err != nil || r.ID != 3 || r.Name != "status" || r.MetaThread != "x" || string(r.Args) != "{}" {
		t.Fatalf("Decode = %+v, %v", r, err)
	}
	if err := Decode([]byte(`{"id":"seven"}`), &r); !errors.Is(err, ErrBadMessage) {
		t.Fatalf("type mismatch: %v", err)
	}
}

func TestDecodeAPI(t *testing.T) {
	var r AgentRequest
	ok := `{"request_id":"00112233445566778899aabbccddeeff","harness":"claude","session":"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d","helper":""}`
	if err := DecodeAPI([]byte(ok), &r); err != nil || r.Harness != HarnessClaude {
		t.Fatalf("DecodeAPI = %+v, %v", r, err)
	}
	bad := []string{
		``,
		`{`,
		`{"request_id":"x","extra":1}`,
		ok + ok,
		ok + ` {}`,
		ok + `}`,
		ok + ` ]`,
		`{}}`,
		`{} ]`,
		`{"harness":7}`,
		`{"request_id":"` + strings.Repeat("a", MaxBody) + `"}`,
	}
	for _, b := range bad {
		var r AgentRequest
		if err := DecodeAPI([]byte(b), &r); !errors.Is(err, ErrBadMessage) {
			n := min(len(b), 60)
			t.Errorf("DecodeAPI(%s) = %v; want ErrBadMessage", b[:n], err)
		}
	}
	if err := DecodeAPI([]byte(ok+"\n"), &r); err != nil {
		t.Errorf("trailing newline refused: %v", err)
	}
	var e Empty
	if err := DecodeAPI([]byte(`{}`), &e); err != nil {
		t.Errorf("{} into Empty: %v", err)
	}
	if err := DecodeAPI([]byte(`{"a":1}`), &e); err == nil {
		t.Errorf("unknown field into Empty accepted")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"0.1.0", "0.2.0", -1},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.1.1", "0.1.0", 1},
	}
	for _, c := range cases {
		got, err := CompareVersions(c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, %v; want %d", c.a, c.b, got, err, c.want)
		}
	}
	for _, v := range []string{"", "1", "1.2", "1.2.3.4", "v1.2.3", "1.2.x", "1.-2.3", "01.2.3", "1.2.3-rc1", "+1.2.3"} {
		if _, err := CompareVersions(v, "0.1.0"); err == nil {
			t.Errorf("CompareVersions(%q) accepted", v)
		}
		if _, err := CompareVersions("0.1.0", v); err == nil {
			t.Errorf("CompareVersions(_, %q) accepted", v)
		}
	}
	if _, err := CompareVersions(Version, Version); err != nil {
		t.Errorf("Version %q does not parse: %v", Version, err)
	}
}

func TestToolFromName(t *testing.T) {
	cases := []struct {
		h    Harness
		name string
		want string
		ok   bool
	}{
		{HarnessClaude, "mcp__plugin_kwak_kwak__helper", "helper", true},
		{HarnessClaude, "mcp__plugin_kwak_kwak__send", "send", true},
		{HarnessCodex, "mcp__kwak__thread", "thread", true},
		{HarnessCodex, "mcp__plugin_kwak_kwak__helper", "", false},
		{HarnessClaude, "mcp__kwak__helper", "", false},
		{HarnessClaude, "mcp__plugin_kwak_kwak__rm", "", false},
		{HarnessClaude, "Bash", "", false},
		{HarnessClaude, "", "", false},
		{"gemini", "mcp__kwak__send", "", false},
	}
	for _, c := range cases {
		got, ok := ToolFromName(c.h, c.name)
		if got != c.want || ok != c.ok {
			t.Errorf("ToolFromName(%q, %q) = %q, %v", c.h, c.name, got, ok)
		}
	}
	for _, h := range []Harness{HarnessClaude, HarnessCodex} {
		for _, tool := range Tools {
			if got, ok := ToolFromName(h, ToolPrefix(h)+tool); !ok || got != tool {
				t.Errorf("%s %s does not round-trip", h, tool)
			}
		}
	}
}

func TestParseHookStdin(t *testing.T) {
	stdin := `{"session_id":"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d","hook_event_name":"PostToolUseFailure",
		"cwd":"/w","tool_name":"Bash","tool_input":{"command":"false"},"error":"exit 1","v":9,"op":"x",
		"kwak":"9.9.9","harness":"codex","event":"stop","chain":[{"pid":1,"start":2}]}`
	r, name, err := ParseHookStdin(HarnessClaude, EventPostTool, []byte(stdin))
	if err != nil {
		t.Fatal(err)
	}
	want := HookRequest{V: 1, Kwak: Version, Op: OpHook, Harness: HarnessClaude, Event: EventPostTool,
		SessionID: "4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d", Cwd: "/w", ToolName: "Bash"}
	if name != "PostToolUseFailure" || r.ToolInput != nil || r.Chain != nil {
		t.Fatalf("got %+v %q", r, name)
	}
	r.ToolInput, r.Chain = nil, nil
	if b1, b2 := mustJSON(t, r), mustJSON(t, want); b1 != b2 {
		t.Fatalf("got  %s\nwant %s", b1, b2)
	}
	r, _, err = ParseHookStdin(HarnessCodex, EventPostTool, []byte(`{"tool_name":"mcp__kwak__helper","tool_input":{"name":"tests"}}`))
	if err != nil || string(r.ToolInput) != `{"name":"tests"}` {
		t.Fatalf("kwak tool input dropped: %+v %v", r, err)
	}
	for _, c := range []struct {
		h     Harness
		event string
		stdin string
	}{
		{"gemini", EventPrompt, `{}`},
		{HarnessClaude, "PostToolUse", `{}`},
		{HarnessClaude, EventPrompt, ``},
		{HarnessClaude, EventPrompt, `{"session_id":`},
		{HarnessClaude, EventPrompt, `[]`},
		{HarnessClaude, EventPrompt, `"x"`},
	} {
		if _, _, err := ParseHookStdin(c.h, c.event, []byte(c.stdin)); err == nil {
			t.Errorf("ParseHookStdin(%q, %q, %q) accepted", c.h, c.event, c.stdin)
		}
	}
}

func TestParseHookStdinWrongTypes(t *testing.T) {
	sid := "4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d"
	for _, c := range []struct {
		name  string
		stdin string
		check func(HookRequest, string) bool
	}{
		{"source number", `{"session_id":"` + sid + `","source":7}`,
			func(r HookRequest, _ string) bool { return r.Source == "" && r.SessionID == sid }},
		{"stop_hook_active string", `{"session_id":"` + sid + `","stop_hook_active":"false"}`,
			func(r HookRequest, _ string) bool { return !r.StopHookActive && r.SessionID == sid }},
		{"stop_hook_active true string", `{"stop_hook_active":"true","cwd":"/w"}`,
			func(r HookRequest, _ string) bool { return !r.StopHookActive && r.Cwd == "/w" }},
		{"session_id object", `{"session_id":{"a":1},"source":"startup"}`,
			func(r HookRequest, _ string) bool { return r.SessionID == "" && r.Source == "startup" }},
		{"hook_event_name number", `{"hook_event_name":3,"session_id":"` + sid + `"}`,
			func(r HookRequest, name string) bool { return name == "" && r.SessionID == sid }},
		{"tool_name array", `{"tool_name":["mcp__plugin_kwak_kwak__helper"],"tool_input":{"name":"x"}}`,
			func(r HookRequest, _ string) bool { return r.ToolName == "" && r.ToolInput == nil }},
		{"tool_input string", `{"tool_name":"mcp__plugin_kwak_kwak__helper","tool_input":"x"}`,
			func(r HookRequest, _ string) bool { return r.ToolName != "" && r.ToolInput == nil }},
		{"tool_input null", `{"tool_name":"mcp__plugin_kwak_kwak__helper","tool_input":null}`,
			func(r HookRequest, _ string) bool { return r.ToolInput == nil }},
		{"protocol fields of other types", `{"v":"x","op":1,"chain":"y","harness":2,"cwd":"/w"}`,
			func(r HookRequest, _ string) bool {
				return r.V == ProtocolVersion && r.Op == OpHook && r.Harness == HarnessClaude && r.Chain == nil && r.Cwd == "/w"
			}},
		{"null stdin", `null`,
			func(r HookRequest, _ string) bool { return r.SessionID == "" && r.Op == OpHook }},
	} {
		r, name, err := ParseHookStdin(HarnessClaude, EventStop, []byte(c.stdin))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !c.check(r, name) {
			t.Errorf("%s: got %+v %q", c.name, r, name)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestValidHarnessAndEvent(t *testing.T) {
	if !ValidHarness(HarnessClaude) || !ValidHarness(HarnessCodex) || ValidHarness("") || ValidHarness("Claude") {
		t.Error("ValidHarness")
	}
	for _, e := range []string{EventSessionStart, EventPrompt, EventPostTool, EventStop, EventSubagentStop, EventSessionEnd} {
		if !ValidEvent(e) {
			t.Errorf("ValidEvent(%q) = false", e)
		}
	}
	for _, e := range []string{"", "Stop", "post_tool", "pre-tool"} {
		if ValidEvent(e) {
			t.Errorf("ValidEvent(%q) = true", e)
		}
	}
}
