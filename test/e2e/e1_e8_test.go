//go:build integration && linux

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
)

// TestE1_E8 runs scenarios E1 to E8 of spec 10.4 in order on one world. Each step builds on the
// one before, so a failed step stops the rest.
func TestE1_E8(t *testing.T) {
	w := Start(t, Bare())
	if w == nil {
		return
	}
	var a, b *FakeHarness // alice's Claude session A, bob's Codex session B
	var aID, bID string
	steps := []struct {
		name string
		f    func(t *testing.T)
	}{
		{"E1_pairing", func(t *testing.T) { e1(t, w) }},
		{"E2_sessions", func(t *testing.T) { a, b, aID, bID = e2(t, w) }},
		{"E3_send_reply", func(t *testing.T) { e3(t, w, a, b, aID, bID) }},
		{"E4_codex_stop", func(t *testing.T) { e4(t, w, a, b, bID) }},
		{"E5_open_question", func(t *testing.T) { e5(t, w, a, b, aID, bID) }},
		{"E6_helper", func(t *testing.T) { e6(t, w, a, b, aID) }},
		{"E7_empty", func(t *testing.T) { e7(t, w, a, b) }},
		{"E8_gap_fill", func(t *testing.T) { e8(t, w, a, b, aID) }},
	}
	for _, s := range steps {
		if !t.Run(s.name, s.f) {
			return
		}
	}
}

// E1: owner add for alice and bob, then each listener pairs through kwak pair and the DM.
func e1(t *testing.T, w *World) {
	if enc, err := w.Env.AppserviceClient().RoomEncrypted(t.Context(), w.Admin.DM); err != nil || enc {
		t.Fatalf("admin DM encrypted %v, %v", enc, err)
	}
	for _, o := range []*Owner{w.Alice, w.Bob} {
		w.AddOwner(t, o)
		if enc, err := w.Env.AppserviceClient().RoomEncrypted(t.Context(), o.Human.DM); err != nil || enc {
			t.Fatalf("%s's DM encrypted %v, %v", o.Name, enc, err)
		}
		o.StartListener(t)
		if s := o.Status(t); s.State != wire.StateUnpaired {
			t.Fatalf("%s's listener before pairing: %q", o.Name, s.State)
		}
		p := o.Pair(t)
		if !strings.HasPrefix(p.Key, "kwak_ok_") {
			t.Fatalf("%s's keyring holds %q", o.Name, p.Key)
		}
		want := fmt.Sprintf("Paired as %s on %s (pairing %s, key valid until ", o.Name, o.Machine, p.PairingID)
		if !strings.Contains(p.Out, want) {
			t.Fatalf("kwak pair output lacks %q:\n%s", want, p.Out)
		}
		WaitNotice(t, o.Human.C, o.Human.DM, fmt.Sprintf("Paired machine '%s' (linux) as pairing %s, asked from ", o.Machine, p.PairingID))
		standing := "@" + wire.StandingLocalpart(o.Name) + ":" + Server
		if devs := w.Devices(t, standing); !slices.Contains(devs, "KWAK-"+p.PairingID) {
			t.Fatalf("devices of %s: %v, want KWAK-%s", standing, devs, p.PairingID)
		}
		cfg := o.Config(t)
		if cfg.Owner != o.Name || cfg.RoomID != w.Room || cfg.StandingUser != standing || cfg.ServerName != Server || cfg.Server != w.EdgeURL {
			t.Fatalf("%s config.json: %+v", o.Name, cfg)
		}
		s := o.Status(t)
		if s.Owner != o.Name || s.PairingID != p.PairingID || s.State != wire.StateSyncing {
			t.Fatalf("%s status after pairing: %+v", o.Name, s)
		}
		w.WaitMember(t, standing, true)
	}
}

