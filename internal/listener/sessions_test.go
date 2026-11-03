package listener

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

const cwd = wiretest.ClaudeCwd

var k1, k2 = sessionKey("claude", sid1), sessionKey("claude", sid2)

func TestLinkHookFirst(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	e.child(h, 120)
	e.start(110, "claude", sid1, "startup", cwd)
	if s := e.session(k1); s.HarnessProc != nil {
		t.Fatalf("harness proc set before the MCP hello: %+v", s.HarnessProc)
	}
	c := e.mcp(120, "claude", "")
	if s := e.session(k1); s.HarnessProc == nil || *s.HarnessProc != h || !s.Tools {
		t.Fatalf("hello did not link: %+v", s)
	}
	e.waitAgent(k1)
	if r := e.tool(c, "status", `{}`); !strings.HasPrefix(r.Text, "Agent: @agent.fcanela.4f2a9c1e7b034d5a\n") {
		t.Fatalf("status: %+v", r)
	}
}

func TestLinkMCPFirst(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	e.child(h, 120)
	c := e.mcp(120, "claude", "")
	e.clk.instant = true
	if r := e.tool(c, "status", `{}`); r.Text != textNotSeen || !r.IsError {
		t.Fatalf("before the hook: %+v", r)
	}
	e.clk.instant = false
	e.start(110, "claude", sid1, "startup", cwd)
	if s := e.session(k1); s.HarnessProc == nil || *s.HarnessProc != h || !s.Tools {
		t.Fatalf("session start did not attach the open MCP connection: %+v", s)
	}
}

func TestToolWaitsForSessionStart(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	c := e.mcp(e.child(h, 120).PID, "claude", "")
	got := make(chan wire.ToolResponse)
	go func() { got <- e.tool(c, "volunteer", `{"on":true}`) }()
	eventually(t, "the tool to wait", func() bool { return e.clk.waiting() > 0 })
	e.start(110, "claude", sid1, "startup", cwd)
	if r := <-got; r.IsError {
		t.Fatalf("tool after a late session start: %+v", r)
	}
}

func TestClearNewestSessionWins(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	c := e.mcp(e.child(h, 120).PID, "claude", sid1)
	e.start(110, "claude", sid1, "startup", cwd)
	e.clk.Advance(time.Minute)
	e.start(110, "claude", sid2, "clear", cwd)
	if s := e.session(k1); s.Ended == nil {
		t.Fatal("/clear did not end the old session")
	}
	e.tool(c, "volunteer", `{"on":true}`)
	if !e.session(k2).Volunteer || e.session(k1).Volunteer {
		t.Fatal("the tool call did not go to the newest session")
	}
}

func TestNestedHarnessesStayApart(t *testing.T) {
	e := newEnv(t)
	e.connect()
	outer := e.claudeTree(100)
	e.child(outer, 110)
	bash := e.child(outer, 200)
	inner := e.child(bash, 300)
	e.child(inner, 310)
	ci := e.mcp(e.child(inner, 320).PID, "claude", "")
	co := e.mcp(e.child(outer, 120).PID, "claude", "")
	e.start(110, "claude", sid1, "startup", cwd)
	e.start(310, "claude", sid2, "startup", cwd)
	e.tool(ci, "volunteer", `{"on":true}`)
	if !e.session(k2).Volunteer || e.session(k1).Volunteer {
		t.Fatal("inner MCP reached the outer session")
	}
	e.tool(co, "volunteer", `{"on":true}`)
	if !e.session(k1).Volunteer {
		t.Fatal("outer MCP did not reach its session")
	}
	uid := e.waitAgent(k1)
	e.deliver(event("$m", bob, e.clk.Now(), text("q", uid)))
	if r := e.hook(310, hookReq("claude", wire.EventPrompt, sid1, cwd)); r.Text != "" {
		t.Fatal("the inner harness drained the outer session's inbox")
	}
	if r := e.hook(110, hookReq("claude", wire.EventPrompt, sid1, cwd)); r.Text == "" {
		t.Fatal("the outer harness got nothing")
	}
}

func TestCodexMetaRouting(t *testing.T) {
	e := newEnv(t)
	e.connect()
	app := proc.ID{PID: 500, Start: 500}
	e.procs.Add(app, proc.ID{})
	sh := e.child(app, 510)
	e.child(sh, 511)
	sh2 := e.child(app, 520)
	e.child(sh2, 521)
	c := e.mcp(e.child(app, 530).PID, "codex", "")
	e.start(511, "codex", cx1, "startup", wiretest.CodexCwd)
	e.clk.Advance(time.Minute)
	e.start(521, "codex", cx2, "startup", wiretest.CodexCwd)
	e.tool(c, "volunteer", `{"on":true}`, cx1)
	if !e.session(sessionKey("codex", cx1)).Volunteer || e.session(sessionKey("codex", cx2)).Volunteer {
		t.Fatal("_meta.sessionId did not pick the session")
	}
}

