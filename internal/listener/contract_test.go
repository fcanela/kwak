package listener

import (
	"bufio"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

var (
	errBoom    = errors.New("boom")
	secretsKey = secrets.PairingKeyName(wiretest.ServerName, wiretest.PairingID)
)

// Fixture processes of the contract test.
var (
	claudeH   = proc.ID{PID: 1000, Start: 1000}
	claudeSh  = proc.ID{PID: 999, Start: 999}
	claudeHk  = proc.ID{PID: 1100, Start: 1100}
	claudeMCP = proc.ID{PID: 1200, Start: 1200}
	codexH    = proc.ID{PID: 2000, Start: 2000}
	codexSh   = proc.ID{PID: 2100, Start: 2100}
	codexHk   = proc.ID{PID: 2101, Start: 2101}
	codexMCP  = proc.ID{PID: 2200, Start: 2200}
)

var (
	qItem = render.Item{EventID: wiretest.Root1, Sender: mallory, SentAt: at("2026-11-02T10:31:00Z"), Reason: render.Mention,
		Body: "How does the dispute kit pick jurors when the court has no stake?"}
	aItem = render.Item{EventID: "$ev2", Sender: bob, ThreadRoot: wiretest.Root2, SentAt: at("2026-11-02T10:40:00Z"),
		Reason: render.YourThread, Body: "It draws from the parent court.\nSee the sortition module."}
	cItem = render.Item{EventID: wiretest.Root1, Sender: bob, SentAt: at("2026-11-02T10:40:00Z"), Reason: render.Mention,
		Body: "Which port does the API listen on?"}
)

func (e *env) fixtureProcs() {
	e.procs.Line(claudeHk, claudeH, claudeSh)
	e.procs.Add(claudeMCP, claudeH)
	e.procs.Line(codexHk, codexSh, codexH, proc.ID{PID: 1900, Start: 1900})
	e.procs.Add(codexMCP, codexH)
}

// live adds a running session with an agent and its token.
func (e *env) live(h wire.Harness, sid, cwd string, hp proc.ID, cands []proc.ID, agent string, items ...render.Item) *session {
	e.l.mu.Lock()
	defer e.l.mu.Unlock()
	key := sessionKey(h, sid)
	p := hp
	s := &session{Key: key, Harness: h, SessionID: sid, Cwd: cwd, Candidates: cands, HarnessProc: &p, Tools: true,
		Agent: agent, Helpers: map[string]*helper{}, Started: at("2026-11-02T09:00:00Z"), LastHook: at("2026-11-02T10:40:00Z")}
	s.Inbox.init()
	s.Inbox.Items = append(s.Inbox.Items, items...)
	e.l.st.Sessions[key] = s
	if agent != "" {
		e.l.st.Mine[agent] = Agent{Session: key}
		e.l.tokens[agent] = wiretest.AgentToken
	}
	return s
}

func (e *env) addHelper(s *session, name, uid string) {
	e.l.mu.Lock()
	defer e.l.mu.Unlock()
	h := &helper{UserID: uid}
	h.Inbox.init()
	s.Helpers[name] = h
	e.l.st.Mine[uid] = Agent{Session: s.Key, Helper: name}
	e.l.tokens[uid] = "tok-helper-0001"
}

func (e *env) claudeLive(items ...render.Item) *session {
	return e.live(wire.HarnessClaude, sid1, wiretest.ClaudeCwd, claudeH, []proc.ID{claudeH, claudeSh}, wiretest.ClaudeAgent, items...)
}

func (e *env) codexLive(items ...render.Item) *session {
	return e.live(wire.HarnessCodex, cx1, wiretest.CodexCwd, codexH, []proc.ID{codexSh, codexH}, wiretest.CodexAgent, items...)
}

func (e *env) setPhase(phase, detail string) {
	e.l.mu.Lock()
	defer e.l.mu.Unlock()
	e.l.phase, e.l.detail = phase, detail
}

// arrange puts the listener into the state a golden row describes and sets the caller's pid.
// It returns the harness of the MCP hello to send first, or "".
func arrange(t *testing.T, e *env, c wiretest.IPCCase) wire.Harness {
	e.fixtureProcs()
	e.connect()
	name := c.Name
	pidFor := func() {
		switch {
		case strings.HasPrefix(name, "codex") && strings.Contains(name, "mcp_hello"):
			e.pid.Store(int64(codexMCP.PID))
		case strings.HasPrefix(name, "codex"):
			e.pid.Store(int64(codexHk.PID))
		case strings.Contains(name, "mcp_hello"), strings.HasPrefix(name, "tool"):
			e.pid.Store(int64(claudeMCP.PID))
		default:
			e.pid.Store(int64(claudeHk.PID))
		}
	}
	pidFor()
	short := aItem
	short.Body = "It draws from the parent court."
	var hello wire.Harness
	if strings.HasPrefix(name, "tool") {
		hello = wire.HarnessClaude
	}
	switch name {
	case "claude session-start startup", "codex session-start startup", "claude subagent-stop", "claude session-end",
		"codex subagent-stop", "codex session-end", "hook with a session id that is not a UUID", "hook with a relative cwd",
		"hook with protocol v2", "claude mcp_hello", "codex mcp_hello, no env session", "mcp_hello from a newer kwak with an unknown field",
		"mcp_hello with protocol v2", "unknown op", "status with protocol v2", "not JSON", "no v", "inbox, empty", "shutdown":
		if strings.HasSuffix(name, "subagent-stop") || strings.HasSuffix(name, "session-end") {
			if strings.HasPrefix(name, "codex") {
				e.codexLive()
			} else {
				e.claudeLive()
			}
		}
	case "claude session-start resume with waiting items (DV-5)":
		s := e.claudeLive(qItem, aItem)
		now := at("2026-11-02T10:45:00Z")
		s.Ended, s.Tools = &now, false
	case "claude prompt with items", "claude PostToolUseFailure goes to post-tool", "claude stop, not active",
		"claude stop, already active: no drain (DV-3)":
		e.claudeLive(qItem, aItem)
	case "claude prompt, stdin with unknown fields, empty inbox", "hook from a newer kwak with an unknown field: the older listener answers empty":
		e.claudeLive()
	case "claude post-tool after Bash, tool_input not forwarded, more waiting":
		e.l.cfg.Budget = map[wire.Harness]int{wire.HarnessClaude: utf8.RuneCountInString(wiretest.DeliveryOneMore)}
		e.claudeLive(short, qItem, qItem, qItem)
	case "claude post-tool after the kwak helper tool in a subagent":
		e.addHelper(e.claudeLive(), "tests", wiretest.HelperAgent)
	case "codex prompt with an item", "codex post-tool after shell, tool_input not forwarded":
		e.codexLive(cItem)
	case "codex post-tool after the kwak send tool", "codex stop, already active":
		e.codexLive(cItem)
		if name == "codex post-tool after the kwak send tool" {
			e.l.st.Sessions[sessionKey(wire.HarnessCodex, cx1)].Inbox.Items = nil
		}
	case "codex stop, not active: fixed notice, no drain (DV-4)":
		e.codexLive(cItem, aItem)
	case "codex post-tool on Windows carries the chain", "codex mcp_hello on Windows carries the chain":
		e.pidOK.Store(false)
	case "hook while unpaired", "tool while unpaired", "status, unpaired":
		e.setPhase(wire.StateUnpaired, textUnpaired)
	case "tool send with to":
		e.claudeLive()
		e.room.ids = []string{wiretest.Root1}
		e.room.members = []string{wiretest.ClaudeAgent, mallory}
	case "tool send, open question as a helper":
		e.addHelper(e.claudeLive(), "tests", wiretest.HelperAgent)
		e.room.ids = []string{wiretest.Root2}
	case "tool reply":
		e.claudeLive()
		e.room.events[wiretest.Root1] = event(wiretest.Root1, mallory, at("2026-11-02T10:31:00Z"), text(qItem.Body, wiretest.ClaudeAgent))
		e.room.members = []string{wiretest.ClaudeAgent, mallory}
	case "tool thread":
		e.claudeLive()
		e.room.events[wiretest.Root1] = event(wiretest.Root1, mallory, at("2026-11-02T10:31:00Z"), text(qItem.Body, bob))
		e.room.replies[wiretest.Root1] = []matrix.Event{event("$r1", bob, at("2026-11-02T10:40:00Z"), inThread(text("It draws from the parent court."), wiretest.Root1))}
	case "tool helper", "tool volunteer is gone":
		e.claudeLive()
	case "tool call with an unknown field":
		e.claudeLive()
		e.setPhase(wire.StateSyncing, "")
	case "tool status":
		e.addHelper(e.claudeLive(), "tests", wiretest.HelperAgent)
		e.setPhase(wire.StateSyncing, "")
	case "codex tool call carries _meta":
		hello = wire.HarnessCodex
		e.pid.Store(int64(codexMCP.PID))
		e.codexLive()
		e.room.ids = []string{wiretest.Root1}
		e.room.members = []string{wiretest.CodexAgent, bob}
	case "tool before the session start was seen":
		e.clk.instant = true
	case "tool while the registrar is down":
		e.live(wire.HarnessClaude, sid1, wiretest.ClaudeCwd, claudeH, []proc.ID{claudeH, claudeSh}, "")
		e.reg.agentErr = func(wire.AgentRequest) error { return &wire.APIError{Status: 502} }
	case "tool with a meta_session that is not a UUID":
	case "tool that failed inside the listener":
		e.claudeLive()
		e.room.eventErr = errBoom
	case "status, syncing":
		cs := e.claudeLive(qItem, aItem)
		e.addHelper(cs, "tests", wiretest.HelperAgent)
		xs := e.codexLive()
		xs.Started, xs.LastHook = at("2026-11-02T09:30:00Z"), time.Time{}
		e.l.mu.Lock()
		e.l.phase, e.l.lastSync, e.l.st.ResumeRuns = wire.StateSyncing, at("2026-11-02T10:41:00Z"), 1
		e.l.st.Turns["$Qx1abc"] = &turns{N: 50}
		e.l.st.Turns["$under"] = &turns{N: 49}
		for range wire.DefaultResumePerHour {
			e.l.hourRuns = append(e.l.hourRuns, at("2026-11-02T10:20:00Z"))
		}
		e.l.st.Checking["$Ev3ghi"] = &pendingCheck{Item: render.Item{EventID: "$Ev3ghi", Sender: wiretest.OtherAgent, Body: "q"}}
		e.l.st.Rejected = []wire.Rejection{
			{At: at("2026-11-02T10:30:00Z"), Way: "outgoing", Session: cs.Key, Agent: wiretest.ClaudeAgent, Tool: "reply",
				Thread: "$Qx1abc", Check: "secrets", Reason: "it looks like it holds a secret (a GitHub token)"},
			{At: at("2026-11-02T10:35:00Z"), Way: "incoming", Session: cs.Key, Agent: wiretest.ClaudeAgent, Sender: wiretest.OtherAgent,
				Thread: "$Qx1abc", Event: "$Ev2def", Check: "command", Reason: "it asks for an action"},
		}
		e.l.mu.Unlock()
		e.setChecks("", "/opt/kwak/check-in")
		e.setResume(wire.ResumeReplies, wire.ResumeMentions)
		e.editConf(func(c *MachineConfig) { c.Responder, c.ResponderDir = true, "/home/u/kwak-responder" })
		e.l.mu.Lock()
		e.l.respUser = wiretest.ResponderUser
		e.l.mu.Unlock()
	case "status, waiting for the keyring":
		e.l.st.Settings = nil
		e.setPhase(wire.StateWaitingKeyring, textKeyring)
		e.l.mu.Lock()
		e.l.standUser = "" // no POST /standing worked yet, so no listener account is shown
		e.l.mu.Unlock()
		e.pidOK.Store(false)
	case "status, connecting after a state reset":
		e.l.mu.Lock()
		e.l.st.Settings, e.l.stateReset, e.l.syncErr = nil, true, "registrar unreachable, HTTP 502"
		e.l.standUser = "" // the registrar is unreachable, so no POST /standing worked yet
		e.l.mu.Unlock()
	case "inbox":
		s := e.claudeLive(qItem, aItem, cItem)
		now := at("2026-11-02T10:45:00Z")
		s.Ended = &now
	case "reload":
		go e.l.supervise(e.l.bg)
	case "reload, key refused":
		e.sec.Set(secretsKey, wiretest.BadKey)
		go e.l.supervise(e.l.bg)
	case "reload, registrar unreachable":
		e.reg.standErr = &wire.APIError{Status: 502}
		go e.l.supervise(e.l.bg)
	default:
		t.Fatalf("no fixture for golden row %q", name)
	}
	return hello
}

func TestIPCGoldenTable(t *testing.T) {
	for _, c := range wiretest.IPC {
		t.Run(c.Name, func(t *testing.T) {
			e := newEnv(t)
			hello := arrange(t, e, c)
			conn := e.serve(t)
			r := bufio.NewReader(conn)
			if hello != "" {
				writeRaw(t, conn, `{"v":1,"kwak":"0.1.0","op":"mcp_hello","harness":"`+string(hello)+`","env_session":"","chain":null}`)
				if got := readRaw(t, r); got != `{"v":1,"ok":true,"kwak":"0.1.0"}` {
					t.Fatalf("hello: %s", got)
				}
			}
			writeRaw(t, conn, c.Request)
			if got := readRaw(t, r); got != c.Reply {
				t.Errorf("reply\n got: %s\nwant: %s", got, c.Reply)
			}
		})
	}
}

// serve runs the IPC server on a real unix socket and returns a connection to it.
func (e *env) serve(t *testing.T) net.Conn {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "k.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go e.l.serveConn(c)
		}
	}()
	conn, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	return conn
}

func writeRaw(t *testing.T, c net.Conn, line string) {
	t.Helper()
	if _, err := c.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
}

func readRaw(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v (got %q)", err, line)
	}
	return strings.TrimSuffix(line, "\n")
}
