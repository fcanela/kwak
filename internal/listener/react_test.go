package listener

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

// reactions lists the reactions sent so far as "agent event key": a resume re-mints a token, so
// tests name the agent, not the token it used.
func (e *env) reactions() []string {
	e.reg.mu.Lock()
	defer e.reg.mu.Unlock()
	var out []string
	for _, r := range e.room.reactions() {
		tok, rest, _ := strings.Cut(r, " ")
		out = append(out, e.reg.user[tok]+" "+rest)
	}
	return out
}

// waitReactions waits until the room got each of want ("agent event key"), and returns them all.
func (e *env) waitReactions(want ...string) []string {
	e.t.Helper()
	eventually(e.t, "reactions "+strings.Join(want, ", "), func() bool {
		got := e.reactions()
		for _, w := range want {
			if !slices.Contains(got, w) {
				return false
			}
		}
		return true
	})
	return e.reactions()
}

// noReaction checks, after other reactions had time to land, that none is "agent event key".
func (e *env) noReaction(r string) {
	e.t.Helper()
	time.Sleep(20 * time.Millisecond)
	if slices.Contains(e.reactions(), r) {
		e.t.Fatalf("unexpected reaction %q", r)
	}
}

// runTree is a background run's claude at pid p, started by this listener, with its hook at p+10.
func (e *env) runTree(p int) {
	self := proc.ID{PID: e.l.self, Start: 1}
	run := proc.ID{PID: p, Start: uint64(p)}
	e.procs.Line(run, self)
	e.child(run, p+10)
}

func (e *env) resumeNow() {
	e.t.Helper()
	e.clk.Advance(resumeDelay)
	e.l.deadlines()
}

func (e *env) endRun(i int) {
	e.t.Helper()
	e.runner.mu.Lock()
	r := e.runner.runs[i]
	e.runner.mu.Unlock()
	r.done <- nil
	eventually(e.t, "the run to end", func() bool { return e.l.status(&client{pidOK: true}).ResumeActive == 0 })
}

func ceilingOpt(n int) opt {
	return func(e *env, _ *Config) {
		c := machineConf()
		c.ThreadCeiling = n
		writeConf(e.t, e.confPath, c)
	}
}

