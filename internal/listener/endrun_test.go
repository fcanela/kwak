package listener

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

// reply with dont_know posts nothing, runs no check and marks 🤷‍♂️ on the messages of that thread a
// 💬 would mark; they then wait for nothing more, so a later reply gives them no 💬 (decision 45).
func TestDontKnowMarksAndPostsNothing(t *testing.T) {
	e := newEnv(t)
	e.setChecks(checkOut, "")
	e.check.exit = 1
	e.connect()
	a := e.claudeSession(100, sid1, cwd)
	ca := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	ask := event("$ask", bob, e.clk.Now(), text("which port?", a))
	other := event("$other", bob, e.clk.Now(), text("and the host?", a))
	e.room.events["$ask"], e.room.events["$other"] = ask, other
	e.deliver(ask, other)
	e.prompt(100, sid1, cwd)
	e.waitReactions(a+" $ask 👀", a+" $other 👀")

	for args, want := range map[string]string{
		`{"thread":"$ask"}`: "reply needs 'text' with your answer, or dont_know true when you do not know it",
		`{"thread":"$ask","text":"5432","dont_know":true}`:      "give either 'text' or dont_know true, not both",
		`{"thread":"$ask","dont_know":true,"to":["agent.x.y"]}`: "'to' cannot go with dont_know: nothing is posted, so nobody is addressed",
		`{"thread":"$ask","dont_know":true,"as":"nobody"}`:      "you have no helper named nobody; create it with the helper tool",
	} {
		if r := e.tool(ca, "reply", args); !r.IsError || r.Text != want {
			t.Errorf("%s: %+v", args, r)
		}
	}
	r := e.tool(ca, "reply", `{"thread":"$ask","dont_know":true}`)
	if r.IsError || r.Text != "Nothing was posted in thread $ask. kwak added 🤷‍♂️ to the message of it you were shown since your last post there.\n" {
		t.Fatalf("%+v", r)
	}
	e.waitReactions(a + " $ask 🤷‍♂️")
	e.noReaction(a + " $other 🤷‍♂️")
	if n := len(e.room.sentContents()); n != 0 || len(e.check.calls()) != 0 {
		t.Fatalf("dont_know posted %d messages and ran %d checks", n, len(e.check.calls()))
	}
	if r := e.tool(ca, "reply", `{"thread":"$ask","dont_know":true}`); r.IsError || !strings.Contains(r.Text, "so kwak added no 🤷‍♂️") {
		t.Fatalf("a second dont_know: %+v", r)
	}
	e.check.exit = 0
	if r := e.tool(ca, "reply", `{"thread":"$ask","text":"5432","dont_know":false}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	e.noReaction(a + " $ask 💬")
}

// endedWithThreads is an ended session whose agent started $root1, $root2 and $root3, each known
// to the room so reply can read it, with a background run going for one reply in each and for a
// root that mentions the agent. It returns the agent and the run's tools client.
func (e *env) endedWithThreads(dir string) (string, *client) {
	e.t.Helper()
	e.setResume(wire.ResumeReplies)
	e.dirs[dir] = true
	uid := e.endedClaude(100, sid1, dir, "$root1")
	e.procs.Kill(100)
	e.runner.onCancel = func() { e.procs.Kill(500) }
	for _, id := range []string{"$root2", "$root3"} {
		e.deliver(event(id, uid, e.clk.Now(), text("my question")))
	}
	for _, id := range []string{"$root1", "$root2", "$root3"} {
		e.room.events[id] = event(id, uid, e.clk.Now(), text("my question"))
	}
	e.deliver(event("$a", bob, e.clk.Now(), inThread(text("answer one"), "$root1")),
		event("$b", bob, e.clk.Now(), inThread(text("answer two"), "$root2")),
		event("$c", bob, e.clk.Now(), inThread(text("answer three"), "$root3")),
		event("$m", bob, e.clk.Now(), text("a new question for you", uid)))
	e.resumeNow()
	if e.runner.count() != 1 {
		e.t.Fatal("no run")
	}
	e.runTree(500)
	if r := e.start(510, "claude", sid1, "resume", dir); !strings.Contains(r.Text, "answer three") {
		e.t.Fatalf("the run's SessionStart printed %q", r.Text)
	}
	e.waitReactions(uid+" $a ⏳", uid+" $c ⏳", uid+" $m ⏳")
	return uid, e.mcp(e.child(proc.ID{PID: 500, Start: 500}, 520).PID, "claude", "")
}

// A clean end of a background run gives 👍 on each reply in the session's own threads that the
// run printed and neither answered nor marked; never on a root, never ⚠️ (decision 45).
func TestCleanRunEndGivesThumbsUp(t *testing.T) {
	e := newEnv(t)
	e.connect()
	uid, c := e.endedWithThreads(t.TempDir())
	if r := e.tool(c, "reply", `{"thread":"$root1","text":"thanks"}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	if r := e.tool(c, "reply", `{"thread":"$root2","dont_know":true}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	e.waitReactions(uid+" $a 💬", uid+" $b 🤷‍♂️")
	e.endRun(0)
	e.waitReactions(uid + " $c 👍")
	for _, no := range []string{"$a 👍", "$b 👍", "$m 👍", "$a ⚠️", "$c ⚠️", "$m ⚠️"} {
		e.noReaction(uid + " " + no)
	}
}

// A run that does not end cleanly gives ⚠️ on each message that got ⏳ from it and that it neither
// answered nor marked, and no 👍: a non-zero exit, and an exit 0 after a stop (decision 45).
func TestUncleanRunEndGivesWarning(t *testing.T) {
	for name, end := range map[string]func(e *env){
		"non-zero exit": func(e *env) { e.runner.runs[0].done <- errors.New("exit status 1") },
		"exit 0 after a stop": func(e *env) {
			e.l.mu.Lock()
			e.l.running[k1].cancel()
			e.l.mu.Unlock()
			e.runner.runs[0].done <- nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.connect()
			e.runner.stubborn = true
			uid, c := e.endedWithThreads(t.TempDir())
			if r := e.tool(c, "reply", `{"thread":"$root1","text":"thanks"}`); r.IsError {
				t.Fatalf("%+v", r)
			}
			e.deliver(event("$late", bob, e.clk.Now(), inThread(text("late answer"), "$root3")))
			e.hook(510, hookReq("claude", wire.EventPostTool, sid1, e.session(k1).Cwd))
			e.waitReactions(uid+" $a 💬", uid+" $late 👀")
			e.runner.mu.Lock()
			end(e)
			e.runner.mu.Unlock()
			e.waitReactions(uid+" $b ⚠️", uid+" $c ⚠️", uid+" $m ⚠️")
			for _, no := range []string{"$a ⚠️", "$late ⚠️", "$b 👍", "$c 👍", "$late 👍"} {
				e.noReaction(uid + " " + no)
			}
		})
	}
}

// "You win" stops the run, so its messages get ⚠️ before the owner's session takes over; a run
// stopped before its own SessionStart gave no ⏳, so it gives no ⚠️ either (decision 45).
func TestYouWinGivesWarning(t *testing.T) {
	e := newEnv(t)
	e.connect()
	dir := t.TempDir()
	uid, _ := e.endedWithThreads(dir)
	e.claudeTree(300)
	e.child(proc.ID{PID: 300, Start: 300}, 310)
	e.start(310, "claude", sid1, "resume", dir)
	e.waitReactions(uid+" $a ⚠️", uid+" $c ⚠️")
	e.noReaction(uid + " $c 👍")

	e2 := resumeEnv(t, wire.ResumeReplies)
	dir2 := t.TempDir()
	e2.dirs[dir2] = true
	uid2 := e2.endedClaude(100, sid1, dir2, "$root")
	e2.procs.Kill(100)
	e2.deliver(event("$x", bob, e2.clk.Now(), inThread(text("first answer"), "$root")))
	e2.resumeNow()
	e2.claudeTree(300)
	e2.child(proc.ID{PID: 300, Start: 300}, 310)
	e2.start(310, "claude", sid1, "resume", dir2)
	e2.noReaction(uid2 + " $x ⚠️")
}

// The listener stopping a run is not a clean end: its messages get ⚠️, sent even though the
// listener is stopping (decision 45).
func TestShutdownGivesWarning(t *testing.T) {
	e := newEnv(t)
	e.connect()
	uid, _ := e.endedWithThreads(t.TempDir())
	e.l.stop()
	e.l.wg.Wait()
	for _, r := range []string{"$a ⚠️", "$c ⚠️"} {
		if !containsString(e.reactions(), uid+" "+r) {
			t.Fatalf("no %s after the shutdown: %v", r, e.reactions())
		}
	}
}

// 👍 goes only on replies in a thread the session started: a reply in someone else's thread that
// the run printed and did not answer keeps 👀 and ⏳ at a clean end (decision 45, option a).
func TestReplyInOtherThreadGetsNoThumbsUp(t *testing.T) {
	e := newEnv(t)
	e.connect()
	uid, _ := e.endedWithThreads(t.TempDir())
	e.deliver(event("$mr", bob, e.clk.Now(), inThread(text("more on it", uid), "$m")))
	e.hook(510, hookReq("claude", wire.EventPostTool, sid1, e.session(k1).Cwd))
	e.waitReactions(uid + " $mr 👀")
	e.endRun(0)
	e.waitReactions(uid + " $c 👍")
	e.noReaction(uid + " $mr 👍")
}

// dont_know with 'as' marks the messages printed to that helper, and the helper sends the 🤷‍♂️, as
// it would send the 💬 of its reply (6.4, Q50).
func TestDontKnowAsHelperMarksItsMessages(t *testing.T) {
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
	sub := hookReq("claude", wire.EventPostTool, sid1, cwd)
	sub.AgentID = "a7f3"
	if r := e.hook(110, sub); !strings.Contains(r.Text, "for the helper") {
		t.Fatalf("helper drain: %q", r.Text)
	}
	e.waitReactions(wiretest.HelperAgent + " $h1 👀")
	r := e.tool(c, "reply", `{"thread":"$h1","dont_know":true,"as":"tests"}`)
	if r.IsError || !strings.Contains(r.Text, "kwak added 🤷‍♂️ to the message") {
		t.Fatalf("%+v", r)
	}
	e.waitReactions(wiretest.HelperAgent + " $h1 🤷‍♂️")
	e.noReaction(a + " $h1 🤷‍♂️")
}

// The 15 minute limit stops a run, and an exit 0 after it is not a clean end: ⚠️ on the messages
// that got ⏳ and were neither answered nor marked, no 👍 (5.11, decision 45). The limit runs on the
// listener's clock.
func TestTimedOutRunGivesWarning(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.runner.stubborn = true
	uid, _ := e.endedWithThreads(t.TempDir())
	e.clk.Advance(wire.DefaultRunTimeout)
	e.runner.mu.Lock()
	ctx := e.runner.runs[0].ctx
	e.runner.mu.Unlock()
	eventually(t, "the 15 minute limit to stop the run", func() bool { return ctx.Err() != nil })
	e.endRun(0)
	e.waitReactions(uid+" $a ⚠️", uid+" $b ⚠️", uid+" $c ⚠️", uid+" $m ⚠️")
	e.noReaction(uid + " $c 👍")
}

// A reply that got 👍 is done: a later 💬 or 🤷‍♂️ in its thread does not mark it again, so an
// answer never gets 🤷‍♂️ after its 👍 (decision 45).
func TestThumbsUpEndsTheWait(t *testing.T) {
	e := newEnv(t)
	e.connect()
	dir := t.TempDir()
	uid, _ := e.endedWithThreads(dir)
	// As live: the run's SessionEnd arrives before its process exits.
	e.hook(510, hookReq("claude", wire.EventSessionEnd, sid1, dir))
	e.procs.Kill(500)
	e.endRun(0)
	e.waitReactions(uid + " $c 👍")

	e.deliver(event("$q", bob, e.clk.Now(), inThread(text("and the host?"), "$root3")))
	e.claudeTree(300)
	e.child(proc.ID{PID: 300, Start: 300}, 310)
	e.start(310, "claude", sid1, "resume", dir)
	e.prompt(300, sid1, dir)
	e.waitReactions(uid + " $q 👀")
	c := e.mcp(e.child(proc.ID{PID: 300, Start: 300}, 320).PID, "claude", "")
	r := e.tool(c, "reply", `{"thread":"$root3","dont_know":true}`)
	if r.IsError || !strings.Contains(r.Text, "kwak added 🤷‍♂️ to the message of it") {
		t.Fatalf("%+v", r)
	}
	e.waitReactions(uid + " $q 🤷‍♂️")
	e.noReaction(uid + " $c 🤷‍♂️")
}