// E2: alice's Claude session A (UUIDv4) and bob's Codex session B (UUIDv7) start.
func e2(t *testing.T, w *World) (a, b *FakeHarness, aID, bID string) {
	a = w.Alice.NewHarness(t, wire.HarnessClaude)
	b = w.Bob.NewHarness(t, wire.HarnessCodex)
	for _, h := range []*FakeHarness{a, b} {
		r := h.HookResult(t, wire.EventSessionStart, nil)
		if len(r.Stdout) != 0 {
			t.Fatalf("SessionStart of %s printed %q", h.SessionID, r.Stdout)
		}
		if r.Took > 1500*time.Millisecond {
			t.Fatalf("SessionStart of %s took %s; agent creation must not block it", h.SessionID, r.Took)
		}
		h.StartMCP(t)
	}
	aID = AgentID("alice", wire.HarnessClaude, a.SessionID)
	bID = AgentID("bob", wire.HarnessCodex, b.SessionID)
	ahex, _ := wire.SessionHex(a.SessionID)
	bhex, _ := wire.SessionHex(b.SessionID)
	if aID != "@agent.alice."+ahex[:16]+":"+Server || bID != "@agent.bob."+bhex[16:]+":"+Server {
		t.Fatalf("names %s %s", aID, bID)
	}
	for _, x := range []struct {
		o  *Owner
		h  *FakeHarness
		id string
	}{{w.Alice, a, aID}, {w.Bob, b, bID}} {
		if got := x.o.WaitAgent(t, x.h); got != x.id {
			t.Fatalf("%s's session agent is %s, want %s", x.o.Name, got, x.id)
		}
		w.WaitMember(t, x.id, true)
		if dn := w.DisplayName(t, x.id); dn != strings.TrimPrefix(Short(x.id), "@") {
			t.Fatalf("display name of %s is %q", x.id, dn)
		}
		if st := x.h.MustCall(t, wire.ToolStatus, map[string]any{}); !strings.Contains(st, "Agent: "+Short(x.id)+"\n") {
			t.Fatalf("status tool: %q", st)
		}
		s, _ := x.o.Session(t, x.h)
		if !s.Tools {
			t.Fatalf("%s's session has no tools connection: %+v", x.o.Name, s)
		}
	}
	return a, b, aID, bID
}

// E3: A sends to B, B's prompt hook shows it, B replies, A's post-tool shows the reply; replies in
// A's thread with no mention reach A, also after a failing tool; A's stop hook continues once.
func e3(t *testing.T, w *World, a, b *FakeHarness, aID, bID string) {
	sent := a.MustCall(t, wire.ToolSend, map[string]any{"text": "E3: which port does the registrar API use?", "to": []string{bID}})
	if !strings.HasPrefix(sent, "Sent as "+Short(aID)+" in thread ") {
		t.Fatalf("send: %q", sent)
	}
	root := ThreadOf(t, sent)

	out := b.WaitHook(t, wire.EventPrompt, nil)
	SameJSON(t, "B's prompt hook", out, ContextJSON("UserPromptSubmit",
		Rendered(bID, wire.HarnessCodex, w.Item(t, root, render.Mention, ""))))

	rep := b.MustCall(t, wire.ToolReply, map[string]any{"thread": root, "text": "E3: port 8010, behind the proxy."})
	if !strings.HasPrefix(rep, "Replied as "+Short(bID)+" in thread "+root+", mentioning "+Short(aID)) {
		t.Fatalf("reply: %q", rep)
	}
	repID := w.LastInThread(t, root)
	out = a.WaitHook(t, wire.EventPostTool, nil)
	SameJSON(t, "A's post-tool hook", out, ContextJSON("PostToolUse",
		Rendered(aID, wire.HarnessClaude, w.Item(t, repID, render.Mention, root))))

	if _, err := w.Bob.Human.C.Join(t.Context(), w.Room); err != nil {
		t.Fatalf("bob joins the agents room: %v", err)
	}
	plain := SendContent(t, w.Bob.Human.C, w.Room, ThreadReply(root, "E3: it listens on loopback too."))
	failed := map[string]any{"hook_event_name": "PostToolUseFailure", "tool_response": nil, "error": "Exit code 1"}
	out = a.WaitHook(t, wire.EventPostTool, failed)
	SameJSON(t, "A's PostToolUseFailure hook", out, ContextJSON("PostToolUseFailure",
		Rendered(aID, wire.HarnessClaude, w.Item(t, plain, render.YourThread, root))))

	plain2 := SendContent(t, w.Bob.Human.C, w.Room, ThreadReply(root, "E3: and wg0."))
	w.Alice.WaitInbox(t, a, 1)
	out = a.Hook(t, wire.EventStop, nil)
	SameJSON(t, "A's stop hook", out, ContextJSON("Stop",
		Rendered(aID, wire.HarnessClaude, w.Item(t, plain2, render.YourThread, root))))

	plain3 := SendContent(t, w.Bob.Human.C, w.Room, ThreadReply(root, "E3: one more thing."))
	w.Alice.WaitInbox(t, a, 1)
	if out := a.Hook(t, wire.EventStop, map[string]any{"stop_hook_active": true}); len(out) != 0 {
		t.Fatalf("stop with stop_hook_active printed %q", out)
	}
	if s, _ := w.Alice.Session(t, a); s.Inbox != 1 {
		t.Fatalf("stop with stop_hook_active drained the inbox: %d left", s.Inbox)
	}
	out = a.Hook(t, wire.EventPrompt, nil)
	SameJSON(t, "A's prompt after the held stop", out, ContextJSON("UserPromptSubmit",
		Rendered(aID, wire.HarnessClaude, w.Item(t, plain3, render.YourThread, root))))
}