func TestCodexToolBeforeAnyHook(t *testing.T) {
	e := newEnv(t)
	e.connect()
	app := proc.ID{PID: 500, Start: 500}
	e.procs.Add(app, proc.ID{})
	c := e.mcp(e.child(app, 530).PID, "codex", "")
	if r := e.tool(c, "volunteer", `{"on":true}`, cx1); r.IsError {
		t.Fatalf("untrusted hooks: %+v", r)
	}
	st := e.l.status(&client{pidOK: true})
	if len(st.Sessions) != 1 || st.Sessions[0].Note != "no hook seen for session codex:"+cx1 {
		t.Fatalf("status: %+v", st.Sessions)
	}
}

func TestForgedSessionIDGetsNothing(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	e.start(110, "claude", sid1, "startup", cwd)
	uid := e.waitAgent(k1)
	e.deliver(event("$m", bob, e.clk.Now(), text("q", uid)))
	rogue := e.claudeTree(900)
	e.child(rogue, 910)
	if r := e.hook(910, hookReq("claude", wire.EventPrompt, sid1, cwd)); r.Text != "" || r.Err != "" {
		t.Fatalf("forged prompt: %+v", r)
	}
	if n := len(e.items(k1)); n != 1 {
		t.Fatalf("the forged hook drained %d", 1-n)
	}
}

func TestForgedResumeChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	e.mcp(e.child(h, 120).PID, "claude", "")
	e.start(110, "claude", sid1, "startup", cwd)
	uid := e.waitAgent(k1)
	e.deliver(event("$m", bob, e.clk.Now(), text("q", uid)))
	rogue := e.claudeTree(900)
	e.child(rogue, 910)
	if r := e.start(910, "claude", sid1, "resume", cwd); r.Text != "" {
		t.Fatalf("forged resume printed %q", r.Text)
	}
	if s := e.session(k1); s.Candidates[0] != h || *s.HarnessProc != h || len(s.Inbox.Items) != 1 {
		t.Fatalf("forged resume changed the session: %+v", s)
	}
}

func TestCodexHarnessProcIsNotTheShell(t *testing.T) {
	e := newEnv(t)
	e.connect()
	codex := proc.ID{PID: 300, Start: 300}
	e.procs.Line(codex, proc.ID{PID: 299, Start: 299})
	sh := e.child(codex, 310)
	e.child(sh, 311)
	e.start(311, "codex", cx1, "startup", wiretest.CodexCwd)
	e.procs.Kill(311)
	e.procs.Kill(310)
	sh2 := e.child(codex, 320)
	e.child(sh2, 321)
	e.hook(321, hookReq("codex", wire.EventPrompt, cx1, wiretest.CodexCwd))
	s := e.session(sessionKey("codex", cx1))
	if s.HarnessProc == nil || *s.HarnessProc != codex {
		t.Fatalf("harness proc %+v, want codex %+v", s.HarnessProc, codex)
	}
}

func TestMCPCloseWithLiveHarnessKeepsSession(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	c := e.mcp(e.child(h, 120).PID, "claude", "")
	e.start(110, "claude", sid1, "startup", cwd)
	e.l.mcpClosed(c)
	if s := e.session(k1); s.Ended != nil || s.Tools {
		t.Fatalf("live harness: %+v", s)
	}
	c = e.mcp(e.child(h, 121).PID, "claude", "")
	if !e.session(k1).Tools {
		t.Fatal("a new hello did not re-attach")
	}
	e.procs.Kill(100)
	e.l.mcpClosed(c)
	if e.session(k1).Ended == nil {
		t.Fatal("dead harness: session not ended")
	}
}

func TestHelloInsideGraceCancelsClose(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	e.mcp(e.child(h, 120).PID, "claude", "")
	e.start(110, "claude", sid1, "startup", cwd)
	e.waitAgent(k1)
	e.hook(110, hookReq("claude", wire.EventSessionEnd, sid1, cwd))
	e.clk.Advance(5 * time.Minute)
	e.mcp(e.child(h, 121).PID, "claude", "")
	if s := e.session(k1); s.Ended != nil || s.CloseDue != nil {
		t.Fatalf("hello inside the grace: %+v", s)
	}
	e.clk.Advance(10 * time.Minute)
	e.l.deadlines()
	if len(e.reg.closes) != 0 {
		t.Fatalf("closed after a re-attach: %v", e.reg.closes)
	}
}