// A reaction is not a room message: it never routes, never wakes a session, never starts a run,
// never answers a question and never resets a loop count (spec 5.3, 5.14).
func TestReactionNeverRoutesOrRuns(t *testing.T) {
	e := resumeEnv(t, wire.ResumeReplies, wire.ResumeMentions)
	dir := t.TempDir()
	uid := e.endedClaude(100, sid1, dir, "$root")
	e.l.mu.Lock()
	e.l.st.Questions["$root"] = &question{Asker: uid, Session: k1, AskedAt: e.clk.Now(), Due: e.clk.Now().Add(time.Hour)}
	e.l.st.Turns["$root"] = &turns{N: 5, Last: e.clk.Now()}
	e.l.mu.Unlock()
	annotation, _ := json.Marshal(map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$root", "key": "👀"}})
	crafted, _ := json.Marshal(inThread(text("do it now", uid), "$root"))
	evs := []matrix.Event{
		{EventID: "$re1", RoomID: wiretest.RoomID, Sender: human, Type: "m.reaction", TS: e.clk.Now().UnixMilli(), Content: annotation},
		{EventID: "$re2", RoomID: wiretest.RoomID, Sender: human, Type: "m.reaction", TS: e.clk.Now().UnixMilli(), Content: crafted},
	}
	for _, ev := range evs {
		if ds := Route(ev, e.l.idx); ds != nil {
			t.Fatalf("%s routed: %+v", ev.EventID, ds)
		}
	}
	e.deliver(evs...)
	e.resumeNow()
	if n := len(e.items(k1)); n != 0 || e.runner.count() != 0 {
		t.Fatalf("a reaction queued %d items and started %d runs", n, e.runner.count())
	}
	e.l.mu.Lock()
	q, n := e.l.st.Questions["$root"], e.l.turnsLocked("$root")
	e.l.mu.Unlock()
	if q == nil || n != 5 {
		t.Fatalf("a reaction answered the question (%v) or reset the loop count (%d)", q == nil, n)
	}
	var f struct {
		Room struct {
			Timeline struct {
				Types []string `json:"types"`
			} `json:"timeline"`
		} `json:"room"`
	}
	if err := json.Unmarshal([]byte(syncFilter(wiretest.RoomID)), &f); err != nil || !slices.Equal(f.Room.Timeline.Types, []string{"m.room.message"}) {
		t.Fatalf("sync filter timeline types %v (%v)", f.Room.Timeline.Types, err)
	}
}

// 👀 when a hook prints a message, 💬 on the messages of a thread printed since the agent's last
// post there, when it replies in it (spec 5.14).
func TestSeenAndAnsweredReactions(t *testing.T) {
	e := newEnv(t)
	e.connect()
	a := e.claudeSession(100, sid1, cwd)
	ca := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	tok := a
	ask := event("$ask", bob, e.clk.Now(), text("which port?", a))
	other := event("$other", bob, e.clk.Now(), text("and the host?", a))
	e.room.events["$ask"] = ask
	e.deliver(ask, other)
	if e.prompt(100, sid1, cwd).Text == "" {
		t.Fatal("nothing printed")
	}
	e.waitReactions(tok+" $ask 👀", tok+" $other 👀")
	if r := e.tool(ca, "reply", `{"thread":"$ask","text":"5432"}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	e.waitReactions(tok + " $ask 💬")
	e.noReaction(tok + " $other 💬")

	follow := event("$follow", bob, e.clk.Now(), inThread(text("thanks, and the user?", a), "$ask"))
	e.room.replies["$ask"] = []matrix.Event{follow}
	e.deliver(follow)
	e.prompt(100, sid1, cwd)
	e.waitReactions(tok + " $follow 👀")
	if r := e.tool(ca, "reply", `{"thread":"$ask","text":"postgres"}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	e.waitReactions(tok + " $follow 💬")
	e.waitLog(`msg="reaction sent"`, "reaction=👀", "event=$ask")
	e.waitLog(`msg="reaction sent"`, "reaction=💬", "event=$ask")
	n := 0
	for _, r := range e.reactions() {
		if r == tok+" $ask 💬" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("$ask got 💬 %d times", n)
	}
}

// A failing reaction blocks nothing: the delivery and the reply go on.
func TestReactionFailureBlocksNothing(t *testing.T) {
	e := newEnv(t)
	e.connect()
	a := e.claudeSession(100, sid1, cwd)
	ca := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	e.room.mu.Lock()
	e.room.reactErr = &matrix.Error{Status: 403, Code: matrix.ErrForbidden}
	e.room.mu.Unlock()
	ask := event("$ask", bob, e.clk.Now(), text("which port?", a))
	e.room.events["$ask"] = ask
	e.deliver(ask)
	if !strings.Contains(e.prompt(100, sid1, cwd).Text, "which port?") {
		t.Fatal("the delivery failed with the reaction")
	}
	if r := e.tool(ca, "reply", `{"thread":"$ask","text":"5432"}`); r.IsError || !strings.HasPrefix(r.Text, "Replied as ") {
		t.Fatalf("%+v", r)
	}
}

// A token the homeserver no longer knows, as after a resume's re-mint, is replaced once and the
// reaction is sent again.
func TestReactionAfterAnUnknownToken(t *testing.T) {
	e := newEnv(t)
	e.connect()
	a := e.claudeSession(100, sid1, cwd)
	e.l.mu.Lock()
	old := e.l.tokens[a]
	e.l.mu.Unlock()
	e.room.mu.Lock()
	e.room.revoked[old] = true
	e.room.mu.Unlock()
	calls := e.reg.agentCalls()
	e.deliver(event("$ask", bob, e.clk.Now(), text("which port?", a)))
	e.prompt(100, sid1, cwd)
	e.waitReactions(a + " $ask 👀")
	if e.reg.agentCalls() != calls+1 {
		t.Fatalf("%d agent calls, want %d", e.reg.agentCalls(), calls+1)
	}
}

// The loop ceiling: at most N runs per thread, then ⏸️, a status line and no run; a person's
// message in the thread starts the count again; the count is saved in state.json (spec 5.15).
func TestLoopCeiling(t *testing.T) {
	e := newEnv(t, ceilingOpt(2))
	e.setResume(wire.ResumeReplies)
	e.connect()
	dir := t.TempDir()
	uid := e.endedClaude(100, sid1, dir, "$root")
	tok := uid
	for i := 0; i < 2; i++ {
		e.deliver(event("$r"+string(rune('a'+i)), bob, e.clk.Now(), inThread(text("again"), "$root")))
		e.resumeNow()
		if e.runner.count() != i+1 {
			t.Fatalf("run %d did not start", i+1)
		}
		e.endRun(i)
	}
	e.deliver(event("$rc", bob, e.clk.Now(), inThread(text("and again"), "$root")))
	e.resumeNow()
	if e.runner.count() != 2 {
		t.Fatal("a third run started at the ceiling")
	}
	e.waitReactions(tok + " $rc ⏸️")
	if n := len(e.items(k1)); n != 3 {
		t.Fatalf("%d items held, want 3", n)
	}
	st := e.l.status(&client{pidOK: true})
	if len(st.Paused) != 1 || st.Paused[0] != (wire.PausedThread{Thread: "$root", Turns: 2}) {
		t.Fatalf("status paused %+v", st.Paused)
	}
	e.l.save()
	saved, _, err := loadState(e.dir)
	if err != nil || saved.Turns["$root"] == nil || saved.Turns["$root"].N != 2 {
		t.Fatalf("turns not saved: %+v %v", saved.Turns, err)
	}

	e.deliver(event("$rd", bob, e.clk.Now(), inThread(text("one more"), "$root")))
	e.resumeNow()
	if e.runner.count() != 2 {
		t.Fatal("ran at the ceiling")
	}
	got := e.waitReactions(tok + " $rd ⏸️")
	n := 0
	for _, r := range got {
		if r == tok+" $rc ⏸️" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("$rc got ⏸️ %d times", n)
	}

	for _, sender := range []string{bob, "@registrar:" + wiretest.ServerName} {
		e.deliver(event("$x"+sender[1:4], sender, e.clk.Now(), inThread(text("not a person"), "$root")))
	}
	if st := e.l.status(&client{pidOK: true}); len(st.Paused) != 1 {
		t.Fatal("an agent or the bot reset the count")
	}
	e.deliver(event("$alice", human, e.clk.Now(), inThread(text("stop looping"), "$root")))
	if st := e.l.status(&client{pidOK: true}); len(st.Paused) != 0 {
		t.Fatalf("a person did not reset the count: %+v", st.Paused)
	}
	e.deliver(event("$re", bob, e.clk.Now(), inThread(text("back"), "$root")))
	e.resumeNow()
	if e.runner.count() != 3 {
		t.Fatal("no run after the reset")
	}
}

// A run fed by two threads counts once for each. A thread at the ceiling gets no run: a run that
// another thread needs is not shown its messages, nor those that come in during the run; they get
// ⏸️, do not count and stay held for the owner (spec 5.15).
func TestCeilingCountsEachThread(t *testing.T) {
	e := newEnv(t, ceilingOpt(1))
	e.setResume(wire.ResumeReplies)
	e.connect()
	dir := t.TempDir()
	e.dirs[dir] = true
	uid := e.endedClaude(100, sid1, dir, "$root")
	e.procs.Kill(100)
	e.runner.onCancel = func() { e.procs.Kill(500) }
	e.deliver(event("$root2", uid, e.clk.Now(), text("second question")))
	e.deliver(event("$a1", bob, e.clk.Now(), inThread(text("alpha one"), "$root")),
		event("$a2", bob, e.clk.Now(), inThread(text("alpha two"), "$root")),
		event("$b1", bob, e.clk.Now(), inThread(text("beta one"), "$root2")))
	e.resumeNow()
	if e.runner.count() != 1 {
		t.Fatal("no run")
	}
	turnsOf := func() (int, int) {
		e.l.mu.Lock()
		defer e.l.mu.Unlock()
		return e.l.turnsLocked("$root"), e.l.turnsLocked("$root2")
	}
	if a, b := turnsOf(); a != 1 || b != 1 {
		t.Fatalf("turns %d and %d, want 1 each", a, b)
	}
	e.endRun(0)
	e.deliver(event("$a3", bob, e.clk.Now(), inThread(text("alpha three"), "$root")))
	e.resumeNow()
	if e.runner.count() != 1 {
		t.Fatal("ran with every thread at the ceiling")
	}
	e.deliver(event("$alice", human, e.clk.Now(), inThread(text("go on"), "$root2")),
		event("$b2", bob, e.clk.Now(), inThread(text("beta two"), "$root2")))
	e.resumeNow()
	if e.runner.count() != 2 {
		t.Fatal("a thread under the ceiling did not start a run")
	}
	if a, b := turnsOf(); a != 1 || b != 1 {
		t.Fatalf("turns %d and %d, want 1 and 1: only the thread under the ceiling counts", a, b)
	}

	e.runTree(500)
	r := e.start(510, "claude", sid1, "resume", dir)
	if !strings.Contains(r.Text, "beta one") || !strings.Contains(r.Text, "beta two") || strings.Contains(r.Text, "alpha") {
		t.Fatalf("the run's SessionStart printed %q", r.Text)
	}
	e.waitReactions(uid+" $a1 ⏸️", uid+" $a2 ⏸️", uid+" $a3 ⏸️", uid+" $b1 ⏳", uid+" $b2 ⏳")
	e.noReaction(uid + " $a1 ⏳")
	e.deliver(event("$a4", bob, e.clk.Now(), inThread(text("alpha four"), "$root")))
	if r := e.hook(510, hookReq("claude", wire.EventPostTool, sid1, dir)); strings.Contains(r.Text, "alpha") {
		t.Fatalf("the run was shown a message that came in at the ceiling: %q", r.Text)
	}
	e.waitReactions(uid + " $a4 ⏸️")
	if n := len(e.items(k1)); n != 4 {
		t.Fatalf("%d items held, want 4", n)
	}

	e.claudeTree(300)
	e.child(proc.ID{PID: 300, Start: 300}, 310)
	r = e.start(310, "claude", sid1, "resume", dir)
	for _, want := range []string{"alpha one", "alpha two", "alpha three", "alpha four"} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("the owner did not get %q: %q", want, r.Text)
		}
	}
	if a, _ := turnsOf(); a != 1 {
		t.Fatalf("the thread at the ceiling counted: %d", a)
	}
}

