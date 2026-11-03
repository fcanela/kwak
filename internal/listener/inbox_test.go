package listener

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

// claudeSession starts a Claude session at harness pid p (hooks from p+10) and returns its agent.
func (e *env) claudeSession(p int, sid, dir string) string {
	e.t.Helper()
	h := e.claudeTree(p)
	e.child(h, p+10)
	e.start(p+10, "claude", sid, "startup", dir)
	return e.waitAgent(sessionKey("claude", sid))
}

func (e *env) prompt(p int, sid, dir string) wire.HookResponse {
	return e.hook(p+10, hookReq("claude", wire.EventPrompt, sid, dir))
}

func mentionEvents(n int, to string, ts time.Time) []matrix.Event {
	var evs []matrix.Event
	for i := 0; i < n; i++ {
		evs = append(evs, event(fmt.Sprintf("$m%03d", i), bob, ts.Add(time.Duration(i)*time.Second), text(fmt.Sprintf("msg-%03d", i), to)))
	}
	return evs
}

func TestDrainAtomicUnderConcurrency(t *testing.T) {
	e := newEnv(t, budgetOpt("claude", 900))
	e.connect()
	uid := e.claudeSession(100, sid1, cwd)
	e.deliver(mentionEvents(120, uid, e.clk.Now())...)
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				r := e.prompt(100, sid1, cwd)
				if r.Text == "" {
					return
				}
				mu.Lock()
				for _, line := range strings.Split(r.Text, "\n") {
					if b, ok := strings.CutPrefix(line, "| msg-"); ok {
						seen[b]++
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 120 {
		t.Fatalf("%d distinct items delivered, want 120", len(seen))
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("item %s delivered %d times", k, n)
		}
	}
}

func TestDrainBudgetAndStateBeforeReply(t *testing.T) {
	e := newEnv(t, budgetOpt("claude", 900))
	e.connect()
	uid := e.claudeSession(100, sid1, cwd)
	e.deliver(mentionEvents(20, uid, e.clk.Now())...)
	r := e.prompt(100, sid1, cwd)
	if r.More == 0 || !strings.Contains(r.Text, fmt.Sprintf("%d more messages are waiting", r.More)) {
		t.Fatalf("more %d, text %q", r.More, r.Text)
	}
	data, err := os.ReadFile(filepath.Join(e.dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	var st state
	json.Unmarshal(data, &st)
	if _, ok := st.Sessions[k1].Inbox.Delivered["$m000"]; !ok {
		t.Fatal("state.json was not written before the reply")
	}
}

func TestDeliveredDedupesReroutedBatch(t *testing.T) {
	e := newEnv(t)
	e.connect()
	uid := e.claudeSession(100, sid1, cwd)
	evs := mentionEvents(60, uid, e.clk.Now())
	e.deliver(evs...)
	for e.prompt(100, sid1, cwd).Text != "" {
	}
	e.deliver(evs...)
	if n := len(e.items(k1)); n != 0 {
		t.Fatalf("%d items came back after a re-route", n)
	}
}

func TestHeldMessagesMoveOnceByCwd(t *testing.T) {
	e := newEnv(t)
	e.connect()
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	uid := e.claudeSession(100, sid1, real)
	e.hook(110, hookReq("claude", wire.EventSessionEnd, sid1, real))
	e.deliver(event("$q", bob, e.clk.Now(), text("for the old one", uid)))
	e.clk.Advance(time.Minute)
	e.child(e.claudeTree(200), 210)
	got := e.start(210, "claude", sid2, "startup", link).Text
	if !strings.Contains(got, "earlier agent @agent.fcanela.4f2a9c1e7b034d5a (that session ended)") || !strings.Contains(got, "| for the old one") {
		t.Fatalf("SessionStart after the move: %q", got)
	}
	e.clk.Advance(time.Minute)
	e.child(e.claudeTree(300), 310)
	if got := e.start(310, "claude", sid3, "startup", real).Text; got != "" {
		t.Fatalf("moved twice: %q", got)
	}
	r := e.start(110, "claude", sid1, "resume", real)
	if !strings.Contains(r.Text, "1 message for you was handed to session "+sid2+" in this directory on 2026-11-02.") {
		t.Fatalf("hand-over marker: %q", r.Text)
	}
	if r := e.start(110, "claude", sid1, "resume", real); r.Text != "" {
		t.Fatalf("marker shown twice: %q", r.Text)
	}
}

func TestHeldTiesGoToTheFirstStart(t *testing.T) {
	e := newEnv(t)
	e.connect()
	dir := t.TempDir()
	uid := e.claudeSession(100, sid1, dir)
	e.hook(110, hookReq("claude", wire.EventSessionEnd, sid1, dir))
	e.deliver(event("$q", bob, e.clk.Now(), text("x", uid)))
	h2, h3 := e.claudeTree(200), e.claudeTree(300)
	e.child(h2, 210)
	e.child(h3, 310)
	var wg sync.WaitGroup
	texts := make([]string, 2)
	for i, p := range []int{210, 310} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			texts[i] = e.start(p, "claude", []string{sid2, sid3}[i], "startup", dir).Text
		}()
	}
	wg.Wait()
	if (texts[0] == "") == (texts[1] == "") {
		t.Fatalf("held item went to both or neither: %q", texts)
	}
}

func TestCwdCaseFolding(t *testing.T) {
	if runtime.GOOS == "linux" {
		if normCwd("/A/b") == normCwd("/a/b") {
			t.Fatal("Linux paths are case-sensitive")
		}
		return
	}
	if normCwd("/A/b") != normCwd("/a/b") {
		t.Fatal("case not folded")
	}
}

func TestHeldKeptUntilNinetyDays(t *testing.T) {
	e := newEnv(t)
	e.connect()
	uid := e.claudeSession(100, sid1, cwd)
	e.hook(110, hookReq("claude", wire.EventSessionEnd, sid1, cwd))
	e.clk.Advance(closeGrace)
	e.l.deadlines()
	e.deliver(event("$q", bob, e.clk.Now(), text("x", uid)))
	e.clk.Advance(89 * 24 * time.Hour)
	e.l.liveness()
	if s := e.session(k1); s == nil || len(s.Inbox.Items) != 1 {
		t.Fatal("dropped before 90 days")
	}
	e.clk.Advance(2 * 24 * time.Hour)
	e.l.liveness()
	if e.session(k1) != nil || len(e.l.st.Mine) != 0 {
		t.Fatal("not pruned after 90 days")
	}
}

func TestQuestionTimeoutAndMembership(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	e.room.members = []string{wiretest.ClaudeAgent}
	e.room.ids = []string{"$ask"}
	r := e.tool(c, "send", `{"text":"anyone?","to":["agent.bob.0718a1b2c3d4e5f6","@alice"]}`)
	want := "Sent as @agent.fcanela.4f2a9c1e7b034d5a in thread $ask.\n" +
		"@agent.bob.0718a1b2c3d4e5f6 is not in the room: its session has ended. The message waits for its owner's next session.\n" +
		"@alice is not in the room: its session has ended. The message waits for its owner's next session.\n" + textNoPoll
	if r.Text != want {
		t.Fatalf("send:\n%s", r.Text)
	}
	m := e.room.sentContents()[0]
	if ids := m["m.mentions"].(map[string]any)["user_ids"].([]any); len(ids) != 2 || ids[1] != human {
		t.Fatalf("mentions %v", m["m.mentions"])
	}
	e.clk.Advance(14 * time.Minute)
	e.l.deadlines()
	if n := len(e.items(k1)); n != 0 {
		t.Fatal("timeout fired early")
	}
	e.clk.Advance(time.Minute)
	e.l.deadlines()
	got := e.items(k1)
	if len(got) != 1 || got[0].Reason != render.Notice ||
		got[0].Body != "No reply in thread $ask after 15 minutes. Assume the agents you asked are shut down. Carry on without the answer, or ask someone else." {
		t.Fatalf("timeout item %+v", got)
	}
	e.deliver(event("$late", bob, e.clk.Now(), inThread(text("late"), "$ask")))
	if got := e.items(k1); len(got) != 2 || got[1].Reason != render.YourThread {
		t.Fatalf("a late reply is not delivered: %+v", got)
	}
}

func TestReplyCancelsQuestionTimeout(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	e.room.ids = []string{"$ask"}
	e.tool(c, "send", `{"text":"q","to":["agent.bob.0718a1b2c3d4e5f6"]}`)
	e.deliver(event("$a", bob, e.clk.Now(), inThread(text("answer"), "$ask")))
	e.clk.Advance(time.Hour)
	e.l.deadlines()
	for _, it := range e.items(k1) {
		if it.Reason == render.Notice {
			t.Fatal("timeout after a reply")
		}
	}
}

func TestSendRefusesRemoteIDs(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	r := e.tool(c, "send", `{"text":"q","to":["@bob:matrix.org"]}`)
	if !r.IsError || r.Text != "kwak only knows accounts on kwak.test" || len(e.room.sent) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestReplyMentionsAndRelation(t *testing.T) {
	e := newEnv(t)
	e.connect()
	me := e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	ts := e.clk.Now()
	e.room.events["$root"] = event("$root", mallory, ts, text("q", me))
	e.room.events["$inner"] = event("$inner", bob, ts, inThread(text("x"), "$root"))
	e.room.replies["$root"] = []matrix.Event{
		event("$inner", bob, ts, inThread(text("x"), "$root")),
		event("$mine", me, ts, inThread(text("y"), "$root")),
	}
	r := e.tool(c, "reply", `{"thread":"$inner","text":"answer"}`)
	if r.IsError || !strings.HasPrefix(r.Text, "Replied as @agent.fcanela.4f2a9c1e7b034d5a in thread $root, mentioning @agent.mallory.8261713321e78fea, @agent.bob.0718a1b2c3d4e5f6.") {
		t.Fatalf("%+v", r)
	}
	rel := e.room.sentContents()[0]["m.relates_to"].(map[string]any)
	if rel["event_id"] != "$root" || rel["rel_type"] != "m.thread" || rel["m.in_reply_to"].(map[string]any)["event_id"] != "$mine" {
		t.Fatalf("relation %v", rel)
	}
}

func TestThreadMarksShownItemsInCallerQueueOnly(t *testing.T) {
	e := newEnv(t)
	e.connect()
	a := e.claudeSession(100, sid1, cwd)
	b := e.claudeSession(200, sid2, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	ts := e.clk.Now()
	root := event("$root", bob, ts, text("q", a, b))
	e.room.events["$root"] = root
	e.deliver(root)
	if r := e.tool(c, "thread", `{"thread":"$root"}`); r.IsError || !strings.Contains(r.Text, "no replies yet") {
		t.Fatalf("%+v", r)
	}
	if len(e.items(k1)) != 0 || len(e.items(k2)) != 1 {
		t.Fatalf("queues %d and %d", len(e.items(k1)), len(e.items(k2)))
	}
}

func TestHelpers(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	h := proc.ID{PID: 100, Start: 100}
	c := e.mcp(e.child(h, 120).PID, "claude", "")
	if r := e.tool(c, "helper", `{"name":"tests"}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	hk := hookReq("claude", wire.EventPostTool, sid1, cwd)
	hk.AgentID, hk.ToolName, hk.ToolInput = "a7f3", "mcp__plugin_kwak_kwak__helper", json.RawMessage(`{"name":"tests"}`)
	e.hook(110, hk)
	e.deliver(event("$h1", bob, e.clk.Now(), text("for the helper", wiretest.HelperAgent)))
	if r := e.prompt(100, sid1, cwd); r.Text != "" {
		t.Fatal("the main inbox got the bound helper's item")
	}
	other := hookReq("claude", wire.EventPostTool, sid1, cwd)
	other.AgentID = "zzz"
	if r := e.hook(110, other); r.Text != "" {
		t.Fatal("an unbound agent_id got items")
	}
	sub := hookReq("claude", wire.EventPostTool, sid1, cwd)
	sub.AgentID = "a7f3"
	if r := e.hook(110, sub); !strings.Contains(r.Text, "for @agent.fcanela.4f2a9c1e7b034d5a.tests") {
		t.Fatalf("helper drain: %q", r.Text)
	}
	e.deliver(event("$h2", bob, e.clk.Now(), text("later", wiretest.HelperAgent)))
	stop := hookReq("claude", wire.EventSubagentStop, sid1, cwd)
	stop.AgentID = "a7f3"
	e.hook(110, stop)
	r := e.prompt(100, sid1, cwd)
	if !strings.Contains(r.Text, "for your helper tests, which has finished") || !strings.Contains(r.Text, "| later") {
		t.Fatalf("hand-over: %q", r.Text)
	}
	if r := e.tool(c, "send", `{"text":"x","as":"tests"}`); r.IsError || !strings.HasPrefix(r.Text, "Sent as @agent.fcanela.4f2a9c1e7b034d5a.tests") {
		t.Fatalf("send as helper: %+v", r)
	}
	if r := e.tool(c, "send", `{"text":"x","as":"nope"}`); !r.IsError {
		t.Fatal("send as an unknown helper")
	}
}

func TestUnboundHelperItemsGoToMainInbox(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	e.tool(c, "helper", `{"name":"tests"}`)
	e.deliver(event("$h1", bob, e.clk.Now(), text("hi", wiretest.HelperAgent)))
	if r := e.prompt(100, sid1, cwd); !strings.Contains(r.Text, "for your helper tests") {
		t.Fatalf("%q", r.Text)
	}
}

func TestCodexHelperBoundToThreadID(t *testing.T) {
	e := newEnv(t)
	e.connect()
	codex := proc.ID{PID: 300, Start: 300}
	e.procs.Line(codex, proc.ID{PID: 299, Start: 299})
	sh := e.child(codex, 310)
	e.child(sh, 311)
	e.start(311, "codex", cx1, "startup", wiretest.CodexCwd)
	e.waitAgent(sessionKey("codex", cx1))
	c := e.mcp(e.child(codex, 330).PID, "codex", "")
	thread := "019a2b3c-0000-7000-8000-000000000001"
	if r := e.tool(c, "helper", `{"name":"tests"}`, cx1, thread); r.IsError {
		t.Fatalf("%+v", r)
	}
	r := e.tool(c, "send", `{"text":"from the subagent"}`, cx1, thread)
	if !strings.HasPrefix(r.Text, "Sent as @agent.fcanela.8261713321e78fea.tests") {
		t.Fatalf("%+v", r)
	}
	r = e.tool(c, "send", `{"text":"from the main thread"}`, cx1, cx1)
	if !strings.HasPrefix(r.Text, "Sent as @agent.fcanela.8261713321e78fea in") {
		t.Fatalf("%+v", r)
	}
}

func TestVolunteerOpenQuestions(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	e.deliver(event("$o1", bob, e.clk.Now(), text("anyone?")))
	if len(e.items(k1)) != 0 {
		t.Fatal("open question without volunteering")
	}
	e.tool(c, "volunteer", `{"on":true}`)
	e.deliver(event("$o2", bob, e.clk.Now(), text("anyone?")))
	if got := e.items(k1); len(got) != 1 || got[0].Reason != render.OpenQuestion {
		t.Fatalf("%+v", got)
	}
	e.hook(110, hookReq("claude", wire.EventSessionEnd, sid1, cwd))
	e.deliver(event("$o3", bob, e.clk.Now(), text("anyone?")))
	if len(e.items(k1)) != 1 {
		t.Fatal("an ended session still volunteers")
	}
}

func TestStopHooks(t *testing.T) {
	e := newEnv(t)
	e.connect()
	uid := e.claudeSession(100, sid1, cwd)
	e.deliver(event("$x", bob, e.clk.Now(), text("q", uid)))
	active := hookReq("claude", wire.EventStop, sid1, cwd)
	active.StopHookActive = true
	if r := e.hook(110, active); r.Text != "" || len(e.items(k1)) != 1 {
		t.Fatal("stop_hook_active drained")
	}
	if r := e.hook(110, hookReq("claude", wire.EventStop, sid1, cwd)); r.Text == "" || len(e.items(k1)) != 0 {
		t.Fatal("stop did not drain")
	}
}

func TestMissedNotice(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	e.l.mu.Lock()
	e.l.missedLocked()
	e.l.mu.Unlock()
	if r := e.prompt(100, sid1, cwd); !strings.Contains(r.Text, "| some messages may have been missed") {
		t.Fatalf("%q", r.Text)
	}
}

func TestHeldMovesAcrossHarness(t *testing.T) {
	e := newEnv(t)
	e.connect()
	dir := t.TempDir()
	uid := e.claudeSession(100, sid1, dir)
	e.hook(110, hookReq("claude", wire.EventSessionEnd, sid1, dir))
	e.deliver(event("$q", bob, e.clk.Now(), text("for claude", uid)))
	codex := proc.ID{PID: 300, Start: 300}
	e.procs.Line(codex, proc.ID{PID: 299, Start: 299})
	e.child(e.child(codex, 310), 311)
	if got := e.start(311, "codex", cx1, "startup", dir).Text; !strings.Contains(got, "| for claude") {
		t.Fatalf("Codex SessionStart in the same cwd: %q", got)
	}
}

func TestHeldMovesOnlyOnStartupOrClear(t *testing.T) {
	for _, src := range []string{"", "other"} {
		e := newEnv(t)
		e.connect()
		dir := t.TempDir()
		uid := e.claudeSession(100, sid1, dir)
		e.hook(110, hookReq("claude", wire.EventSessionEnd, sid1, dir))
		e.deliver(event("$q", bob, e.clk.Now(), text("x", uid)))
		e.child(e.claudeTree(200), 210)
		if got := e.start(210, "claude", sid2, src, dir).Text; got != "" {
			t.Fatalf("source %q moved held items: %q", src, got)
		}
		if n := len(e.items(k1)); n != 1 {
			t.Fatalf("source %q: %d items left, want 1", src, n)
		}
	}
}
