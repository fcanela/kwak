//go:build integration && linux

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/registrar"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/test/integration/ittest"
)

// TestE9_E17 runs scenarios E9 and E11 to E17 of spec 10.4 in order on one world. Agents idle-close
// only after an hour here, so every close a step checks is the one that step caused. E10 needs the
// short idle_after and has its own world (TestE10).
func TestE9_E17(t *testing.T) {
	w := Start(t, WithRegistrarConfig(func(c *registrar.Config) { c.IdleAfter = registrar.Duration(time.Hour) }))
	if w == nil {
		return
	}
	a := w.Alice.NewHarness(t, wire.HarnessClaude)
	a.Begin(t)
	b := w.Bob.NewHarness(t, wire.HarnessCodex)
	b.Begin(t)
	aID := AgentID("alice", wire.HarnessClaude, a.SessionID)
	bID := AgentID("bob", wire.HarnessCodex, b.SessionID)
	if got := w.Alice.WaitAgent(t, a); got != aID {
		t.Fatalf("A's agent is %s, want %s", got, aID)
	}
	if got := w.Bob.WaitAgent(t, b); got != bID {
		t.Fatalf("B's agent is %s, want %s", got, bID)
	}
	var root string // A's open question, rooted before A ended
	var a1, a2 *FakeHarness
	steps := []struct {
		name string
		f    func(t *testing.T)
	}{
		{"E9_end_grace_resume", func(t *testing.T) { a, root = e9(t, w, a, b, aID) }},
		{"E11_forged_callers", func(t *testing.T) { e11(t, w, a, b, aID) }},
		{"E12_appservice_auth", func(t *testing.T) { e12(t, w) }},
		{"E14_listener_restart", func(t *testing.T) { e14(t, w, a, b, aID) }},
		{"E15_background_resume", func(t *testing.T) { e15(t, w, a, b, root) }},
		{"E17_one_listener", func(t *testing.T) { a1, a2 = e17(t, w) }},
		{"E13_revoke_all", func(t *testing.T) { e13(t, w, b, bID, a1) }},
		{"E16_secret_scan", func(t *testing.T) { e16(t, w, a1, a2, b) }},
	}
	for _, s := range steps {
		if !t.Run(s.name, s.f) {
			return
		}
	}
}

// E9: A's harness dies, so its MCP connection closes with the harness gone. After the 10 minute
// grace the agent is closed and its token dead; B's send says A is not in the room; A resumes under
// the same name and SessionStart prints the held message.
func e9(t *testing.T, w *World, a, b *FakeHarness, aID string) (*FakeHarness, string) {
	t0 := time.Now()
	root := ThreadOf(t, a.MustCall(t, wire.ToolSend, map[string]any{"text": "E9: an open question from A before it ends"}))
	old := e9Token(t, w, aID, t0)
	lp := e9LP(aID)

	a.Kill(t)
	e9WaitEnded(t, w.Alice, a)
	if r := w.Reg.Agent(lp); r == nil || r.Closed != nil {
		t.Fatalf("A's agent closed before the grace ran out: %+v", r)
	}
	w.Alice.Clock.Advance(11 * time.Minute)
	EventuallyMsg(t, "A's agent closed after the grace", func() (bool, string) {
		r := w.Reg.Agent(lp)
		return r != nil && r.Closed != nil && r.CloseReason == wire.ReasonSessionEnded, fmt.Sprintf("%+v", r)
	})
	w.WaitMember(t, aID, false)
	e9WaitDead(t, w, old)

	sent := b.MustCall(t, wire.ToolSend, map[string]any{"text": "E9: for A, whose session ended", "to": []string{aID}})
	if want := Short(aID) + " is not in the room: its session has ended. The message waits for its owner's next session.\n"; !strings.Contains(sent, want) {
		t.Fatalf("B's send result lacks %q:\n%s", want, sent)
	}
	held := ThreadOf(t, sent)
	w.Alice.WaitInbox(t, a, 1)

	r := w.Alice.NewHarnessWith(t, wire.HarnessClaude, a.SessionID, a.Cwd)
	out := r.Hook(t, wire.EventSessionStart, map[string]any{"source": "resume"})
	SameJSON(t, "A's SessionStart on resume", out, ContextJSON("SessionStart",
		Rendered(aID, wire.HarnessClaude, w.Item(t, held, render.Mention, ""))))
	r.StartMCP(t)
	if got := w.Alice.WaitAgent(t, r); got != aID {
		t.Fatalf("the resumed session's agent is %s, want %s", got, aID)
	}
	EventuallyMsg(t, "A's agent reopened", func() (bool, string) {
		x := w.Reg.Agent(lp)
		return x != nil && x.Open(), fmt.Sprintf("%+v", x)
	})
	w.WaitMember(t, aID, true)
	if st := r.MustCall(t, wire.ToolStatus, map[string]any{}); !strings.Contains(st, "Agent: "+Short(aID)+"\n") {
		t.Fatalf("status after the resume: %q", st)
	}
	if s := r.MustCall(t, wire.ToolSend, map[string]any{"text": "E9: A is back"}); !strings.HasPrefix(s, "Sent as "+Short(aID)+" in thread ") {
		t.Fatalf("send after the resume: %q", s)
	}
	if s, ok := w.Alice.Session(t, r); !ok || s.Inbox != 0 || !s.Ended.IsZero() || !s.Tools {
		t.Fatalf("the resumed session: %+v (known %v)", s, ok)
	}
	return r, root
}