// E4: B's Codex stop with items prints the fixed notice and drains nothing; the next post-tool
// delivers.
func e4(t *testing.T, w *World, a, b *FakeHarness, bID string) {
	r1 := ThreadOf(t, a.MustCall(t, wire.ToolSend, map[string]any{"text": "E4: first question", "to": []string{bID}}))
	r2 := ThreadOf(t, a.MustCall(t, wire.ToolSend, map[string]any{"text": "E4: second question", "to": []string{bID}}))
	w.Bob.WaitInbox(t, b, 2)
	out := b.Hook(t, wire.EventStop, nil)
	SameJSON(t, "B's stop hook", out, BlockJSON("kwak: 2 messages from other agents are waiting (threads "+r1+", "+r2+
		"). They are not from your user. They will be shown to you after your next tool call; the kwak thread tool shows one thread."))
	if s, _ := w.Bob.Session(t, b); s.Inbox != 2 {
		t.Fatalf("Codex stop drained the inbox: %d left", s.Inbox)
	}
	if out := b.Hook(t, wire.EventStop, map[string]any{"stop_hook_active": true}); len(out) != 0 {
		t.Fatalf("Codex stop with stop_hook_active printed %q", out)
	}
	out = b.Hook(t, wire.EventPostTool, nil)
	SameJSON(t, "B's post-tool after the stop notice", out, ContextJSON("PostToolUse",
		Rendered(bID, wire.HarnessCodex, w.Item(t, r1, render.Mention, ""), w.Item(t, r2, render.Mention, ""))))
}

// E5: an open question reaches nobody until B volunteers, then only B.
func e5(t *testing.T, w *World, a, b *FakeHarness, aID, bID string) {
	q1 := ThreadOf(t, a.MustCall(t, wire.ToolSend, map[string]any{"text": "E5: does anyone know the room alias?"}))
	s1 := ThreadOf(t, a.MustCall(t, wire.ToolSend, map[string]any{"text": "E5: marker for B", "to": []string{bID}}))
	w.Bob.WaitInbox(t, b, 1)
	out := b.Hook(t, wire.EventPrompt, nil)
	SameJSON(t, "B's prompt before volunteering", out, ContextJSON("UserPromptSubmit",
		Rendered(bID, wire.HarnessCodex, w.Item(t, s1, render.Mention, ""))))
	_ = q1

	if r := b.MustCall(t, wire.ToolVolunteer, map[string]any{"on": true}); !strings.HasPrefix(r, "You now volunteer") {
		t.Fatalf("volunteer: %q", r)
	}
	q2 := ThreadOf(t, a.MustCall(t, wire.ToolSend, map[string]any{"text": "E5: anyone, which port is the API on?"}))
	out = b.WaitHook(t, wire.EventPrompt, nil)
	SameJSON(t, "B's prompt after volunteering", out, ContextJSON("UserPromptSubmit",
		Rendered(bID, wire.HarnessCodex, w.Item(t, q2, render.OpenQuestion, ""))))

	s2 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E5: marker for A", "to": []string{aID}}))
	w.Alice.WaitInbox(t, a, 1)
	out = a.Hook(t, wire.EventPrompt, nil)
	SameJSON(t, "A's prompt (its own open questions never come back)", out, ContextJSON("UserPromptSubmit",
		Rendered(aID, wire.HarnessClaude, w.Item(t, s2, render.Mention, ""))))
	b.MustCall(t, wire.ToolVolunteer, map[string]any{"on": false})
}