// A run that is still going, its SessionStart not come yet, holds the session's next run back: a
// new message of its thread gets no ⏸️ and the resume is tried again (spec 5.11, 5.15).
func TestNoPauseWhileItsRunGoes(t *testing.T) {
	e := newEnv(t, ceilingOpt(1))
	e.setResume(wire.ResumeReplies)
	e.connect()
	uid := e.endedClaude(100, sid1, t.TempDir(), "$root")
	e.deliver(event("$a1", bob, e.clk.Now(), inThread(text("x"), "$root")))
	e.resumeNow()
	if e.runner.count() != 1 {
		t.Fatal("no run")
	}
	e.deliver(event("$a2", bob, e.clk.Now(), inThread(text("y"), "$root")))
	e.resumeNow()
	e.noReaction(uid + " $a2 ⏸️")
	e.noReaction(uid + " $a1 ⏸️")
	if s := e.session(k1); s.ResumeDue == nil {
		t.Fatal("the resume is not tried again")
	}
	e.endRun(0)
}

// After a person resets a thread's count, a session that holds messages of that thread is tried
// again, even when the person's message does not reach it (spec 5.15).
func TestResetWakesAPausedSession(t *testing.T) {
	e := newEnv(t, ceilingOpt(2))
	e.setResume(wire.ResumeReplies, wire.ResumeMentions)
	e.connect()
	e.endedClaude(100, sid1, t.TempDir(), "$root")
	uid2 := e.endedClaude(200, sid2, t.TempDir(), "$root2")
	e.l.mu.Lock()
	e.l.st.Turns["$root"] = &turns{N: 2, Last: e.clk.Now()}
	e.l.mu.Unlock()
	e.deliver(event("$m", bob, e.clk.Now(), inThread(text("over to you", uid2), "$root")))
	if len(e.items(k2)) != 1 {
		t.Fatal("the mention did not reach the second session")
	}
	e.resumeNow()
	if e.runner.count() != 0 {
		t.Fatal("ran at the ceiling")
	}
	e.deliver(event("$p", human, e.clk.Now(), inThread(text("go on"), "$root")))
	if len(e.items(k2)) != 1 {
		t.Fatal("the person's message reached the second session")
	}
	e.resumeNow()
	if e.runner.count() != 2 {
		t.Fatalf("%d runs after the reset, want 2", e.runner.count())
	}
}

