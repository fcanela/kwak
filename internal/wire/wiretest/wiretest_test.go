package wiretest

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/wire"
)

// reencode decodes golden into v and encodes it again.
func reencode(t *testing.T, name, golden string, v any, strict bool) {
	t.Helper()
	var err error
	if strict {
		err = wire.DecodeAPI([]byte(golden), v)
	} else {
		err = wire.Decode([]byte(golden), v)
	}
	if err != nil {
		t.Fatalf("%s: decode %s: %v", name, golden, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: encode: %v", name, err)
	}
	if string(out) != golden {
		t.Fatalf("%s: re-encoded differently\n got %s\nwant %s", name, out, golden)
	}
}

func TestHTTPTableRoundTrips(t *testing.T) {
	names := map[string]bool{}
	for _, e := range HTTP {
		if names[e.Name] {
			t.Errorf("duplicate row name %q", e.Name)
		}
		names[e.Name] = true
		if e.Request != "" {
			v := e.NewRequest()
			if v == nil {
				t.Fatalf("%s: no request type", e.Name)
			}
			if e.BadBody {
				if err := wire.DecodeAPI([]byte(e.Request), v); err == nil {
					t.Errorf("%s: bad body decodes", e.Name)
				}
			} else {
				reencode(t, e.Name, e.Request, v, true)
			}
		}
		if e.Response != "" {
			v := e.NewResponse()
			if v == nil {
				t.Fatalf("%s: no response type", e.Name)
			}
			reencode(t, e.Name, e.Response, v, true)
			if ae, ok := v.(*wire.APIError); ok && ae.Errcode != e.Errcode {
				t.Errorf("%s: body errcode %q, row says %q", e.Name, ae.Errcode, e.Errcode)
			}
		}
	}
}

func TestHTTPTableRules(t *testing.T) {
	routes := map[string]string{
		wire.PathPair: "POST", wire.PathPairClaim: "POST", wire.PathStanding: "POST", wire.PathAgents: "POST",
		wire.PathAgentsClose: "POST", wire.PathAgentsAlive: "POST", wire.PathUnpair: "POST", wire.PathMe: "GET",
		wire.PathReportJoin: "POST",
	}
	success := map[string]bool{}
	unauth := map[string]bool{}
	for _, e := range HTTP {
		route, ok := strings.CutPrefix(e.Path, wire.APIPrefix)
		method, known := routes[route]
		if !ok || !known {
			t.Errorf("%s: unknown path %q", e.Name, e.Path)
			continue
		}
		if e.Method != method {
			t.Errorf("%s: method %s, want %s", e.Name, e.Method, method)
		}
		if (method == "GET") != (e.Request == "") {
			t.Errorf("%s: GET has no body and POST always has one", e.Name)
		}
		if e.Auth != "" && !strings.HasPrefix(e.Auth, "Bearer ") {
			t.Errorf("%s: auth %q is not a bearer", e.Name, e.Auth)
		}
		if route != wire.PathPair && route != wire.PathPairClaim && e.Status < 300 && e.Auth != bearer(OwnerKey) {
			t.Errorf("%s: a keyed route succeeded without the owner key", e.Name)
		}
		if route == wire.PathPairClaim && e.Auth == "" {
			t.Errorf("%s: claim without its secret", e.Name)
		}
		switch {
		case e.Edge:
			if e.Errcode != "" || e.Response != "" || e.RetryAfter == "" {
				t.Errorf("%s: a proxy row has no errcode, no body and a Retry-After", e.Name)
			}
		case e.Status >= 300:
			if wire.StatusFor(e.Errcode) != e.Status {
				t.Errorf("%s: status %d does not go with %q", e.Name, e.Status, e.Errcode)
			}
			if e.Response == "" {
				t.Errorf("%s: error without a body", e.Name)
			}
		default:
			if e.Errcode != "" {
				t.Errorf("%s: success with an errcode", e.Name)
			}
			success[route] = true
		}
		if e.Errcode == wire.ErrBadKey {
			unauth[route] = true
		}
		if e.RetryAfter != "" && !e.Edge {
			t.Errorf("%s: Retry-After outside a proxy row", e.Name)
		}
	}
	for route := range routes {
		if !success[route] {
			t.Errorf("route %s has no success row", route)
		}
		if route != wire.PathPair && route != wire.PathPairClaim && !unauth[route] {
			t.Errorf("route %s has no 401 row", route)
		}
	}
}

// The status, route and errcode pairs plan P0 and spec 5.1 name.
func TestHTTPTableCoversSpec(t *testing.T) {
	want := []struct {
		route   string
		status  int
		errcode string
	}{
		{wire.PathPair, 201, ""},
		{wire.PathPair, 429, ""},
		{wire.PathPairClaim, 202, ""},
		{wire.PathPairClaim, 200, ""},
		{wire.PathPairClaim, 410, wire.ErrExpired},
		{wire.PathAgents, 403, wire.ErrNotYours},
		{wire.PathAgents, 403, wire.ErrParentClosed},
		{wire.PathAgents, 400, wire.ErrBadSession},
		{wire.PathAgents, 409, wire.ErrNamesExhausted},
		{wire.PathAgents, 502, wire.ErrHomeserver},
		{wire.PathAgents, 401, wire.ErrBadKey},
		{wire.PathAgents, 403, wire.ErrOwnerRemoved},
		{wire.PathAgentsClose, 403, wire.ErrNotYours},
		{wire.PathAgentsClose, 400, wire.ErrBadRequest},
		{wire.PathAgentsAlive, 400, wire.ErrBadRequest},
		{wire.PathStanding, 403, wire.ErrOwnerRemoved},
		{wire.PathMe, 403, wire.ErrOwnerRemoved},
		{wire.PathReportJoin, 200, ""},
		{wire.PathReportJoin, 403, wire.ErrNotYours},
		{wire.PathReportJoin, 404, wire.ErrNoReportRoom},
		{wire.PathReportJoin, 401, wire.ErrBadKey},
	}
	for _, w := range want {
		found := false
		for _, e := range HTTP {
			if e.Path == wire.APIPrefix+w.route && e.Status == w.status && e.Errcode == w.errcode {
				found = true
			}
		}
		if !found {
			t.Errorf("no row for %s %d %s", w.route, w.status, w.errcode)
		}
	}
	var pair []Exchange
	for _, e := range HTTP {
		if e.Path == wire.APIPrefix+wire.PathPair && e.Status == 201 {
			pair = append(pair, e)
		}
	}
	auths := map[string]string{}
	for _, e := range pair {
		var r wire.PairResponse
		_ = json.Unmarshal([]byte(e.Response), &r)
		auths[e.Auth] = strconv.FormatBool(r.Replaces)
	}
	if auths[""] != "false" || auths[bearer(OldOwnerKey)] != "true" || auths[bearer(BadKey)] != "false" {
		t.Errorf("/pair rows: replaces by auth = %v", auths)
	}
	var tooMany wire.CloseRequest
	if err := json.Unmarshal([]byte(`{"user_ids":`+TooManyIDs+`}`), &tooMany); err != nil || len(tooMany.UserIDs) != wire.MaxIDs+1 {
		t.Errorf("TooManyIDs has %d ids, %v", len(tooMany.UserIDs), err)
	}
	if n := len(`{"user_ids":` + TooManyIDs + `,"reason":"session_ended"}`); n > wire.MaxBody {
		t.Errorf("TooManyIDs body is %d bytes, over MaxBody: the 400 would be for size", n)
	}
}

// kwakLiterals returns every "KWAK_..." string literal in the wire package's non-test sources.
func kwakLiterals(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no wire sources: %v", err)
	}
	var out []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, err := strconv.Unquote(lit.Value)
				if err == nil && strings.HasPrefix(s, "KWAK_") {
					out = append(out, s)
				}
			}
			return true
		})
	}
	return out
}