func TestConnectionCloseEndsOnlyDeadHarness(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h1, h2 := e.claudeTree(100), e.claudeTree(200)
	e.child(h1, 110)
	e.child(h2, 210)
	c1 := e.mcp(e.child(h1, 120).PID, "claude", "")
	e.mcp(e.child(h2, 220).PID, "claude", "")
	e.start(110, "claude", sid1, "startup", cwd)
	e.start(110, "claude", sid3, "clear", cwd)
	e.start(210, "claude", sid2, "startup", cwd)
	e.procs.Kill(100)
	e.l.mcpClosed(c1)
	if e.session(sessionKey("claude", sid3)).Ended == nil || e.session(k1).Ended == nil {
		t.Fatal("sessions of the dead harness still live")
	}
	if e.session(k2).Ended != nil {
		t.Fatal("the other harness's session ended")
	}
}

func TestLivenessTickerEndsDeadHarness(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	e.mcp(e.child(h, 120).PID, "claude", "")
	e.start(110, "claude", sid1, "startup", cwd)
	e.l.liveness()
	if e.session(k1).Ended != nil {
		t.Fatal("ended while alive")
	}
	e.procs.Kill(100)
	e.l.liveness()
	if e.session(k1).Ended == nil {
		t.Fatal("ticker missed the dead harness")
	}
}

func TestSourcesAndAgents(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	e.mcp(e.child(h, 120).PID, "claude", "")
	e.start(110, "claude", sid1, "startup", cwd)
	uid := e.waitAgent(k1)
	if uid != wiretest.ClaudeAgent || e.reg.agentCalls() != 1 {
		t.Fatalf("startup: %s, %d calls", uid, e.reg.agentCalls())
	}
	e.start(110, "claude", sid1, "compact", cwd)
	time.Sleep(10 * time.Millisecond)
	if e.reg.agentCalls() != 1 {
		t.Fatal("compact minted")
	}
	e.hook(110, hookReq("claude", wire.EventSessionEnd, sid1, cwd))
	e.clk.Advance(closeGrace)
	e.l.deadlines()
	if len(e.reg.closes) != 1 || e.reg.closes[0][0] != uid || !e.session(k1).AgentClosed {
		t.Fatalf("close after grace: %v", e.reg.closes)
	}
	e.start(110, "claude", sid1, "resume", cwd)
	if got := e.waitAgent(k1); got != uid || e.reg.agentCalls() != 2 {
		t.Fatalf("resume: %s, %d calls", got, e.reg.agentCalls())
	}
	e.start(110, "claude", sid2, "clear", cwd)
	if e.waitAgent(k2) == uid || e.session(k1).Ended == nil {
		t.Fatal("clear: no new agent, or the old session lives on")
	}
}

func TestResumeCancelsClose(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	e.start(110, "claude", sid1, "startup", cwd)
	e.waitAgent(k1)
	e.hook(110, hookReq("claude", wire.EventSessionEnd, sid1, cwd))
	e.clk.Advance(9 * time.Minute)
	e.start(110, "claude", sid1, "resume", cwd)
	e.clk.Advance(5 * time.Minute)
	e.l.deadlines()
	if len(e.reg.closes) != 0 {
		t.Fatal("closed after a resume inside the grace")
	}
}

func TestEnsureSingleFlight(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	c := e.mcp(e.child(h, 120).PID, "claude", "")
	e.reg.gate = make(chan struct{})
	e.start(110, "claude", sid1, "startup", cwd)
	done := make(chan wire.ToolResponse)
	go func() { done <- e.tool(c, "status", `{}`) }()
	go e.l.reensure()
	eventually(t, "the first call", func() bool { return e.reg.agentCalls() == 1 })
	close(e.reg.gate)
	<-done
	e.waitAgent(k1)
	if n := e.reg.agentCalls(); n != 1 {
		t.Fatalf("%d POST /agents for one session", n)
	}
}

