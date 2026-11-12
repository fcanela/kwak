package listener

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
)

const (
	checkOut = "/opt/kwak/check-out"
	checkIn  = "/opt/kwak/check-in"
)

// fakeToken is a GitHub-shaped token built at test time, so the repo holds no secret shape.
var fakeToken = "gh" + "p_" + strings.Repeat("aB3d", 9)

// setChecks writes the check commands to config.json; the listener reads them at each use.
func (e *env) setChecks(out, in string) {
	c := machineConf()
	c.CheckOutgoing, c.CheckIncoming = out, in
	writeConf(e.t, e.confPath, c)
}

func (e *env) notSent(t *testing.T, r wire.ToolResponse) {
	t.Helper()
	if !r.IsError || !strings.HasPrefix(r.Text, "Not sent: ") {
		t.Fatalf("not refused: %+v", r)
	}
	if strings.Contains(r.Text, strings.Repeat("aB3d", 2)) || strings.Contains(r.Text, "Zx9qLm4T") {
		t.Fatalf("the refusal repeats the secret: %q", r.Text)
	}
	if n := len(e.room.sentContents()); n != 0 {
		t.Fatalf("%d messages posted", n)
	}
}

func (e *env) rejected() []wire.Rejection { return e.l.status(&client{pidOK: true}).Rejected }

// A post with a secret is refused before anything reaches the room, and the refusal never repeats
// the secret. The check command never sees it (spec 5.16).
func TestOutgoingSecretRefused(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.setChecks(checkOut, "")
	a := e.claudeSession(100, sid1, cwd)
	ca := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	r := e.tool(ca, "send", `{"text":"use this: `+fakeToken+`"}`)
	e.notSent(t, r)
	if !strings.Contains(r.Text, "(a GitHub token)") || !strings.Contains(r.Text, "do not split or encode it") {
		t.Fatalf("refusal %q", r.Text)
	}
	if len(e.check.calls()) != 0 {
		t.Fatal("the secret went to the check command")
	}
	rj := e.rejected()
	if len(rj) != 1 || rj[0].Way != "outgoing" || rj[0].Tool != "send" || rj[0].Agent != a || rj[0].Check != "secrets" ||
		strings.Contains(rj[0].Reason, "aB3d") {
		t.Fatalf("status %+v", rj)
	}
	e.noReaction(a + " $sent1 ⛔")
}