func TestEveryErrcodeInHTTPTable(t *testing.T) {
	lits := kwakLiterals(t)
	if len(lits) < 8 {
		t.Fatalf("found only %d errcode literals: %v", len(lits), lits)
	}
	for _, code := range lits {
		if !slices.Contains(wire.Errcodes, code) {
			t.Errorf("%s is declared in wire but missing from wire.Errcodes", code)
		}
	}
	for _, code := range wire.Errcodes {
		if wire.StatusFor(code) == 0 {
			t.Errorf("%s has no HTTP status", code)
		}
		found := false
		for _, e := range HTTP {
			if e.Errcode == code {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not appear in the HTTP table", code)
		}
	}
}

func TestFixturesFollowNameRules(t *testing.T) {
	cases := []struct {
		h       wire.Harness
		session string
		agent   string
	}{
		{wire.HarnessClaude, ClaudeSession, ClaudeAgent},
		{wire.HarnessCodex, CodexSession, CodexAgent},
	}
	for _, c := range cases {
		hex, err := wire.SessionHex(c.session)
		if err != nil {
			t.Fatal(err)
		}
		if got := "@" + wire.Candidates(Owner, c.h, hex)[0] + ":" + ServerName; got != c.agent {
			t.Errorf("%s agent fixture %q, rules give %q", c.h, c.agent, got)
		}
	}
	parent := strings.TrimSuffix(strings.TrimPrefix(ClaudeAgent, "@"), ":"+ServerName)
	if lp, err := wire.HelperLocalpart(parent, "tests"); err != nil || "@"+lp+":"+ServerName != HelperAgent {
		t.Errorf("helper fixture %q, rules give %q, %v", HelperAgent, lp, err)
	}
	if got := "@" + wire.ListenerLocalpart(Owner, "brave-heron", false) + ":" + ServerName; got != StandingUser {
		t.Errorf("listener fixture %q, rules give %q", StandingUser, got)
	}
	if got := "@" + wire.ListenerLocalpart(Owner, "calm-otter", true) + ":" + ServerName; got != TestStanding {
		t.Errorf("test listener fixture %q, rules give %q", TestStanding, got)
	}
	if got := "@" + LegacyStandingLocalpart(Owner, false) + ":" + ServerName; got != LegacyStanding {
		t.Errorf("old standing fixture %q, helper gives %q", LegacyStanding, got)
	}
	// The per-owner standing accounts of the builds before decision 42 parse as standing accounts
	// with no word pair, which is how the registrar's job pass finds them (decision 43).
	for _, test := range []bool{false, true} {
		lp := LegacyStandingLocalpart(Owner, test)
		if n, err := wire.ParseLocalpart(lp); err != nil || n != (wire.Name{Kind: wire.KindStanding, Owner: Owner, Test: test}) {
			t.Errorf("old standing account %q parses as %+v, %v", lp, n, err)
		}
	}
	for _, key := range []string{OwnerKey, OldOwnerKey, BadKey} {
		if !regexp.MustCompile(`^kwak_ok_[A-Za-z0-9_-]{43}$`).MatchString(key) {
			t.Errorf("key fixture %q is not kwak_ok_ plus 43 base64url", key)
		}
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(Claim) {
		t.Errorf("claim fixture %q is not 43 base64url", Claim)
	}
}

const (
	realServer = "acceso" + "prohibido"
	emDash     = rune(0x2014)
)

func TestTablesNameNoRealServer(t *testing.T) {
	var all []string
	for _, e := range HTTP {
		all = append(all, e.Name, e.Auth, e.Request, e.Response)
	}
	for _, c := range IPC {
		all = append(all, c.Name, c.Stdin, c.Request, c.Canonical, c.Reply)
	}
	for _, s := range all {
		if strings.Contains(strings.ToLower(s), realServer) {
			t.Errorf("table names the real server: %.80s", s)
		}
		if strings.ContainsRune(s, emDash) {
			t.Errorf("table holds an em-dash: %.80s", s)
		}
	}
}

func TestIPCTableRoundTrips(t *testing.T) {
	names := map[string]bool{}
	for _, c := range IPC {
		if names[c.Name] {
			t.Errorf("duplicate row name %q", c.Name)
		}
		names[c.Name] = true
		h, herr := wire.ReadHead([]byte(c.Request))
		switch {
		case errors.Is(herr, wire.ErrBadMessage):
			if !c.Early || c.Reply != mustJSON(t, wire.Refuse(wire.IPCErrBadInput)) {
				t.Errorf("%s: an unreadable head is refused early with bad_input", c.Name)
			}
		case errors.Is(herr, wire.ErrVersion):
			if !c.Early || c.Reply != mustJSON(t, wire.Refuse(wire.IPCErrVersion)) {
				t.Errorf("%s: v %d is refused early with version", c.Name, h.V)
			}
		case herr != nil:
			t.Fatalf("%s: %v", c.Name, herr)
		}
		if req := c.NewRequest(); req != nil {
			if c.Canonical != "" {
				if c.Canonical == c.Request {
					t.Errorf("%s: Canonical equals Request", c.Name)
				}
				if err := wire.Decode([]byte(c.Request), req); err != nil {
					t.Fatalf("%s: %v", c.Name, err)
				}
				if got := mustJSON(t, req); got != c.Canonical {
					t.Errorf("%s: tolerant decode\n got %s\nwant %s", c.Name, got, c.Canonical)
				}
				req = c.NewRequest()
			}
			reencode(t, c.Name, c.CanonicalRequest(), req, false)
		}
		reencode(t, c.Name, c.Reply, c.NewReply(), false)
		if c.Early {
			var a wire.Ack
			_ = json.Unmarshal([]byte(c.Reply), &a)
			if a.OK || a.Err == "" || a.Kwak != "" {
				t.Errorf("%s: an early refusal is ok:false with err and no kwak", c.Name)
			}
		}
		if h.Op == wire.OpTool && herr == nil {
			var req wire.ToolRequest
			var rep wire.ToolResponse
			_ = json.Unmarshal([]byte(c.Request), &req)
			_ = json.Unmarshal([]byte(c.Reply), &rep)
			if req.ID != rep.ID {
				t.Errorf("%s: reply id %d, request id %d", c.Name, rep.ID, req.ID)
			}
			if !rep.OK && (rep.Err == "" || rep.Text != "" || rep.IsError) {
				t.Errorf("%s: an IPC failure carries only err", c.Name)
			}
		}
	}
}

func TestIPCStdinGivesRequest(t *testing.T) {
	for _, c := range IPC {
		if c.Stdin == "" {
			continue
		}
		var want wire.HookRequest
		if err := json.Unmarshal([]byte(c.CanonicalRequest()), &want); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		got, eventName, err := wire.ParseHookStdin(want.Harness, want.Event, []byte(c.Stdin))
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		got.Chain = want.Chain
		if s := mustJSON(t, got); s != c.CanonicalRequest() {
			t.Errorf("%s: from stdin\n got %s\nwant %s", c.Name, s, c.CanonicalRequest())
		}
		var stdin map[string]any
		_ = json.Unmarshal([]byte(c.Stdin), &stdin)
		if stdin["hook_event_name"] != eventName || eventName == "" {
			t.Errorf("%s: hook_event_name %q", c.Name, eventName)
		}
		if _, err := wire.ValidSessionID(got.SessionID); err != nil {
			t.Errorf("%s: stdin session id %q", c.Name, got.SessionID)
		}
	}
}

func TestIPCTableCoversSpec(t *testing.T) {
	ops := map[string]bool{}
	events := map[string]bool{}
	tools := map[string]bool{}
	errs := map[string]bool{}
	stdinEvents := map[string]bool{}
	var unknownStdin, unknownIPC, windowsHook, windowsHello bool
	for _, c := range IPC {
		var head struct {
			wire.Head
			Harness string          `json:"harness"`
			Event   string          `json:"event"`
			Name    string          `json:"name"`
			Chain   []proc.ID       `json:"chain"`
			Args    json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal([]byte(c.Request), &head); err == nil && !c.Early {
			ops[head.Op] = true
			if head.Op == wire.OpHook {
				events[head.Harness+" "+head.Event] = true
				if len(head.Chain) > 0 {
					windowsHook = true
				}
			}
			if head.Op == wire.OpMCPHello && len(head.Chain) > 0 {
				windowsHello = true
			}
			if head.Op == wire.OpTool {
				tools[head.Name] = true
			}
		}
		var r struct {
			Err string `json:"err"`
		}
		_ = json.Unmarshal([]byte(c.Reply), &r)
		errs[r.Err] = true
		if c.Stdin != "" {
			var m map[string]any
			_ = json.Unmarshal([]byte(c.Stdin), &m)
			stdinEvents[head.Harness+" "+m["hook_event_name"].(string)] = true
			if _, ok := m["effort"]; ok {
				unknownStdin = true
			}
		}
		if c.Canonical != "" {
			unknownIPC = true
		}
	}
	for _, op := range []string{wire.OpHook, wire.OpMCPHello, wire.OpTool, wire.OpStatus, wire.OpInbox, wire.OpReload, wire.OpShutdown} {
		if !ops[op] {
			t.Errorf("no row for op %s", op)
		}
	}
	for _, h := range []wire.Harness{wire.HarnessClaude, wire.HarnessCodex} {
		for _, ev := range []string{wire.EventSessionStart, wire.EventPrompt, wire.EventPostTool, wire.EventStop, wire.EventSubagentStop, wire.EventSessionEnd} {
			if !events[string(h)+" "+ev] {
				t.Errorf("no hook row for %s %s", h, ev)
			}
		}
	}
	for _, name := range []string{"claude PostToolUse", "claude PostToolUseFailure", "codex PostToolUse"} {
		if !stdinEvents[name] {
			t.Errorf("no stdin row for %s", name)
		}
	}
	for _, tool := range wire.Tools {
		if !tools[tool] {
			t.Errorf("no tool row for %s", tool)
		}
	}
	for _, e := range []string{wire.IPCErrVersion, wire.IPCErrBadOp, wire.IPCErrBadInput, wire.IPCErrUnpaired, wire.IPCErrInternal} {
		if !errs[e] {
			t.Errorf("no reply with err %q", e)
		}
	}
	if !unknownStdin || !unknownIPC {
		t.Errorf("unknown fields: stdin %v, ipc %v", unknownStdin, unknownIPC)
	}
	if !windowsHook || !windowsHello {
		t.Errorf("windows chain rows: hook %v, mcp_hello %v", windowsHook, windowsHello)
	}
	var b bytes.Buffer
	for _, c := range IPC {
		b.WriteString(c.Reply)
	}
	for _, text := range []string{CodexStopNotice, TextUnpaired, TextNotSeenYet, TextNoAgentYet} {
		if !strings.Contains(b.String(), js(text)[1:len(js(text))-1]) {
			t.Errorf("no reply carries %q", text)
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

func TestExchangeCheck(t *testing.T) {
	var agentNew, alive, notYours Exchange
	for _, e := range HTTP {
		switch e.Name {
		case "agents: new Claude session":
			agentNew = e
		case "alive: all open":
			alive = e
		case "agents: helper whose parent is closed":
			notYours = e
		}
	}
	ok := []struct {
		e      Exchange
		status int
		body   string
	}{
		{agentNew, 200, agentNew.Response},
		{agentNew, 200, `{"user_id":"` + ClaudeAgent + `","access_token":"other","device_id":"KWAK","room_id":"!x","new":true,"reopened":false}`},
		{alive, 200, `{"unknown":["@a:b"],"closed":[]}`},
		{notYours, 403, `{"errcode":"KWAK_PARENT_CLOSED","error":"other words"}`},
	}
	for _, c := range ok {
		if err := c.e.Check(c.status, []byte(c.body)); err != nil {
			t.Errorf("%s: %v", c.e.Name, err)
		}
	}
	bad := []struct {
		e      Exchange
		status int
		body   string
	}{
		{agentNew, 201, agentNew.Response},
		{agentNew, 200, `{"user_id":"` + ClaudeAgent + `","access_token":"x","device_id":"KWAK","room_id":"!x","new":false,"reopened":false}`},
		{agentNew, 200, `{"user_id":"@agent.fcanela.x:kwak.test","access_token":"x","device_id":"KWAK","room_id":"!x","new":true,"reopened":false}`},
		{agentNew, 200, `{"user_id":"` + ClaudeAgent + `","access_token":"x","device_id":"KWAK","room_id":"!x","new":true}`},
		{agentNew, 200, `{"user_id":"` + ClaudeAgent + `","access_token":"x","device_id":"KWAK","room_id":"!x","new":true,"reopened":false,"extra":1}`},
		{agentNew, 200, `{"user_id":"` + ClaudeAgent + `","access_token":7,"device_id":"KWAK","room_id":"!x","new":true,"reopened":false}`},
		{alive, 200, `{"unknown":null,"closed":[]}`},
		{alive, 200, `{"unknown":[],"closed":[]} {}`},
		{alive, 200, `{"unknown":["@a:b"],"closed":{}}`},
		{notYours, 403, `{"errcode":"KWAK_NOT_YOURS","error":"x"}`},
		{notYours, 403, `not json`},
	}
	for _, c := range bad {
		if err := c.e.Check(c.status, []byte(c.body)); err == nil {
			t.Errorf("%s: accepted %d %s", c.e.Name, c.status, c.body)
		}
	}
	edge := HTTP[slices.IndexFunc(HTTP, func(e Exchange) bool { return e.Edge })]
	if err := edge.Check(429, []byte("<html>slow down</html>")); err != nil {
		t.Errorf("edge row: %v", err)
	}
}

func TestSameShapeArrays(t *testing.T) {
	if err := SameShape([]byte(`{"a":[{"x":1}]}`), []byte(`{"a":[{"x":2},{"y":1}]}`)); err != nil {
		t.Errorf("first element matches: %v", err)
	}
	if err := SameShape([]byte(`{"a":[{"x":1}]}`), []byte(`{"a":[{"y":2}]}`)); err == nil {
		t.Errorf("element with other keys accepted")
	}
	if err := SameShape([]byte(`{"a":[]}`), []byte(`{"a":[1]}`)); err != nil {
		t.Errorf("empty golden array: %v", err)
	}
}