// The closed agent of an ended session is not reopened just to react.
func TestPausedReactionNeedsAnOpenAgent(t *testing.T) {
	e := newEnv(t, ceilingOpt(1))
	e.setResume(wire.ResumeReplies)
	e.connect()
	dir := t.TempDir()
	e.endedClaude(100, sid1, dir, "$root")
	e.deliver(event("$a1", bob, e.clk.Now(), inThread(text("x"), "$root")))
	e.resumeNow()
	e.endRun(0)
	e.clk.Advance(closeGrace)
	e.l.deadlines()
	eventually(t, "the close", func() bool { return e.session(k1).AgentClosed })
	calls := e.reg.agentCalls()
	e.deliver(event("$a2", bob, e.clk.Now(), inThread(text("y"), "$root")))
	e.resumeNow()
	time.Sleep(20 * time.Millisecond)
	if len(e.reactions()) != 0 || e.reg.agentCalls() != calls || e.runner.count() != 1 {
		t.Fatalf("reactions %v, agent calls %d (was %d), runs %d", e.reactions(), e.reg.agentCalls(), calls, e.runner.count())
	}
}

// "You win": the run's own SessionStart is served as before and gets ⏳; the owner's SessionStart
// stops the run first, then gets the messages the run had not printed and one notice naming the
// threads it had printed (spec 5.11).
func TestYouWinStopsTheRun(t *testing.T) {
	e := resumeEnv(t, wire.ResumeReplies)
	dir := t.TempDir()
	e.dirs[dir] = true
	uid := e.endedClaude(100, sid1, dir, "$root")
	e.procs.Kill(100)
	e.runner.onCancel = func() { e.procs.Kill(500) }
	e.deliver(event("$a", bob, e.clk.Now(), inThread(text("first answer"), "$root")))
	e.resumeNow()
	if e.runner.count() != 1 {
		t.Fatal("no run")
	}
	e.runTree(500)
	if r := e.start(510, "claude", sid1, "resume", dir); !strings.Contains(r.Text, "first answer") {
		t.Fatalf("the run's SessionStart printed %q", r.Text)
	}
	e.waitReactions(uid+" $a ⏳", uid+" $a 👀")
	if st := e.l.status(&client{pidOK: true}); st.ResumeActive != 1 {
		t.Fatal("the run's own SessionStart stopped it")
	}

	e.deliver(event("$b", bob, e.clk.Now(), inThread(text("second answer"), "$root")))
	e.claudeTree(300)
	e.child(proc.ID{PID: 300, Start: 300}, 310)
	r := e.start(310, "claude", sid1, "resume", dir)
	if st := e.l.status(&client{pidOK: true}); st.ResumeActive != 0 {
		t.Fatal("the run still runs after the owner's SessionStart")
	}
	if !strings.Contains(r.Text, "second answer") || strings.Contains(r.Text, "first answer") {
		t.Fatalf("the owner's SessionStart printed %q", r.Text)
	}
	if !strings.Contains(r.Text, "kwak stopped this session's background run when you opened it") ||
		!strings.Contains(r.Text, "(thread $root). Read it with the kwak thread tool.") {
		t.Fatalf("no notice naming the printed thread: %q", r.Text)
	}
	if s := e.session(k1); s.Ended != nil || !containsID(s.Candidates, proc.ID{PID: 300, Start: 300}) {
		t.Fatalf("the owner's session is not linked: %+v", s)
	}
}