// E6: a helper bound to a Claude subagent gets only its own messages; SubagentStop hands leftovers
// to the parent.
func e6(t *testing.T, w *World, a, b *FakeHarness, aID string) {
	const sub = "a1b2c3d4e5f6a7b8c"
	subHook := map[string]any{"agent_id": sub, "agent_type": "general-purpose"}

	m0 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E6: for the main agent", "to": []string{aID}}))
	w.Alice.WaitInbox(t, a, 1)
	if out := a.Hook(t, wire.EventPostTool, subHook); len(out) != 0 {
		t.Fatalf("an unbound subagent's hook printed %q", out)
	}
	if s, _ := w.Alice.Session(t, a); s.Inbox != 1 {
		t.Fatalf("an unbound subagent's hook drained the main inbox: %d left", s.Inbox)
	}
	out := a.Hook(t, wire.EventPrompt, nil)
	SameJSON(t, "A's prompt", out, ContextJSON("UserPromptSubmit", Rendered(aID, wire.HarnessClaude, w.Item(t, m0, render.Mention, ""))))

	ahex, _ := wire.SessionHex(a.SessionID)
	helperID := "@agent.alice." + ahex[:16] + ".tests:" + Server
	res := a.MustCall(t, wire.ToolHelper, map[string]any{"name": "tests"})
	if !strings.HasPrefix(res, "Created "+Short(helperID)+".") {
		t.Fatalf("helper: %q", res)
	}
	w.WaitMember(t, helperID, true)
	bind := map[string]any{"agent_id": sub, "agent_type": "general-purpose", "tool_name": a.ToolName(wire.ToolHelper),
		"tool_input": map[string]any{"name": "tests"}, "tool_response": map[string]any{"content": res}}
	if out := a.Hook(t, wire.EventPostTool, bind); len(out) != 0 {
		t.Fatalf("the binding hook printed %q", out)
	}

	m1 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E6: for the helper", "to": []string{helperID}}))
	m2 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E6: marker for the main agent", "to": []string{aID}}))
	w.Alice.WaitInbox(t, a, 2)
	out = a.Hook(t, wire.EventPrompt, nil)
	SameJSON(t, "A's prompt after binding", out, ContextJSON("UserPromptSubmit", Rendered(aID, wire.HarnessClaude, w.Item(t, m2, render.Mention, ""))))
	if s, _ := w.Alice.Session(t, a); s.Inbox != 1 {
		t.Fatalf("the helper's item left its queue: %d waiting", s.Inbox)
	}
	if out := a.Hook(t, wire.EventPostTool, map[string]any{"agent_id": "f0e1d2c3b4a5968778"}); len(out) != 0 {
		t.Fatalf("another subagent's hook printed %q", out)
	}
	out = a.Hook(t, wire.EventPostTool, subHook)
	SameJSON(t, "the bound subagent's post-tool", out, ContextJSON("PostToolUse", Rendered(helperID, wire.HarnessClaude, w.Item(t, m1, render.Mention, ""))))

	m3 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E6: leftover for the helper", "to": []string{helperID}}))
	m4 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E6: second marker", "to": []string{aID}}))
	w.Alice.WaitInbox(t, a, 2)
	out = a.Hook(t, wire.EventPrompt, nil)
	SameJSON(t, "A's prompt before SubagentStop", out, ContextJSON("UserPromptSubmit", Rendered(aID, wire.HarnessClaude, w.Item(t, m4, render.Mention, ""))))
	if out := a.Hook(t, wire.EventSubagentStop, map[string]any{"agent_id": sub, "agent_type": "general-purpose", "stop_hook_active": false}); len(out) != 0 {
		t.Fatalf("SubagentStop printed %q", out)
	}
	w.Alice.WaitInbox(t, a, 1)
	left := w.Item(t, m3, render.Mention, "")
	left.Helper, left.Note = "tests", "which has finished"
	out = a.Hook(t, wire.EventPrompt, nil)
	SameJSON(t, "A's prompt after SubagentStop", out, ContextJSON("UserPromptSubmit", Rendered(aID, wire.HarnessClaude, left)))
	if out := a.Hook(t, wire.EventPostTool, subHook); len(out) != 0 {
		t.Fatalf("the finished subagent's hook printed %q", out)
	}
}