// E11: hooks for A's live session from a process outside every fake harness, and from another fake
// harness, including a forged resume, get nothing and leave A's inbox and routing alone.
func e11(t *testing.T, w *World, a, b *FakeHarness, aID string) {
	m := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E11: for A only", "to": []string{aID}}))
	w.Alice.WaitInbox(t, a, 1)
	unchanged := func(what string) {
		t.Helper()
		s, ok := w.Alice.Session(t, a)
		if !ok || s.Inbox != 1 || !s.Ended.IsZero() || !s.Tools {
			t.Fatalf("after %s, A's session is %+v (known %v)", what, s, ok)
		}
	}
	forged := []struct {
		event  string
		fields map[string]any
	}{
		{wire.EventPrompt, nil},
		{wire.EventPostTool, nil},
		{wire.EventStop, nil},
		{wire.EventSessionStart, map[string]any{"source": "resume"}},
		{wire.EventSessionStart, map[string]any{"source": "compact"}},
		{wire.EventSessionEnd, nil},
	}
	for _, x := range forged {
		if out := e9OutsideHook(t, w, w.Alice, wire.HarnessClaude, x.event, a.Stdin(x.event, x.fields)); len(out) != 0 {
			t.Fatalf("a %s hook from outside any harness printed %q", x.event, out)
		}
		unchanged("a " + x.event + " hook from outside any harness")
	}

	c := w.Alice.NewHarness(t, wire.HarnessClaude)
	defer c.Close()
	for _, x := range forged {
		if out := c.HookRaw(t, x.event, a.Stdin(x.event, x.fields), false).Stdout; len(out) != 0 {
			t.Fatalf("a %s hook with A's session id from another harness printed %q", x.event, out)
		}
		unchanged("a " + x.event + " hook with A's session id from another harness")
	}
	if out := c.HookRaw(t, wire.EventPrompt, a.Stdin(wire.EventPrompt, nil), false).Stdout; len(out) != 0 {
		t.Fatalf("after its forged resume, the other harness reaches A's inbox: %q", out)
	}
	unchanged("the other harness's prompt after its forged resume")

	out := a.Hook(t, wire.EventPrompt, nil)
	SameJSON(t, "A's own prompt after the forgeries", out, ContextJSON("UserPromptSubmit",
		Rendered(aID, wire.HarnessClaude, w.Item(t, m, render.Mention, ""))))
	if st := a.MustCall(t, wire.ToolStatus, map[string]any{}); !strings.Contains(st, "Agent: "+Short(aID)+"\n") {
		t.Fatalf("A's status after the forgeries: %q", st)
	}
}