// A run that does not stop within the bound leaves the SessionStart refused, as before, and the
// hook still gets its answer.
func TestYouWinGivesUpAfterTheBound(t *testing.T) {
	e := resumeEnv(t, wire.ResumeReplies)
	e.runner.stubborn = true
	dir := t.TempDir()
	e.dirs[dir] = true
	e.endedClaude(100, sid1, dir, "$root")
	e.procs.Kill(100)
	e.deliver(event("$a", bob, e.clk.Now(), inThread(text("first answer"), "$root")))
	e.resumeNow()
	e.runTree(500)
	e.start(510, "claude", sid1, "resume", dir)
	e.claudeTree(300)
	e.child(proc.ID{PID: 300, Start: 300}, 310)
	done := make(chan wire.HookResponse, 1)
	timers := e.clk.waiting() // the run's 15 minute limit is one
	go func() { done <- e.start(310, "claude", sid1, "resume", dir) }()
	eventually(t, "the stop to wait", func() bool { return e.clk.waiting() > timers })
	e.clk.Advance(stopWait)
	select {
	case r := <-done:
		if r.Text != "" || !r.OK {
			t.Fatalf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the SessionStart never answered")
	}
	if s := e.session(k1); containsID(s.Candidates, proc.ID{PID: 300, Start: 300}) {
		t.Fatal("the session moved while its run still ran")
	}
	e.endRun(0)
}

// "You win" before the run printed anything: the owner gets the waiting messages and no notice.
func TestYouWinBeforeTheRunPrinted(t *testing.T) {
	e := resumeEnv(t, wire.ResumeReplies)
	dir := t.TempDir()
	e.dirs[dir] = true
	e.endedClaude(100, sid1, dir, "$root")
	e.procs.Kill(100)
	e.deliver(event("$a", bob, e.clk.Now(), inThread(text("first answer"), "$root")))
	e.resumeNow()
	if e.runner.count() != 1 {
		t.Fatal("no run")
	}
	e.claudeTree(300)
	e.child(proc.ID{PID: 300, Start: 300}, 310)
	r := e.start(310, "claude", sid1, "resume", dir)
	if !strings.Contains(r.Text, "first answer") || strings.Contains(r.Text, "kwak stopped") {
		t.Fatalf("the owner's SessionStart printed %q", r.Text)
	}
	if st := e.l.status(&client{pidOK: true}); st.ResumeActive != 0 {
		t.Fatal("the run still runs")
	}
}

// While the owner's SessionStart stops a run, no new run starts for that session, also once the
// stopped run has left and before the hook has linked the session (spec 5.11).
func TestYouWinStartsNoRunWhileOpening(t *testing.T) {
	e := resumeEnv(t, wire.ResumeReplies)
	dir := t.TempDir()
	e.dirs[dir] = true
	e.endedClaude(100, sid1, dir, "$root")
	e.procs.Kill(100)
	e.deliver(event("$a", bob, e.clk.Now(), inThread(text("first answer"), "$root")))
	e.resumeNow()
	e.deliver(event("$b", bob, e.clk.Now(), inThread(text("second answer"), "$root")))
	e.claudeTree(300)
	e.child(proc.ID{PID: 300, Start: 300}, 310)
	e.l.stopRunFor(k1, e.chainOf(310))
	e.resumeNow()
	if e.runner.count() != 1 {
		t.Fatal("a new run started while the owner opened the session")
	}
	r := e.start(310, "claude", sid1, "resume", dir)
	if !strings.Contains(r.Text, "second answer") {
		t.Fatalf("the owner's SessionStart printed %q", r.Text)
	}
	e.resumeNow()
	e.l.mu.Lock()
	opening := len(e.l.opening)
	e.l.mu.Unlock()
	if e.runner.count() != 1 || opening != 0 {
		t.Fatalf("runs %d, opening %d", e.runner.count(), opening)
	}
}

// A listener shutdown waits for its runs to exit, so the runner can kill a run's group (spec 5.11).
func TestShutdownWaitsForRuns(t *testing.T) {
	e := resumeEnv(t, wire.ResumeReplies)
	e.runner.stubborn = true
	e.endedClaude(100, sid1, t.TempDir(), "$root")
	e.deliver(event("$a", bob, e.clk.Now(), inThread(text("x"), "$root")))
	e.resumeNow()
	if e.runner.count() != 1 {
		t.Fatal("no run")
	}
	e.l.stop()
	done := make(chan struct{})
	go func() {
		e.l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("shutdown did not wait for the run")
	case <-time.After(50 * time.Millisecond):
	}
	e.runner.mu.Lock()
	e.runner.runs[0].done <- nil
	e.runner.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown never ended")
	}
}