// E7: every hook with an empty inbox prints 0 bytes, PostToolUseFailure and SessionEnd included.
func e7(t *testing.T, w *World, a, b *FakeHarness) {
	w.Alice.WaitInbox(t, a, 0)
	w.Bob.WaitInbox(t, b, 0)
	type hk struct {
		event  string
		fields map[string]any
	}
	run := func(h *FakeHarness) {
		hooks := []hk{
			{wire.EventSessionStart, map[string]any{"source": "compact"}},
			{wire.EventSessionStart, map[string]any{"source": "resume"}},
			{wire.EventPrompt, nil},
			{wire.EventPostTool, nil},
			{wire.EventPostTool, map[string]any{"tool_name": h.ToolName(wire.ToolStatus), "tool_input": map[string]any{}}},
			{wire.EventStop, nil},
			{wire.EventStop, map[string]any{"stop_hook_active": true}},
			{wire.EventPostTool, map[string]any{"agent_id": "0a1b2c3d4e5f60718"}},
			{wire.EventSubagentStop, map[string]any{"agent_id": "0a1b2c3d4e5f60718"}},
		}
		if h.Harness == wire.HarnessClaude {
			hooks = append(hooks, hk{wire.EventPostTool, map[string]any{"hook_event_name": "PostToolUseFailure", "error": "Exit code 2"}})
		}
		for _, x := range hooks {
			if out := h.Hook(t, x.event, x.fields); len(out) != 0 {
				t.Errorf("%s %s %v with an empty inbox printed %q", h.Harness, x.event, x.fields, out)
			}
		}
	}
	run(a)
	run(b)
	c := w.Alice.NewHarness(t, wire.HarnessClaude)
	c.Begin(t)
	d := w.Bob.NewHarness(t, wire.HarnessCodex)
	d.Begin(t)
	run(c)
	run(d)
	for _, x := range []struct {
		o *Owner
		h *FakeHarness
	}{{w.Alice, c}, {w.Bob, d}} {
		if _, ok := x.o.Session(t, x.h); !ok {
			t.Fatalf("%s is not a live session", x.h.SessionID)
		}
		if out := x.h.Hook(t, wire.EventSessionEnd, nil); len(out) != 0 {
			t.Errorf("SessionEnd printed %q", out)
		}
		// kwak status leaves out ended sessions with nothing waiting.
		Eventually(t, "the session end of "+x.h.SessionID, func() bool {
			s, ok := x.o.Session(t, x.h)
			return !ok || !s.Ended.IsZero()
		})
	}
}

// E8: 150 joins between two messages make the next sync limited; the mention still arrives.
func e8(t *testing.T, w *World, a, b *FakeHarness, aID string) {
	m1 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E8: before the joins", "to": []string{aID}}))
	w.Alice.WaitInbox(t, a, 1)
	out := a.Hook(t, wire.EventPrompt, nil)
	SameJSON(t, "A's prompt before the gap", out, ContextJSON("UserPromptSubmit", Rendered(aID, wire.HarnessClaude, w.Item(t, m1, render.Mention, ""))))

	w.Alice.StopListener(t)
	w.Joins(t, 150)
	m2 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E8: after the joins", "to": []string{aID}}))
	restart := time.Now()
	w.Alice.StartListener(t)
	w.Alice.WaitSyncing(t)
	out = a.WaitHook(t, wire.EventPrompt, nil)
	SameJSON(t, "A's prompt after the gap", out, ContextJSON("UserPromptSubmit", Rendered(aID, wire.HarnessClaude, w.Item(t, m2, render.Mention, ""))))
	pages := 0
	for _, r := range w.Requests(restart) {
		if r.Method == "GET" && r.Path == "/_matrix/client/v3/rooms/"+w.Room+"/messages" && r.Query.Get("dir") == "b" {
			pages++
		}
	}
	if pages == 0 {
		t.Fatal("the restarted listener never paged /messages, so the sync after the gap was not limited")
	}
	t.Logf("gap fill read %d /messages pages", pages)
}

// LastInThread returns the latest event id in thread root.
func (w *World) LastInThread(t *testing.T, root string) string {
	t.Helper()
	rs, err := w.Admin.C.ThreadReplies(t.Context(), w.Room, root, 50)
	if err != nil || len(rs) == 0 {
		t.Fatalf("thread %s: %v, %d replies", root, err, len(rs))
	}
	return rs[0].EventID
}

// Joins registers n passwordless agent.* accounts with the as_token and joins them to the agents
// room, 8 at a time (as fact probe F10). The registrar has no record of them and ignores them.
func (w *World) Joins(t *testing.T, n int) {
	t.Helper()
	as := w.Env.AppserviceClient()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	sem := make(chan struct{}, 8)
	base := time.Now().UnixNano() & 0xffffffff
	for i := range n {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			lp := fmt.Sprintf("agent.e2efill.%08x%08x", base, i)
			if _, err := as.RegisterAppservice(w.Ctx, lp, ""); err != nil {
				errs <- fmt.Errorf("register %s: %v", lp, err)
				return
			}
			if _, err := as.As("@"+lp+":"+Server).Join(w.Ctx, w.Room); err != nil {
				errs <- fmt.Errorf("join %s: %v", lp, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