func TestStaleUnknownTokenRemintSkipped(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	e.start(110, "claude", sid1, "startup", cwd)
	e.waitAgent(k1)
	e.l.mu.Lock()
	old := e.l.tokens[wiretest.ClaudeAgent]
	e.l.tokens[wiretest.ClaudeAgent] = "tok-newer"
	e.l.mu.Unlock()
	_, tok, _ := e.l.remint(context.Background(), k1, "", old)
	if tok != "tok-newer" || e.reg.agentCalls() != 1 {
		t.Fatalf("token %s, %d calls", tok, e.reg.agentCalls())
	}
	_, tok, _ = e.l.remint(context.Background(), k1, "", "tok-newer")
	if tok == "tok-newer" || e.reg.agentCalls() != 2 {
		t.Fatalf("current token not re-minted: %s, %d calls", tok, e.reg.agentCalls())
	}
}

func TestSendRemintsOnUnknownToken(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	c := e.mcp(e.child(h, 120).PID, "claude", "")
	e.start(110, "claude", sid1, "startup", cwd)
	e.waitAgent(k1)
	e.room.revoked["tok-agent-0001"] = true
	r := e.tool(c, "send", `{"text":"hi"}`)
	if r.IsError || len(e.room.sent) != 1 || e.room.sent[0].token != "tok-agent-0002" {
		t.Fatalf("send after a revoked token: %+v %+v", r, e.room.sent)
	}
}

func TestSendRetriesNetworkErrorWithSameTxn(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeLive()
	e.fixtureProcs()
	c := e.mcp(claudeMCP.PID, "claude", "")
	e.room.sendErr = []error{context.DeadlineExceeded}
	r := e.tool(c, "send", `{"text":"hi"}`)
	if r.IsError || len(e.room.sent) != 1 {
		t.Fatalf("%+v", r)
	}
	e.room.sendErr = []error{&matrix.Error{Status: 403, Code: "M_FORBIDDEN", Msg: "no"}}
	if r := e.tool(c, "send", `{"text":"hi"}`); !r.IsError || !strings.HasPrefix(r.Text, "Not sent: the homeserver answered HTTP 403") {
		t.Fatalf("%+v", r)
	}
}

func TestHookInputChecks(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	dir := t.TempDir()
	for _, c := range []struct {
		sid, cwd, event string
		ok              bool
	}{
		{sid1, dir, wire.EventSessionStart, true},
		{strings.ToUpper(sid1), dir, wire.EventSessionStart, true},
		{"-rf", dir, wire.EventPrompt, false},
		{sid1, dir + "/", wire.EventPrompt, false},
		{sid1, dir + "/missing", wire.EventPrompt, false},
		{sid1, "", wire.EventSessionStart, false},
		{sid1, "", wire.EventPrompt, true},
	} {
		r := e.hook(110, hookReq("claude", c.event, c.sid, c.cwd))
		if r.OK != c.ok {
			t.Errorf("%q %q %s: ok %v", c.sid, c.cwd, c.event, r.OK)
		}
	}
	if e.session(k1) == nil {
		t.Fatal("an upper-case id was not canonicalised")
	}
}

func TestForgedResumeBeforeHarnessKnown(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	e.start(110, "claude", sid1, "startup", cwd)
	uid := e.waitAgent(k1)
	e.deliver(event("$m", bob, e.clk.Now(), text("secret q", uid)))
	rogue := e.claudeTree(900)
	e.child(rogue, 910)
	if r := e.start(910, "claude", sid1, "resume", cwd); r.Text != "" {
		t.Fatalf("forged resume printed %q", r.Text)
	}
	if r := e.hook(910, hookReq("claude", wire.EventPrompt, sid1, cwd)); r.Text != "" {
		t.Fatalf("rogue prompt hook printed %q", r.Text)
	}
	if r := e.hook(110, hookReq("claude", wire.EventPrompt, sid1, cwd)); !strings.Contains(r.Text, "| secret q") {
		t.Fatalf("own prompt hook got %q", r.Text)
	}
}

func TestResumeFromSameShellWithoutKnownHarness(t *testing.T) {
	e := newEnv(t)
	e.connect()
	h := e.claudeTree(100)
	e.child(h, 110)
	e.start(110, "claude", sid1, "startup", cwd)
	uid := e.waitAgent(k1)
	e.hook(110, hookReq("claude", wire.EventSessionEnd, sid1, cwd))
	e.procs.Kill(110)
	e.procs.Kill(100)
	e.deliver(event("$m", bob, e.clk.Now(), text("held q", uid)))
	h2 := proc.ID{PID: 200, Start: 200}
	e.procs.Add(h2, proc.ID{PID: 99, Start: 99})
	e.child(h2, 210)
	if r := e.start(210, "claude", sid1, "resume", cwd); !strings.Contains(r.Text, "| held q") {
		t.Fatalf("resume printed %q", r.Text)
	}
}