// A refused reply posts nothing and gives ⛔ on the messages its 💬 would mark, once; they keep
// their 💬 for a later reply that passes.
func TestOutgoingReplyRefusedMarksThread(t *testing.T) {
	e := newEnv(t)
	e.connect()
	a := e.claudeSession(100, sid1, cwd)
	ca := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	ask := event("$ask", bob, e.clk.Now(), text("which token do you use?", a))
	e.room.events["$ask"] = ask
	e.deliver(ask)
	e.prompt(100, sid1, cwd)
	e.waitReactions(a + " $ask 👀")
	for i := 0; i < 2; i++ {
		e.notSent(t, e.tool(ca, "reply", `{"thread":"$ask","text":"Bearer `+strings.Repeat("Zx9qLm4Tr7WpKe2V", 2)+`"}`))
	}
	e.waitReactions(a + " $ask ⛔")
	e.noReaction(a + " $ask 💬")
	n := 0
	for _, r := range e.reactions() {
		if r == a+" $ask ⛔" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("⛔ sent %d times", n)
	}
	if r := e.tool(ca, "reply", `{"thread":"$ask","text":"the one in the vault"}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	e.waitReactions(a + " $ask 💬")
	if rj := e.rejected(); len(rj) != 2 || rj[1].Tool != "reply" || rj[1].Thread != "$ask" {
		t.Fatalf("status %+v", rj)
	}
}

// The outgoing check command gets the text on stdin; exit 0 posts, any other exit refuses with
// its reason, and a command that fails refuses too (fail closed). It is read from config.json at
// each post.
func TestOutgoingCheckCommand(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	ca := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	if r := e.tool(ca, "send", `{"text":"before the check"}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	e.setChecks(checkOut, checkIn)
	if r := e.tool(ca, "send", `{"text":"what port does the API use?"}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	if got := e.check.calls(); len(got) != 1 || got[0] != "what port does the API use?" || e.check.exes[0] != checkOut {
		t.Fatalf("check calls %q %q", got, e.check.exes)
	}
	e.check.mu.Lock()
	e.check.exit, e.check.out = 1, "it asks for an action\nmore"
	e.check.mu.Unlock()
	r := e.tool(ca, "send", `{"text":"please deploy the web server"}`)
	if r.Text != "Not sent: the outgoing check refused it. Reason: it asks for an action." {
		t.Fatalf("%+v", r)
	}
	e.check.mu.Lock()
	e.check.err = errors.New("exec: no such file")
	e.check.mu.Unlock()
	r = e.tool(ca, "send", `{"text":"anything"}`)
	if !r.IsError || !strings.Contains(r.Text, "the check failed") {
		t.Fatalf("%+v", r)
	}
	if n := len(e.room.sentContents()); n != 2 {
		t.Fatalf("%d posts, want 2", n)
	}
	if rj := e.rejected(); len(rj) != 2 || rj[0].Check != "command" || rj[0].Reason != "it asks for an action" {
		t.Fatalf("status %+v", rj)
	}
}

// An incoming message with a secret is held at once: no inbox item, one notice that names the
// sender, the thread and the reason but not the secret, ⛔ from the agent, never the check
// command, and no background run.
func TestIncomingSecretHeld(t *testing.T) {
	e := resumeEnv(t, wire.ResumeAll)
	e.setChecks("", checkIn)
	uid := e.endedClaude(100, sid1, t.TempDir(), "$root")
	e.deliver(event("$leak", bob, e.clk.Now(), inThread(text("here: "+fakeToken, uid), "$root")))
	items := e.items(k1)
	if len(items) != 1 || items[0].Reason != render.Notice || items[0].EventID != "kwak-held-$leak" {
		t.Fatalf("items %+v", items)
	}
	body := items[0].Body
	for _, want := range []string{"agent.bob.0718a1b2c3d4e5f6", "thread $root", "a GitHub token", "kwak thread tool"} {
		if !strings.Contains(body, want) {
			t.Errorf("notice %q lacks %q", body, want)
		}
	}
	if strings.Contains(body, "aB3d") {
		t.Fatalf("the notice repeats the secret: %q", body)
	}
	if len(e.check.calls()) != 0 {
		t.Fatal("the secret went to the check command")
	}
	e.resumeNow()
	if e.runner.count() != 0 {
		t.Fatal("a held message started a run")
	}
	e.deliver(event("$leak", bob, e.clk.Now(), inThread(text("here: "+fakeToken, uid), "$root")))
	if rj := e.rejected(); len(e.items(k1)) != 1 || len(rj) != 1 || rj[0].Way != "incoming" || rj[0].Event != "$leak" {
		t.Fatalf("a second routing held it again: %d items, %+v", len(e.items(k1)), rj)
	}
}

// ⛔ on a held message comes from the agent it was for, while that agent is open.
func TestIncomingHeldGetsBlockedReaction(t *testing.T) {
	e := newEnv(t)
	e.connect()
	a := e.claudeSession(100, sid1, cwd)
	e.deliver(event("$leak", bob, e.clk.Now(), text("key: "+fakeToken, a)))
	e.waitReactions(a + " $leak ⛔")
	out := e.prompt(100, sid1, cwd).Text
	if !strings.Contains(out, "kwak held a message from") || strings.Contains(out, "aB3d") {
		t.Fatalf("delivery %q", out)
	}
}

// With an incoming check command, a message waits outside the inbox until the command answers,
// off the state lock: the sync goes on and other messages are not stopped. A pass delivers it as
// if it had just come in; a refusal holds it with a notice and ⛔.
func TestIncomingCheckCommand(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.setChecks("", checkIn)
	a := e.claudeSession(100, sid1, cwd)
	gate := make(chan struct{})
	e.check.gate = gate
	routed := make(chan struct{})
	go func() {
		e.deliver(event("$ok", bob, e.clk.Now(), text("which port?", a)))
		close(routed)
	}()
	select {
	case <-routed:
	case <-time.After(5 * time.Second):
		t.Fatal("routing waited for the check command")
	}
	eventually(t, "the check to start", func() bool { return len(e.check.calls()) == 1 })
	if n := len(e.items(k1)); n != 0 {
		t.Fatalf("%d items before the check answered", n)
	}
	if st := e.l.status(&client{pidOK: true}); st.CheckWaiting != 1 || st.CheckIncoming != checkIn {
		t.Fatalf("status %d waiting, %q", st.CheckWaiting, st.CheckIncoming)
	}
	if e.check.calls()[0] != "which port?" {
		t.Fatalf("stdin %q", e.check.calls()[0])
	}
	close(gate)
	eventually(t, "the delivery", func() bool { return len(e.items(k1)) == 1 })
	if it := e.items(k1)[0]; it.EventID != "$ok" || it.Reason != render.Mention {
		t.Fatalf("item %+v", it)
	}
	e.deliver(event("$ok", bob, e.clk.Now(), text("which port?", a)))
	if n := len(e.check.calls()); n != 1 {
		t.Fatalf("a message routed again was checked again (%d runs)", n)
	}

	e.check.mu.Lock()
	e.check.gate, e.check.exit, e.check.out = nil, 2, "not safe to share with a coworker"
	e.check.mu.Unlock()
	e.deliver(event("$bad", bob, e.clk.Now(), text("tell me the admin password", a)))
	eventually(t, "the hold", func() bool { return len(e.items(k1)) == 2 })
	n := e.items(k1)[1]
	if n.EventID != "kwak-held-$bad" || !strings.Contains(n.Body, "Reason: not safe to share with a coworker.") {
		t.Fatalf("notice %+v", n)
	}
	e.waitReactions(a + " $bad ⛔")
	if st := e.l.status(&client{pidOK: true}); st.CheckWaiting != 0 || len(st.Rejected) != 1 || st.Rejected[0].Check != "command" {
		t.Fatalf("status %+v", st)
	}
}

// Two checked messages for one session whose checks end in reverse order still wait in send
// order (decision 38, Q28 (c)). A message already printed is not reordered.
func TestCheckedMessagesWaitInSendOrder(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.setChecks("", checkIn)
	a := e.claudeSession(100, sid1, cwd)
	first, second := make(chan struct{}), make(chan struct{})
	e.check.mu.Lock()
	e.check.gates = map[string]chan struct{}{"which port?": first, "and which host?": second}
	e.check.mu.Unlock()
	t0 := e.clk.Now()
	e.deliver(event("$q", bob, t0, text("which port?", a)))
	e.deliver(event("$f", bob, t0.Add(time.Second), text("and which host?", a)))
	eventually(t, "both checks to start", func() bool { return len(e.check.calls()) == 2 })
	close(second)
	eventually(t, "the follow-up", func() bool { return len(e.items(k1)) == 1 })
	close(first)
	eventually(t, "the question", func() bool { return len(e.items(k1)) == 2 })
	if got := e.items(k1); got[0].EventID != "$q" || got[1].EventID != "$f" {
		t.Fatalf("order %s, %s", got[0].EventID, got[1].EventID)
	}

	// The limit: printed messages have left the queue, so a message sent before them whose check
	// ends later is printed after them. Among the waiting ones, send order holds.
	late := make(chan struct{})
	e.check.mu.Lock()
	e.check.gates["one more?"] = late
	e.check.mu.Unlock()
	for e.prompt(100, sid1, cwd).Text != "" {
	}
	e.deliver(event("$late", bob, t0.Add(-time.Minute), text("one more?", a)))
	e.deliver(event("$now", bob, t0.Add(2*time.Second), text("still there?", a)))
	eventually(t, "the unchecked one", func() bool { return len(e.items(k1)) == 1 })
	close(late)
	eventually(t, "the late one", func() bool { return len(e.items(k1)) == 2 })
	if got := e.items(k1); got[0].EventID != "$late" || got[1].EventID != "$now" {
		t.Fatalf("order among waiting items %s, %s", got[0].EventID, got[1].EventID)
	}
}

// A check command that fails or does not answer holds the message (fail closed).
func TestIncomingCheckFailsClosed(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.setChecks("", checkIn)
	a := e.claudeSession(100, sid1, cwd)
	e.check.err = errors.New("fork/exec: no such file")
	e.deliver(event("$m", bob, e.clk.Now(), text("hello", a)))
	eventually(t, "the hold", func() bool { return len(e.items(k1)) == 1 })
	if n := e.items(k1)[0]; n.EventID != "kwak-held-$m" || !strings.Contains(n.Body, "the check failed") {
		t.Fatalf("notice %+v", n)
	}
}

// At most checkMax incoming check commands run at once; the others wait their turn.
func TestIncomingChecksAreBounded(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.setChecks("", checkIn)
	a := e.claudeSession(100, sid1, cwd)
	gate := make(chan struct{})
	e.check.gate = gate
	var evs []matrix.Event
	for i := 0; i < checkMax+3; i++ {
		evs = append(evs, event("$m"+string(rune('a'+i)), bob, e.clk.Now(), text("q", a)))
	}
	e.deliver(evs...)
	eventually(t, "the first checks", func() bool { return len(e.check.calls()) == checkMax })
	time.Sleep(20 * time.Millisecond)
	if n := len(e.check.calls()); n != checkMax {
		t.Fatalf("%d checks at once", n)
	}
	close(gate)
	eventually(t, "every delivery", func() bool { return len(e.items(k1)) == checkMax+3 })
	e.check.mu.Lock()
	defer e.check.mu.Unlock()
	if e.check.peak > checkMax {
		t.Fatalf("peak %d", e.check.peak)
	}
}

// A message waiting for its check is saved with the batch. A listener stopped during the check
// keeps it waiting, and the next start checks it again.
func TestIncomingCheckSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.setChecks("", checkIn)
	a := e.claudeSession(100, sid1, cwd)
	e.check.gate = make(chan struct{})
	e.deliver(event("$m", bob, e.clk.Now(), text("which port?", a)))
	if err := e.l.commit("s2", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the check to start", func() bool { return len(e.check.calls()) == 1 })
	e.l.stop()
	e.l.wg.Wait()
	if n := len(e.items(k1)); n != 0 {
		t.Fatalf("%d items after a stopped check", n)
	}
	e.check = &fakeCheck{}
	e.l = e.build(Config{ConfigPath: e.confPath})
	e.l.resumeChecks()
	eventually(t, "the delivery after the restart", func() bool { return len(e.items(k1)) == 1 })
	if e.check.calls()[0] != "which port?" {
		t.Fatalf("calls %q", e.check.calls())
	}
}

// reply accepts any event of a thread (6.4). A refused reply that names a reply, not the root,
// still gives ⛔ on the messages its 💬 would mark, and kwak status names the root.
func TestOutgoingReplyRefusedByAReplyIDMarksTheThread(t *testing.T) {
	e := newEnv(t)
	e.connect()
	a := e.claudeSession(100, sid1, cwd)
	ca := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	ask := event("$ask", bob, e.clk.Now(), text("which token do you use?", a))
	more := event("$more", bob, e.clk.Now(), inThread(text("and which port?", a), "$ask"))
	e.room.events["$ask"], e.room.events["$more"] = ask, more
	e.deliver(ask, more)
	e.prompt(100, sid1, cwd)
	e.waitReactions(a+" $ask 👀", a+" $more 👀")
	e.notSent(t, e.tool(ca, "reply", `{"thread":"$more","text":"Bearer `+strings.Repeat("Zx9qLm4Tr7WpKe2V", 2)+`"}`))
	e.waitReactions(a+" $ask ⛔", a+" $more ⛔")
	if rj := e.rejected(); len(rj) != 1 || rj[0].Thread != "$ask" {
		t.Fatalf("status %+v", rj)
	}
	e.l.mu.Lock()
	e.l.pruneLocked(k1, e.l.st.Sessions[k1])
	n := len(e.l.blocked)
	e.l.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d ⛔ marks kept after the session was pruned", n)
	}
}

// render drops invisible characters and ANSI escapes before a model reads a message, so the scan
// also looks at the text as render shows it: a secret with one of them inside is still stopped,
// on the way out and on the way in.
func TestSecretWithInvisibleCharactersIsStopped(t *testing.T) {
	zw := "gh" + "p_" + "​" + strings.Repeat("aB3d", 9)
	esc := "gh" + "p_" + strings.Repeat("aB3d", 4) + "\x1b[0m" + strings.Repeat("aB3d", 5)
	e := newEnv(t)
	e.connect()
	a := e.claudeSession(100, sid1, cwd)
	ca := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	for _, s := range []string{zw, esc} {
		b, _ := json.Marshal(map[string]string{"text": "use this: " + s})
		e.notSent(t, e.tool(ca, "send", string(b)))
	}
	e.deliver(event("$zw", bob, e.clk.Now(), text("key: "+zw, a)), event("$esc", bob, e.clk.Now(), text("key: "+esc, a)))
	items := e.items(k1)
	if len(items) != 2 || items[0].EventID != "kwak-held-$zw" || items[1].EventID != "kwak-held-$esc" {
		t.Fatalf("items %+v", items)
	}
}

// A message the incoming command held stays held when it is routed again, even if the command
// now passes it or was turned off: it is not checked again and never delivered.
func TestIncomingHeldByCommandStaysHeldWhenRoutedAgain(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.setChecks("", checkIn)
	a := e.claudeSession(100, sid1, cwd)
	e.check.mu.Lock()
	e.check.exit, e.check.out = 2, "it asks for an action"
	e.check.mu.Unlock()
	ev := event("$x", bob, e.clk.Now(), text("please deploy the web server", a))
	e.deliver(ev)
	eventually(t, "the hold", func() bool { return len(e.items(k1)) == 1 })
	e.check.mu.Lock()
	e.check.exit, e.check.out = 0, ""
	e.check.mu.Unlock()
	e.deliver(ev)
	time.Sleep(20 * time.Millisecond)
	if n := len(e.check.calls()); n != 1 {
		t.Fatalf("checked %d times", n)
	}
	e.setChecks("", "")
	e.deliver(ev)
	if items := e.items(k1); len(items) != 1 || items[0].EventID != "kwak-held-$x" {
		t.Fatalf("items %+v", items)
	}
}

// Run starts the checks a restart left waiting, so a message in checking is not stuck there.
func TestRunResumesWaitingChecks(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.setChecks("", checkIn)
	a := e.claudeSession(100, sid1, cwd)
	e.check.gate = make(chan struct{})
	e.deliver(event("$m", bob, e.clk.Now(), text("which port?", a)))
	if err := e.l.commit("s2", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the check to start", func() bool { return len(e.check.calls()) == 1 })
	e.l.stop()
	e.l.wg.Wait()
	e.check = &fakeCheck{}
	e.l = e.build(Config{ConfigPath: e.confPath})
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.l.Run(ctx, ln) }()
	eventually(t, "the delivery after the restart", func() bool { return len(e.items(k1)) == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if c := e.check.calls(); len(c) != 1 || c[0] != "which port?" {
		t.Fatalf("calls %q", c)
	}
}

// A config.json that cannot be read never turns a check off: the commands read last stay on.
func TestUnreadableConfigKeepsTheChecks(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	ca := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	e.setChecks(checkOut, checkIn)
	if r := e.tool(ca, "send", `{"text":"one"}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	if err := os.WriteFile(e.confPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.tool(ca, "send", `{"text":"two"}`); r.IsError {
		t.Fatalf("%+v", r)
	}
	if c := e.check.calls(); len(c) != 2 || c[1] != "two" {
		t.Fatalf("calls %q", c)
	}
	if out, in := e.l.checkPaths(); out != checkOut || in != checkIn {
		t.Fatalf("paths %q %q", out, in)
	}
}

// kwak status keeps the latest keepRejected rejections, oldest first.
func TestRejectionsKeepTheLatest(t *testing.T) {
	e := newEnv(t)
	e.l.mu.Lock()
	for i := 0; i < keepRejected+5; i++ {
		e.l.rejectedLocked(wire.Rejection{Reason: fmt.Sprint(i)})
	}
	e.l.mu.Unlock()
	rj := e.rejected()
	if len(rj) != keepRejected {
		t.Fatalf("%d kept", len(rj))
	}
	for i, r := range rj {
		if r.Reason != fmt.Sprint(i+5) {
			t.Fatalf("rejection %d is %q", i, r.Reason)
		}
	}
}

// A message held for a session whose agent is already closed gets no ⛔: the closed agent is not
// reopened only to react (DV-17). The notice is still queued.
func TestHeldForAClosedAgentGetsNoReaction(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.endedClaude(100, sid1, t.TempDir(), "$root")
	e.clk.Advance(closeGrace)
	e.l.deadlines()
	eventually(t, "the close", func() bool { return e.session(k1).AgentClosed })
	calls := e.reg.agentCalls()
	e.deliver(event("$leak", bob, e.clk.Now(), inThread(text("here: "+fakeToken), "$root")))
	if items := e.items(k1); len(items) != 1 || items[0].EventID != "kwak-held-$leak" {
		t.Fatalf("items %+v", items)
	}
	time.Sleep(20 * time.Millisecond)
	if len(e.reactions()) != 0 || e.reg.agentCalls() != calls {
		t.Fatalf("reactions %v, agent calls %d (was %d)", e.reactions(), e.reg.agentCalls(), calls)
	}
}