// E12: the appservice refuses a wrong or missing hs_token with 403, and with the right token it
// ignores an admin command whose event tuwunel does not have.
func e12(t *testing.T, w *World) {
	hs := w.Env.Registration.HSToken
	wrong := strings.Repeat("0", len(hs))
	empty := map[string]any{"events": []any{}}
	for _, x := range []struct {
		what, query, bearer string
		code                int
	}{
		{"no token", "", "", 403},
		{"a wrong Bearer token", "", wrong, 403},
		{"a wrong access_token", "access_token=" + wrong, "", 403},
		{"the right Bearer token with a wrong access_token", "access_token=" + wrong, hs, 403},
		{"the right Bearer token with an empty access_token", "access_token=", hs, 403},
		{"the right Bearer token", "", hs, 200},
		{"the right access_token", "access_token=" + hs, "", 200},
	} {
		code, body := e12Put(t, x.query, x.bearer, empty)
		if code != x.code {
			t.Fatalf("transaction with %s: %d %s, want %d", x.what, code, body, x.code)
		}
		var e struct {
			Errcode string `json:"errcode"`
		}
		json.Unmarshal(body, &e)
		if x.code == 403 && e.Errcode != "M_FORBIDDEN" {
			t.Fatalf("transaction with %s: body %s", x.what, body)
		}
	}

	carol := w.Env.RegisterHuman(t, "carol")
	ev := map[string]any{
		"event_id": "$e2einvented" + strings.ReplaceAll(UUIDv4(), "-", ""), "room_id": w.Admin.DM,
		"sender": w.Admin.Login.UserID, "type": "m.room.message", "origin_server_ts": time.Now().UnixMilli(),
		"content": map[string]any{"msgtype": "m.text", "body": "owner add " + carol.UserID},
	}
	if code, body := e12Put(t, "", hs, map[string]any{"events": []any{ev}}); code != 200 {
		t.Fatalf("transaction with an invented event: %d %s", code, body)
	}
	added := func() string {
		var why string
		w.Reg.View(func(st *registrar.State) {
			for name, o := range st.Owners {
				if o.Human == carol.UserID {
					why = "carol is owner " + name
				}
			}
			if st.DMs[carol.UserID] != "" {
				why += " carol has a DM"
			}
		})
		return why
	}
	if why := added(); why != "" {
		t.Fatalf("the invented owner add ran: %s", why)
	}
	Send(t, w.Admin.C, w.Admin.DM, "owners")
	list := WaitNotice(t, w.Admin.C, w.Admin.DM, "alice: "+w.Alice.Human.Login.UserID)
	if strings.Contains(list, "carol") {
		t.Fatalf("owners lists carol:\n%s", list)
	}
	if why := added(); why != "" {
		t.Fatalf("the invented owner add ran: %s", why)
	}
	page, err := w.Admin.C.Messages(t.Context(), w.Admin.DM, "", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range page.Chunk {
		var c struct{ Body string }
		json.Unmarshal(e.Content, &c)
		if strings.Contains(c.Body, carol.UserID) {
			t.Fatalf("the admin DM mentions carol: %q", c.Body)
		}
	}
}

// E14: alice's listener stops as on SIGTERM and starts again mid-run: nothing is shown twice,
// nothing is lost, A's token is re-minted and A keeps its name.
func e14(t *testing.T, w *World, a, b *FakeHarness, aID string) {
	old := e9Token(t, w, aID, time.Time{})
	m1 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E14: before the restart, shown before it", "to": []string{aID}}))
	w.Alice.WaitInbox(t, a, 1)
	SameJSON(t, "A's prompt before the restart", a.Hook(t, wire.EventPrompt, nil), ContextJSON("UserPromptSubmit",
		Rendered(aID, wire.HarnessClaude, w.Item(t, m1, render.Mention, ""))))
	m2 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E14: before the restart, still waiting", "to": []string{aID}}))
	w.Alice.WaitInbox(t, a, 1)

	w.Alice.StopListener(t)
	m3 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E14: while the listener is down", "to": []string{aID}}))
	restart := time.Now()
	w.Alice.StartListener(t)
	w.Alice.WaitSyncing(t)
	w.Alice.WaitInbox(t, a, 2)
	SameJSON(t, "A's prompt after the restart", a.Hook(t, wire.EventPrompt, nil), ContextJSON("UserPromptSubmit",
		Rendered(aID, wire.HarnessClaude, w.Item(t, m2, render.Mention, ""), w.Item(t, m3, render.Mention, ""))))
	for _, ev := range []string{wire.EventPrompt, wire.EventPostTool, wire.EventStop} {
		if out := a.Hook(t, ev, nil); len(out) != 0 {
			t.Fatalf("A's %s after the restart showed something again: %q", ev, out)
		}
	}

	// The MCP server's IPC connection died with the listener; it reconnects on a later call.
	var sent string
	EventuallyMsg(t, "A's first tool call after the restart", func() (bool, string) {
		r := a.Call(t, wire.ToolSend, map[string]any{"text": "E14: A after the restart"})
		sent = r.Text
		return !r.IsError, r.Text
	})
	if !strings.HasPrefix(sent, "Sent as "+Short(aID)+" in thread ") {
		t.Fatalf("A's send after the restart: %q", sent)
	}
	if nw := e9Token(t, w, aID, restart); nw == old {
		t.Fatal("A's token was not re-minted after the restart")
	}
	e9WaitDead(t, w, old)
	if s, _ := w.Alice.Session(t, a); s.Agent != aID || !s.Tools || !s.Ended.IsZero() {
		t.Fatalf("A's session after the restart: %+v", s)
	}
	m4 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E14: after the restart", "to": []string{aID}}))
	w.Alice.WaitInbox(t, a, 1)
	SameJSON(t, "A's prompt for a message after the restart", a.Hook(t, wire.EventPrompt, nil), ContextJSON("UserPromptSubmit",
		Rendered(aID, wire.HarnessClaude, w.Item(t, m4, render.Mention, ""))))
}

const e15Prompt = "kwak resumed this session in the background because other agents wrote to it. " +
	"kwak shows you their messages. Answer them with the kwak tools if you can, using what you already " +
	"know from this conversation. Your user is not watching this run."

// E15: alice turns on background resume on her machine, A ends, B replies in A's thread: the Runner double gets
// one run, with the 5.11 argv, in A's cwd, and no message text in argv.
func e15(t *testing.T, w *World, a, b *FakeHarness, root string) {
	var so, se bytes.Buffer
	if c := w.Alice.CLI().Config([]string{"resume-exe", "--harness", "claude", w.Kwak}, strings.NewReader(""), &so, &se); c != 0 {
		t.Fatalf("kwak config resume-exe exited %d: %s %s", c, so.String(), se.String())
	}
	// Decision 44: background resume is a machine setting; the listener reads it with no restart.
	so.Reset()
	se.Reset()
	if c := w.Alice.CLI().Config([]string{"resume", "replies"}, strings.NewReader(""), &so, &se); c != 0 {
		t.Fatalf("kwak config resume replies exited %d: %s %s", c, so.String(), se.String())
	}
	if s := w.Alice.Status(t); s.Resume != wire.ResumeReplies {
		t.Fatalf("alice's background resume: %q", s.Resume)
	}
	// resume-exe is read at start and at each reload (spec 5.2).
	var rr wire.ReloadResponse
	w.Alice.Call(t, wire.OpReload, &rr)
	if !rr.OK {
		t.Fatalf("reload: %+v", rr)
	}
	w.Alice.WaitSyncing(t)

	a.Kill(t)
	e9WaitEnded(t, w.Alice, a)
	const body = "E15: B answers A's open question after A ended"
	if rep := b.MustCall(t, wire.ToolReply, map[string]any{"thread": root, "text": body}); !strings.HasPrefix(rep, "Replied as ") {
		t.Fatalf("B's reply: %q", rep)
	}
	w.Alice.WaitInbox(t, a, 1)
	if runs := w.Alice.Runner.Runs(); len(runs) != 0 {
		t.Fatalf("a resume ran before the 30 s debounce: %+v", runs)
	}
	w.Alice.Clock.Advance(31 * time.Second)
	EventuallyMsg(t, "a background resume", func() (bool, string) {
		n := len(w.Alice.Runner.Runs())
		return n > 0, fmt.Sprintf("%d runs", n)
	})
	run := w.Alice.Runner.Runs()[0]
	want := []string{w.Kwak, "-p", e15Prompt, "--resume", a.SessionID, "--tools", "",
		"--allowedTools", "mcp__kwak__send,mcp__kwak__reply,mcp__kwak__thread",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{"kwak":{"command":"kwak","args":["mcp","--harness","claude"]}}}`,
		"--permission-mode", "dontAsk"}
	if !slices.Equal(run.Argv, want) {
		t.Fatalf("resume argv:\n got %q\nwant %q", run.Argv, want)
	}
	if run.Dir != a.Cwd {
		t.Fatalf("resume dir %q, want %q", run.Dir, a.Cwd)
	}
	for _, arg := range run.Argv {
		if strings.Contains(arg, body) || strings.Contains(arg, "E15") {
			t.Fatalf("message text in the resume argv: %q", arg)
		}
	}
	w.Alice.Clock.Advance(31 * time.Second)
	time.Sleep(12 * time.Second) // more than one 10 s deadline tick
	if runs := w.Alice.Runner.Runs(); len(runs) != 1 {
		t.Fatalf("background resume ran %d times, want 1: %+v", len(runs), runs)
	}
	if s, ok := w.Alice.Session(t, a); !ok || s.Inbox != 1 {
		t.Fatalf("the reply is no longer held for A: %+v (known %v)", s, ok)
	}
}

// E17: two of alice's sessions on one listener ask and answer each other, then A1's helper and A1
// do the same. Each message reaches the other agent only, never its sender.
func e17(t *testing.T, w *World) (a1, a2 *FakeHarness) {
	a1 = w.Alice.NewHarness(t, wire.HarnessClaude)
	a1.Begin(t)
	a2 = w.Alice.NewHarness(t, wire.HarnessCodex)
	a2.Begin(t)
	a1ID := AgentID("alice", wire.HarnessClaude, a1.SessionID)
	a2ID := AgentID("alice", wire.HarnessCodex, a2.SessionID)
	for _, x := range []struct {
		h  *FakeHarness
		id string
	}{{a1, a1ID}, {a2, a2ID}} {
		if got := w.Alice.WaitAgent(t, x.h); got != x.id {
			t.Fatalf("agent of %s is %s, want %s", x.h.SessionID, got, x.id)
		}
	}
	silent := func(h *FakeHarness, who string) {
		t.Helper()
		for _, ev := range []string{wire.EventPrompt, wire.EventPostTool} {
			if out := h.Hook(t, ev, nil); len(out) != 0 {
				t.Fatalf("%s's %s showed %q", who, ev, out)
			}
		}
	}

	q := ThreadOf(t, a1.MustCall(t, wire.ToolSend, map[string]any{"text": "E17: A2, which Go version does this repo use?", "to": []string{a2ID}}))
	w.Alice.WaitInbox(t, a2, 1)
	SameJSON(t, "A2's prompt", a2.Hook(t, wire.EventPrompt, nil), ContextJSON("UserPromptSubmit",
		Rendered(a2ID, wire.HarnessCodex, w.Item(t, q, render.Mention, ""))))
	silent(a1, "A1, the asker")

	rep := a2.MustCall(t, wire.ToolReply, map[string]any{"thread": q, "text": "E17: Go 1.26."})
	if !strings.HasPrefix(rep, "Replied as "+Short(a2ID)+" in thread "+q+", mentioning "+Short(a1ID)) {
		t.Fatalf("A2's reply: %q", rep)
	}
	ans := w.LastInThread(t, q)
	w.Alice.WaitInbox(t, a1, 1)
	SameJSON(t, "A1's prompt", a1.Hook(t, wire.EventPrompt, nil), ContextJSON("UserPromptSubmit",
		Rendered(a1ID, wire.HarnessClaude, Own(w.Item(t, ans, render.Mention, q)))))
	silent(a2, "A2, the answerer")
	silent(a1, "A1 after its answer")

	hx, _ := wire.SessionHex(a1.SessionID)
	helperID := "@agent.alice." + hx[:16] + ".tests:" + Server
	if res := a1.MustCall(t, wire.ToolHelper, map[string]any{"name": "tests"}); !strings.HasPrefix(res, "Created "+Short(helperID)+".") {
		t.Fatalf("helper: %q", res)
	}
	sent := a1.MustCall(t, wire.ToolSend, map[string]any{"text": "E17: A1, which tests are slow?", "to": []string{a1ID}, "as": "tests"})
	if !strings.HasPrefix(sent, "Sent as "+Short(helperID)+" in thread ") {
		t.Fatalf("the helper's send: %q", sent)
	}
	hq := ThreadOf(t, sent)
	w.Alice.WaitInbox(t, a1, 1)
	SameJSON(t, "A1's prompt for its helper's question", a1.Hook(t, wire.EventPrompt, nil), ContextJSON("UserPromptSubmit",
		Rendered(a1ID, wire.HarnessClaude, w.Item(t, hq, render.Mention, ""))))
	silent(a1, "A1 after its helper's question")

	rep = a1.MustCall(t, wire.ToolReply, map[string]any{"thread": hq, "text": "E17: the e2e ones."})
	if !strings.HasPrefix(rep, "Replied as "+Short(a1ID)+" in thread "+hq+", mentioning "+Short(helperID)) {
		t.Fatalf("A1's reply to its helper: %q", rep)
	}
	ans = w.LastInThread(t, hq)
	w.Alice.WaitInbox(t, a1, 1)
	it := Own(w.Item(t, ans, render.Mention, hq))
	it.Helper = "tests"
	SameJSON(t, "A1's prompt for its helper's answer", a1.Hook(t, wire.EventPrompt, nil), ContextJSON("UserPromptSubmit",
		Rendered(a1ID, wire.HarnessClaude, it)))
	silent(a1, "A1 after its helper's answer")
	silent(a2, "A2 at the end")
	return a1, a2
}

// E13: bob sends revoke all: bob's agent tokens die, the agents leave the room, bob's listener is
// unpaired and the tools say so.
func e13(t *testing.T, w *World, b *FakeHarness, bID string, waker *FakeHarness) {
	old := e9Token(t, w, bID, time.Time{})
	pairing := w.Bob.Config(t).PairingID
	standing := w.Reg.Listener(t, pairing)
	Send(t, w.Bob.Human.C, w.Bob.Human.DM, "revoke all")
	WaitNotice(t, w.Bob.Human.C, w.Bob.Human.DM, "Revoked 1 pairing(s) of bob.")
	w.Reg.View(func(st *registrar.State) {
		if p := st.Pairings[pairing]; p == nil || p.Revoked == nil {
			t.Errorf("pairing %s after revoke all: %+v", pairing, p)
		}
	})
	e9WaitDead(t, w, old)
	w.WaitMember(t, bID, false)
	if r := w.Reg.Agent(e9LP(bID)); r == nil || r.Closed == nil || r.CloseReason != "revoked" {
		t.Fatalf("B's agent after revoke all: %+v", r)
	}
	Eventually(t, "the standing device of bob's pairing logged out", func() bool {
		return !slices.Contains(w.Devices(t, standing), "KWAK-"+pairing)
	})

	// A new room event ends bob's sync long poll, so its next sync meets the dead standing token.
	waker.MustCall(t, wire.ToolSend, map[string]any{"text": "E13: is anyone still there?"})
	EventuallyMsg(t, "bob's listener unpaired", func() (bool, string) {
		s := w.Bob.Status(t)
		return s.State == wire.StateUnpaired, fmt.Sprintf("%s %q", s.State, s.Detail)
	})
	const text = "this machine's kwak pairing was revoked or expired; run kwak pair"
	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{wire.ToolSend, map[string]any{"text": "E13: after the revoke"}},
		{wire.ToolStatus, map[string]any{}},
	} {
		if r := b.Call(t, tool.name, tool.args); !r.IsError || r.Text != text {
			t.Fatalf("B's %s after revoke all: %+v", tool.name, r)
		}
	}
	if out := b.Hook(t, wire.EventPrompt, nil); len(out) != 0 {
		t.Fatalf("B's prompt after revoke all printed %q", out)
	}
}

// E16: the I1 scan. No token or key the edge saw, no owner key and neither appservice token is in
// any hook or MCP output, in the listeners' state.json and config.json, in the registrar's state
// file (I5), or in a live harness's environment.
func e16(t *testing.T, w *World, live ...*FakeHarness) {
	secrets := append(w.Secrets(), w.Env.Registration.ASToken, w.Env.Registration.HSToken)
	keys := 0
	for _, s := range secrets {
		if strings.HasPrefix(s, "kwak_ok_") {
			keys++
		}
	}
	if keys < 2 || len(secrets) < 8 {
		t.Fatalf("the scan would prove little: %d secrets, %d owner keys", len(secrets), keys)
	}
	type source struct {
		what string
		data []byte
	}
	var srcs []source
	mcpLines, hookOut := 0, 0
	for _, c := range w.Captured() {
		srcs = append(srcs, source{c.What, c.Data})
		switch {
		case strings.HasSuffix(c.What, " mcp"):
			mcpLines++
		case len(c.Data) > 0:
			hookOut++
		}
	}
	if mcpLines == 0 || hookOut == 0 {
		t.Fatalf("the scan would prove little: %d MCP lines, %d hook outputs", mcpLines, hookOut)
	}
	files := []string{w.Reg.Cfg.State}
	for _, o := range []*Owner{w.Alice, w.Bob} {
		files = append(files, filepath.Join(o.StateDir, "state.json"), o.ConfigPath())
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		srcs = append(srcs, source{f, b})
	}
	for _, h := range live {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(h.PID) + "/environ")
		if err != nil {
			t.Fatalf("environment of the fake harness of %s: %v", h.SessionID, err)
		}
		srcs = append(srcs, source{"environment of " + h.SessionID, b})
	}
	bad := 0
	for _, src := range srcs {
		for _, s := range secrets {
			if s != "" && bytes.Contains(src.data, []byte(s)) {
				bad++
				t.Errorf("%s holds a secret starting %q", src.what, s[:min(len(s), 6)])
			}
		}
	}
	t.Logf("I1 scan: %d sources, %d secrets, %d findings", len(srcs), len(secrets), bad)
}

// TestE10 runs scenario E10 of spec 10.4 with the 3 s idle_after: B's agent is closed by the
// registrar's job, and bob's standing account is kept.
func TestE10(t *testing.T) {
	w := Start(t)
	if w == nil {
		return
	}
	t.Run("E10_idle_close", func(t *testing.T) { e10(t, w) })
}

func e10(t *testing.T, w *World) {
	b := w.Bob.NewHarness(t, wire.HarnessCodex)
	b.Begin(t)
	bID := AgentID("bob", wire.HarnessCodex, b.SessionID)
	if got := w.Bob.WaitAgent(t, b); got != bID {
		t.Fatalf("B's agent is %s, want %s", got, bID)
	}
	t0 := time.Now()
	b.MustCall(t, wire.ToolSend, map[string]any{"text": "E10: B speaks once, then goes idle"})
	old := e9Token(t, w, bID, t0)
	lp := e9LP(bID)
	EventuallyMsg(t, "B's agent closed by the idle job", func() (bool, string) {
		r := w.Reg.Agent(lp)
		return r != nil && r.Closed != nil && r.CloseReason == "idle", fmt.Sprintf("%+v", r)
	})
	if r := w.Reg.Agent(lp); r.Closed.Sub(r.Created) < w.Reg.Cfg.IdleAfter.D() || r.Closed.Sub(r.LastActive) < w.Reg.Cfg.IdleAfter.D() {
		t.Fatalf("B's agent closed before idle_after: %+v", r)
	}
	w.WaitMember(t, bID, false)
	e9WaitDead(t, w, old)

	pairing := w.Bob.Config(t).PairingID
	standing := w.Reg.Listener(t, pairing)
	time.Sleep(2 * w.Reg.Cfg.IdleAfter.D()) // more idle periods, each with job runs
	if r := w.Reg.Agent(standing[1 : len(standing)-len(":"+Server)]); r == nil || !r.Open() {
		t.Fatalf("bob's standing account after the idle job: %+v", r)
	}
	if !slices.Contains(w.Members(t), standing) {
		t.Fatalf("bob's standing account left the room")
	}
	if devs := w.Devices(t, standing); !slices.Contains(devs, "KWAK-"+pairing) {
		t.Fatalf("devices of %s: %v", standing, devs)
	}
	if s := w.Bob.Status(t); s.State != wire.StateSyncing {
		t.Fatalf("bob's listener after the idle close: %+v", s)
	}

	// The kept standing account still syncs: a mention of the closed agent reaches B.
	if _, err := w.Alice.Human.C.Join(t.Context(), w.Room); err != nil {
		t.Fatalf("alice joins the agents room: %v", err)
	}
	ev := SendContent(t, w.Alice.Human.C, w.Room, map[string]any{"msgtype": "m.text", "body": "E10: B, are you there?",
		"m.mentions": map[string]any{"user_ids": []string{bID}}})
	w.Bob.WaitInbox(t, b, 1)
	SameJSON(t, "B's prompt after its agent was idle-closed", b.Hook(t, wire.EventPrompt, nil), ContextJSON("UserPromptSubmit",
		Rendered(bID, wire.HarnessCodex, w.Item(t, ev, render.Mention, ""))))
}

// ---- helpers of E9 to E17 ----

func e9LP(uid string) string { return strings.TrimPrefix(Short(uid), "@") }

// e9Whoami asks tuwunel who tok belongs to.
func e9Whoami(t *testing.T, w *World, tok string) (code int, userID, errcode string) {
	t.Helper()
	code, body := w.HS(t, "GET", "/_matrix/client/v3/account/whoami", tok, "")
	var r struct {
		UserID  string `json:"user_id"`
		Errcode string `json:"errcode"`
	}
	json.Unmarshal(body, &r)
	return code, r.UserID, r.Errcode
}

// e9Token finds uid's live access token among the bearer tokens the edge passed since t0, newest
// first. Agent tokens live only in the listener's memory, so this is how a test learns one.
func e9Token(t *testing.T, w *World, uid string, t0 time.Time) string {
	t.Helper()
	reqs := w.Requests(t0)
	seen := map[string]bool{}
	for i := len(reqs) - 1; i >= 0; i-- {
		tok := reqs[i].Token
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		if code, who, _ := e9Whoami(t, w, tok); code == 200 && who == uid {
			return tok
		}
	}
	t.Fatalf("no live token of %s among %d tokens the edge saw", uid, len(seen))
	return ""
}

// e9WaitDead waits until tuwunel answers M_UNKNOWN_TOKEN for tok.
func e9WaitDead(t *testing.T, w *World, tok string) {
	t.Helper()
	EventuallyMsg(t, "a dead token", func() (bool, string) {
		code, who, e := e9Whoami(t, w, tok)
		return code == 401 && e == "M_UNKNOWN_TOKEN", fmt.Sprintf("whoami %d %s %s", code, who, e)
	})
}

// e9WaitEnded waits until the listener counts h's session as ended. kwak status leaves out ended
// sessions with nothing waiting.
func e9WaitEnded(t *testing.T, o *Owner, h *FakeHarness) {
	t.Helper()
	Eventually(t, "the end of session "+h.SessionID, func() bool {
		s, ok := o.Session(t, h)
		return !ok || !s.Ended.IsZero()
	})
}

// e9OutsideHook runs kwak hook from the test process itself, outside every fake harness.
func e9OutsideHook(t *testing.T, w *World, o *Owner, h wire.Harness, event string, stdin []byte) []byte {
	t.Helper()
	cmd := exec.Command(w.Kwak, "hook", string(h), event)
	cmd.Env = append(withEnv(os.Environ(), envFake, ""), "KWAK_SOCKET="+o.Socket)
	cmd.Stdin = bytes.NewReader(stdin)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		t.Fatalf("kwak hook %s from outside: %v: %s", event, err, se.String())
	}
	w.cap.add("outside hook "+event, so.Bytes())
	return so.Bytes()
}

// e12Put sends one appservice transaction straight to the registrar's appservice port.
func e12Put(t *testing.T, query, bearer string, body any) (int, []byte) {
	t.Helper()
	u := "http://127.0.0.1" + ittest.AppserviceAddr + "/_matrix/app/v1/transactions/e2e-e12-" + UUIDv4()
	if query != "" {
		u += "?" + query
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(t.Context(), "PUT", u, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("PUT a transaction: %v", err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}
