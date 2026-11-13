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
	if !strings.Contains(r.Text, "1 message for you was handed to session "+sid2+" in this directory on 2026-11-02 (thread $q). "+
		"Read it with the kwak thread tool.") {
		t.Fatalf("hand-over marker: %q", r.Text)
	}
	if r := e.start(110, "claude", sid1, "resume", real); r.Text != "" {
		t.Fatalf("marker shown twice: %q", r.Text)
	}
}

func TestHandoverNoticeNamesThreads(t *testing.T) {
	e := newEnv(t)
	e.connect()
	dir := t.TempDir()
	uid := e.endedClaude(100, sid1, dir, "$mine")
	e.deliver(event("$q", bob, e.clk.Now(), text("new question", uid)),
		event("$r1", bob, e.clk.Now(), inThread(text("first answer"), "$mine")),
		event("$r2", mallory, e.clk.Now(), inThread(text("second answer"), "$mine")))
	e.l.mu.Lock()
	e.l.st.Sessions[k1].Inbox.add(render.Item{EventID: "kwak-missed-1", SentAt: e.clk.Now(), Reason: render.Notice, Body: "x"})
	e.l.st.Sessions[k1].Inbox.add(render.Item{EventID: "kwak-timeout-$old", ThreadRoot: "$old", SentAt: e.clk.Now(),
		Reason: render.Notice, Body: "No reply"})
	e.l.mu.Unlock()
	e.child(e.claudeTree(200), 210)
	e.start(210, "claude", sid2, "startup", dir)
	r := e.start(110, "claude", sid1, "resume", dir)
	want := "5 messages for you were handed to session " + sid2 + " in this directory on 2026-11-02 " +
		"(threads $q, $mine, $old). Read them with the kwak thread tool."
	if !strings.Contains(r.Text, want) {
		t.Fatalf("hand-over notice: %q", r.Text)
	}
}