// A message of another room changes nothing, even when handed to applyLocked directly: no item,
// no answered question, no loop count reset (spec 5.3, 5.15).
func TestOtherRoomChangesNothing(t *testing.T) {
	e := resumeEnv(t, wire.ResumeReplies, wire.ResumeMentions)
	uid := e.endedClaude(100, sid1, t.TempDir(), "$root")
	e.l.mu.Lock()
	e.l.st.Questions["$root"] = &question{Asker: uid, Session: k1, AskedAt: e.clk.Now(), Due: e.clk.Now().Add(time.Hour)}
	e.l.st.Turns["$root"] = &turns{N: 5, Last: e.clk.Now()}
	e.l.mu.Unlock()
	ev := event("$elsewhere", human, e.clk.Now(), inThread(text("answer", uid), "$root"))
	ev.RoomID = "!other:" + wiretest.ServerName
	e.l.mu.Lock()
	e.l.applyLocked(ev, []Delivery{{UserID: uid, Session: k1, Reason: render.Mention}})
	q, n := e.l.st.Questions["$root"], e.l.turnsLocked("$root")
	e.l.mu.Unlock()
	if len(e.items(k1)) != 0 || q == nil || n != 5 {
		t.Fatalf("items %d, question kept %v, turns %d", len(e.items(k1)), q != nil, n)
	}
}