func TestHandoverNoticeCapsThreads(t *testing.T) {
	e := newEnv(t)
	e.connect()
	dir := t.TempDir()
	uid := e.claudeSession(100, sid1, dir)
	e.hook(110, hookReq("claude", wire.EventSessionEnd, sid1, dir))
	for i := 0; i < handoverThreads+3; i++ {
		e.deliver(event(fmt.Sprintf("$q%02d", i), bob, e.clk.Now(), text("q", uid)))
	}
	e.child(e.claudeTree(200), 210)
	e.start(210, "claude", sid2, "startup", dir)
	r := e.start(110, "claude", sid1, "resume", dir)
	if !strings.Contains(r.Text, "$q08, $q09, and 3 more). Read them") || strings.Contains(r.Text, "$q10") {
		t.Fatalf("hand-over notice: %q", r.Text)
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
		"@alice is a person: kwak does not ping people.\n" +
		"@agent.bob.0718a1b2c3d4e5f6 is not in the room: its session has ended. The message waits for its owner's next session.\n" + textNoPoll
	if r.Text != want {
		t.Fatalf("send:\n%s", r.Text)
	}
	m := e.room.sentContents()[0]
	if ids := m["m.mentions"].(map[string]any)["user_ids"].([]any); len(ids) != 1 || ids[0] != bob {
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

func TestMemberListRefreshedOnAMiss(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	gone := "@agent.bob.0718a1b2c3d4e5f6 is not in the room"
	calls := func() int {
		e.room.mu.Lock()
		defer e.room.mu.Unlock()
		return e.room.memberN
	}
	e.room.members = []string{wiretest.ClaudeAgent, mallory}
	if r := e.tool(c, "send", `{"text":"q","to":["`+mallory+`"]}`); strings.Contains(r.Text, "not in the room") || calls() != 1 {
		t.Fatalf("first send: %d calls, %q", calls(), r.Text)
	}
	if r := e.tool(c, "send", `{"text":"q","to":["`+mallory+`"]}`); calls() != 1 {
		t.Fatalf("a hit refetched the list: %d calls, %q", calls(), r.Text)
	}
	e.room.mu.Lock()
	e.room.members = append(e.room.members, bob)
	e.room.mu.Unlock()
	e.clk.Advance(10 * time.Second)
	if r := e.tool(c, "send", `{"text":"q","to":["`+bob+`"]}`); strings.Contains(r.Text, gone) || calls() != 2 {
		t.Fatalf("an agent created after the cached list: %d calls, %q", calls(), r.Text)
	}
	carol := `{"text":"q","to":["agent.carol.1111222233334444"]}`
	if r := e.tool(c, "send", carol); !strings.Contains(r.Text, "@agent.carol.1111222233334444 is not in the room") || calls() != 3 {
		t.Fatalf("a real miss: %d calls, %q", calls(), r.Text)
	}
	e.clk.Advance(10 * time.Second)
	if r := e.tool(c, "send", carol); !strings.Contains(r.Text, "is not in the room") || calls() != 3 {
		t.Fatalf("a miss a fresh list already confirmed fetched again: %d calls, %q", calls(), r.Text)
	}
	e.clk.Advance(time.Minute)
	if r := e.tool(c, "send", carol); !strings.Contains(r.Text, "is not in the room") || calls() != 4 {
		t.Fatalf("after the cache expired: %d calls, %q", calls(), r.Text)
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

func TestOpenQuestionTimeout(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	e.room.ids = []string{"$open", "$answered"}
	e.tool(c, "send", `{"text":"anyone?"}`)
	e.tool(c, "send", `{"text":"anyone else?"}`)
	// An older kwak restored over state.json reads only "questions" (spec 5.2): open questions
	// stay out of it, so it never sends them the text for questions with mentions.
	e.l.mu.Lock()
	nq, no := len(e.l.st.Questions), len(e.l.st.OpenQs)
	e.l.mu.Unlock()
	if nq != 0 || no != 2 {
		t.Fatalf("questions %d, open questions %d", nq, no)
	}
	e.deliver(event("$a", bob, e.clk.Now(), inThread(text("me"), "$answered")))
	e.clk.Advance(15 * time.Minute)
	e.l.deadlines()
	var notices []render.Item
	for _, it := range e.items(k1) {
		if it.Reason == render.Notice {
			notices = append(notices, it)
		}
	}
	if len(notices) != 1 || notices[0].ThreadRoot != "$open" || notices[0].Body != "No reply in thread $open after 15 minutes. "+
		"Nobody answered your open question. Carry on without the answer, or ask an agent directly." {
		t.Fatalf("open question notices %+v", notices)
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
	if r.IsError || !strings.HasPrefix(r.Text, "Replied as @agent.fcanela.4f2a9c1e7b034d5a in thread $root, mentioning @agent.mallory.8261713321e78fea.\n") {
		t.Fatalf("%+v", r)
	}
	sent := e.room.sentContents()
	if ids := sent[0]["m.mentions"].(map[string]any)["user_ids"].([]any); len(ids) != 1 || ids[0] != mallory {
		t.Fatalf("an earlier writer was mentioned without being asked for: %v", ids)
	}
	rel := sent[0]["m.relates_to"].(map[string]any)
	if rel["event_id"] != "$root" || rel["rel_type"] != "m.thread" || rel["m.in_reply_to"].(map[string]any)["event_id"] != "$mine" {
		t.Fatalf("relation %v", rel)
	}
	r = e.tool(c, "reply", `{"thread":"$root","text":"thanks","to":["agent.bob.0718a1b2c3d4e5f6"]}`)
	if r.IsError || !strings.HasPrefix(r.Text, "Replied as @agent.fcanela.4f2a9c1e7b034d5a in thread $root, mentioning @agent.mallory.8261713321e78fea, @agent.bob.0718a1b2c3d4e5f6.") {
		t.Fatalf("%+v", r)
	}
	if ids := e.room.sentContents()[1]["m.mentions"].(map[string]any)["user_ids"].([]any); len(ids) != 2 || ids[1] != bob {
		t.Fatalf("an explicit mention was lost: %v", ids)
	}
}

// An agent-shaped id in 'to' that is not a valid agent name (a typo, a cut-off hex) is refused, in
// send and in reply. It is not taken for a person: the agent namespace belongs to kwak (spec 3.7),
// so such an id is a mistake, and an open question would reach the wrong agents.
func TestToRefusesBadAgentID(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	for _, call := range []struct{ tool, args string }{
		{"send", `{"text":"q","to":["agent.bob.0718a1b2"]}`},
		{"send", `{"text":"q","to":["@alice","@agent.bob.0718a1b2:` + wiretest.ServerName + `"]}`},
		{"reply", `{"thread":"$root","text":"q","to":["agent.bob"]}`},
	} {
		r := e.tool(c, call.tool, call.args)
		if !r.IsError || !strings.Contains(r.Text, "is not a valid agent id") || strings.Contains(r.Text, "is a person") {
			t.Fatalf("%s %s:\n%s", call.tool, call.args, r.Text)
		}
	}
	if n := len(e.room.sentContents()); n != 0 {
		t.Fatalf("%d messages sent", n)
	}
	e.l.mu.Lock()
	n := len(e.l.st.OpenQs) + len(e.l.st.Questions)
	e.l.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d questions stored", n)
	}
}

// kwak never pings a person (decision 38, spec 6.4): a person in 'to' is left out of m.mentions and
// the result says so. A send that names only persons mentions nobody, so it is an open question
// for the room and for the no-reply notice.
func TestSendNeverPingsPersons(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	e.room.members = []string{wiretest.ClaudeAgent, bob}
	e.room.ids = []string{"$both", "$person"}
	r := e.tool(c, "send", `{"text":"q","to":["@alice","agent.bob.0718a1b2c3d4e5f6","agent.fcanela.listener"]}`)
	want := "Sent as @agent.fcanela.4f2a9c1e7b034d5a in thread $both.\n@alice is a person: kwak does not ping people.\n"
	if r.IsError || !strings.HasPrefix(r.Text, want) || strings.Contains(r.Text, "open question") {
		t.Fatalf("send to both:\n%s", r.Text)
	}
	sent := e.room.sentContents()
	if ids := sent[0]["m.mentions"].(map[string]any)["user_ids"].([]any); len(ids) != 2 || ids[0] != bob ||
		ids[1] != "@agent.fcanela.listener:"+wiretest.ServerName {
		t.Fatalf("mentions %v", ids)
	}
	r = e.tool(c, "send", `{"text":"q","to":["@alice:`+wiretest.ServerName+`"]}`)
	want = "Sent as @agent.fcanela.4f2a9c1e7b034d5a in thread $person.\n@alice is a person: kwak does not ping people.\n" +
		textOpenNoAgent + textNoPoll
	if r.IsError || r.Text != want {
		t.Fatalf("send to a person only:\n%s", r.Text)
	}
	m := e.room.sentContents()[1]
	if ids, _ := m["m.mentions"].(map[string]any)["user_ids"].([]any); len(ids) != 0 {
		t.Fatalf("a person was mentioned: %v", ids)
	}
	e.l.mu.Lock()
	_, asked := e.l.st.Questions["$both"]
	_, open := e.l.st.OpenQs["$person"]
	e.l.mu.Unlock()
	if !asked || !open {
		t.Fatalf("question kinds: asked %v, open %v", asked, open)
	}
	// On the receiving side, a message that mentions nobody is an open question (Route, 5.4).
	// Since decision 46 only a responder gets one.
	idx := &Index{Room: wiretest.RoomID, Mine: map[string]Agent{bob: {Session: "s"}}, Roots: map[string]string{},
		Threads: map[string]Thread{}, Responder: true, Owner: "bob", RespThreads: map[string]respThread{}}
	if ds := Route(event("$person", wiretest.ClaudeAgent, e.clk.Now(), m), idx); len(ds) != 1 || !ds[0].Responder ||
		ds[0].Reason != render.OpenQuestion {
		t.Fatalf("routed as %+v", ds)
	}
}

// A reply never pings a person: not the person who started the thread, nor one in 'to'. An agent
// that started the thread is still mentioned, and the root's own listener delivers the reply
// either way (decision 38, spec 6.4).
func TestReplyNeverPingsPersons(t *testing.T) {
	e := newEnv(t)
	e.connect()
	me := e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	ts := e.clk.Now()
	e.room.events["$byPerson"] = event("$byPerson", human, ts, text("which port?", me))
	r := e.tool(c, "reply", `{"thread":"$byPerson","text":"5432","to":["@alice","agent.bob.0718a1b2c3d4e5f6"]}`)
	want := "Replied as @agent.fcanela.4f2a9c1e7b034d5a in thread $byPerson, mentioning @agent.bob.0718a1b2c3d4e5f6.\n" +
		"@alice is a person: kwak does not ping people.\n"
	if r.IsError || !strings.HasPrefix(r.Text, want) {
		t.Fatalf("%+v", r)
	}
	if ids := e.room.sentContents()[0]["m.mentions"].(map[string]any)["user_ids"].([]any); len(ids) != 1 || ids[0] != bob {
		t.Fatalf("mentions %v", ids)
	}
	r = e.tool(c, "reply", `{"thread":"$byPerson","text":"thanks"}`)
	if r.IsError || strings.Contains(r.Text, "mentioning") || strings.Contains(r.Text, "is a person") {
		t.Fatalf("%+v", r)
	}
	if ids, _ := e.room.sentContents()[1]["m.mentions"].(map[string]any)["user_ids"].([]any); len(ids) != 0 {
		t.Fatalf("the person who started the thread was mentioned: %v", ids)
	}
	e.room.events["$byAgent"] = event("$byAgent", mallory, ts, text("q", me))
	if r := e.tool(c, "reply", `{"thread":"$byAgent","text":"a"}`); r.IsError || !strings.Contains(r.Text, ", mentioning "+e.l.short(mallory)+".") {
		t.Fatalf("an agent that started the thread was not mentioned: %+v", r)
	}
}

// A "thanks" from the thread's starter names nobody, so it wakes nobody (spec 6.4).
func TestReplyThanksWakesNobody(t *testing.T) {
	e := newEnv(t)
	e.connect()
	a := e.claudeSession(100, sid1, cwd)
	b := e.claudeSession(200, sid2, cwd)
	ca := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	e.room.ids = []string{"$ask", "$thanks"}
	e.tool(ca, "send", `{"text":"which port?","to":["`+b+`"]}`)
	ask := event("$ask", a, e.clk.Now(), text("which port?", b))
	e.room.events["$ask"] = ask
	ans := event("$ans", b, e.clk.Now(), inThread(text("5432", a), "$ask"))
	e.room.replies["$ask"] = []matrix.Event{ans}
	e.deliver(ask, ans)
	for e.prompt(100, sid1, cwd).Text != "" {
	}
	for e.prompt(200, sid2, cwd).Text != "" {
	}
	if r := e.tool(ca, "reply", `{"thread":"$ask","text":"thanks"}`); r.IsError || strings.Contains(r.Text, "mentioning") {
		t.Fatalf("%+v", r)
	}
	m := e.room.sentContents()[1]
	if ids, _ := m["m.mentions"].(map[string]any)["user_ids"].([]any); len(ids) != 0 {
		t.Fatalf("thanks mentions %v", ids)
	}
	e.deliver(event("$thanks", a, e.clk.Now(), m))
	if n := len(e.items(k2)); n != 0 {
		t.Fatalf("the answerer was woken by a thanks: %d items", n)
	}
}

// The asker gets an answer posted with kwak's reply without the refusal sentence, although reply
// mentions it. The answerer, asked directly, gets the sentence (spec 6.1).
func TestAnswerInOwnThreadAsksNoRefusal(t *testing.T) {
	const refuse = "If you will not or cannot answer a question asked of you"
	e := newEnv(t)
	e.connect()
	a := e.claudeSession(100, sid1, cwd)
	b := e.claudeSession(200, sid2, cwd)
	ca := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	cb := e.mcp(e.child(proc.ID{PID: 200, Start: 200}, 220).PID, "claude", "")
	e.room.ids = []string{"$ask", "$ans"}
	e.tool(ca, "send", `{"text":"which port?","to":["`+b+`"]}`)
	ask := event("$ask", a, e.clk.Now(), e.room.sentContents()[0])
	e.room.events["$ask"] = ask
	e.deliver(ask)
	if r := e.prompt(200, sid2, cwd); !strings.Contains(r.Text, "mentions you, new thread $ask") || !strings.Contains(r.Text, refuse) {
		t.Fatalf("the asked agent: %q", r.Text)
	}
	if r := e.tool(cb, "reply", `{"thread":"$ask","text":"5432"}`); r.IsError || !strings.Contains(r.Text, "mentioning "+e.l.short(a)) {
		t.Fatalf("%+v", r)
	}
	e.deliver(event("$ans", b, e.clk.Now(), e.room.sentContents()[1]))
	r := e.prompt(100, sid1, cwd)
	if !strings.Contains(r.Text, "| 5432") || strings.Contains(r.Text, refuse) {
		t.Fatalf("the asker was asked to refuse its own answer: %q", r.Text)
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

// "listener" is a reserved helper name (decision 42): listener account names end with it. The
// listener refuses it before asking the registrar, as a new helper, as a post's 'as', and as the
// helper name a PostToolUse hook would bind.
func TestHelperNameListenerIsReserved(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	before := e.reg.agentCalls()
	if r := e.tool(c, "helper", `{"name":"listener"}`); !r.IsError || !strings.Contains(r.Text, `"listener", which is reserved`) {
		t.Fatalf("helper listener: %+v", r)
	}
	if r := e.tool(c, "send", `{"text":"x","as":"listener"}`); !r.IsError || !strings.Contains(r.Text, `"listener", which is reserved`) {
		t.Fatalf("send as listener: %+v", r)
	}
	if n := e.reg.agentCalls(); n != before {
		t.Fatalf("%d registrar requests for a reserved name", n-before)
	}
	hk := hookReq("claude", wire.EventPostTool, sid1, cwd)
	hk.AgentID, hk.ToolName, hk.ToolInput = "a7f3", "mcp__plugin_kwak_kwak__helper", json.RawMessage(`{"name":"listener"}`)
	e.hook(110, hk)
	e.l.mu.Lock()
	defer e.l.mu.Unlock()
	if s := e.l.st.Sessions[k1]; s == nil || s.Helpers["listener"] != nil {
		t.Fatalf("a helper named listener was bound: %+v", s)
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

// Decision 46 removed volunteer: an open question never reaches a session, live or ended, and the
// tool is gone, so an older kwak mcp that still offers it gets an error.
func TestOpenQuestionsReachNoSession(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	e.deliver(event("$o1", bob, e.clk.Now(), text("anyone?")))
	if r := e.tool(c, "volunteer", `{"on":true}`); !r.IsError || r.Text != "kwak has no tool named volunteer" {
		t.Fatalf("volunteer: %+v", r)
	}
	e.deliver(event("$o2", bob, e.clk.Now(), text("anyone?")))
	if got := e.items(k1); len(got) != 0 {
		t.Fatalf("a session got open questions: %+v", got)
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