// A finished helper's message is printed to the session's agent, which reacts to it and gets 💬
// on it when it replies in its thread; the helper is not minted to react (spec 5.10, 5.14).
func TestFinishedHelperMessageAnsweredByTheSession(t *testing.T) {
	e := newEnv(t)
	e.connect()
	a := e.claudeSession(100, sid1, cwd)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	if r := e.tool(c, "helper", `{"name":"tests"}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	hk := hookReq("claude", wire.EventPostTool, sid1, cwd)
	hk.AgentID, hk.ToolName, hk.ToolInput = "a7f3", "mcp__plugin_kwak_kwak__helper", json.RawMessage(`{"name":"tests"}`)
	e.hook(110, hk)
	ask := event("$h1", bob, e.clk.Now(), text("for the helper", wiretest.HelperAgent))
	e.room.events["$h1"] = ask
	e.deliver(ask)
	stop := hookReq("claude", wire.EventSubagentStop, sid1, cwd)
	stop.AgentID = "a7f3"
	e.hook(110, stop)
	if r := e.prompt(100, sid1, cwd); !strings.Contains(r.Text, "for the helper") {
		t.Fatalf("hand-over: %q", r.Text)
	}
	e.waitReactions(a + " $h1 👀")
	if r := e.tool(c, "reply", `{"thread":"$h1","text":"done"}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	e.waitReactions(a + " $h1 💬")
	e.noReaction(wiretest.HelperAgent + " $h1 👀")
}

// A loop count is kept 90 days after the thread's last automatic turn, like the thread itself.
func TestTurnsPruned(t *testing.T) {
	e := newEnv(t)
	e.l.mu.Lock()
	e.l.st.Turns["$old"] = &turns{N: 3, Last: e.clk.Now()}
	e.l.st.Turns["$new"] = &turns{N: 3, Last: e.clk.Now().Add(keepEnded)}
	e.l.mu.Unlock()
	e.clk.Advance(keepEnded + time.Minute)
	e.l.liveness()
	e.l.mu.Lock()
	defer e.l.mu.Unlock()
	if e.l.st.Turns["$old"] != nil || e.l.st.Turns["$new"] == nil {
		t.Fatalf("turns %+v", e.l.st.Turns)
	}
}

// Shutdown waits for a reaction that is being sent, as it waits for runs (spec 5.14).
func TestShutdownWaitsForReactions(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	gate := make(chan struct{})
	e.room.mu.Lock()
	e.room.reactGate = gate
	e.room.mu.Unlock()
	e.l.react([]reaction{{k1, "", "$ask", reactShown}})
	eventually(t, "the reaction to be on its way", func() bool {
		e.room.mu.Lock()
		defer e.room.mu.Unlock()
		return e.room.reactHeld == 1
	})
	e.l.stop()
	done := make(chan struct{})
	go func() {
		e.l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("shutdown did not wait for the reaction")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown never ended")
	}
}
