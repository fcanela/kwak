package listener

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

// The responder (decision 46, spec 5.18).

func responderEnv(t *testing.T, edit ...func(*MachineConfig)) *env {
	t.Helper()
	e := newEnv(t)
	e.editConf(func(c *MachineConfig) {
		c.Responder = true
		for _, f := range edit {
			f(c)
		}
	})
	e.connect()
	return e
}

func (e *env) responderDir() string { return filepath.Join(filepath.Dir(e.confPath), "responder") }

// respRecords lists the responder records, oldest first.
func (e *env) respRecords() []*session {
	e.l.mu.Lock()
	defer e.l.mu.Unlock()
	var out []*session
	for _, s := range e.l.sortedSessions() {
		if s.Responder {
			out = append(out, s)
		}
	}
	return out
}

// respTok is the responder account's token, once minted.
func (e *env) respTok() string {
	e.l.mu.Lock()
	defer e.l.mu.Unlock()
	return e.l.tokens[wiretest.ResponderUser]
}

// runResponder lets the due responder records start their runs: the first tick asks for the
// account, the next one starts the runs.
func (e *env) runResponder(runs int) {
	e.t.Helper()
	e.clk.Advance(resumeDelay)
	e.l.deadlines()
	eventually(e.t, "the responder account", func() bool { return e.respTok() != "" })
	e.l.deadlines()
	if e.runner.count() != runs {
		e.t.Fatalf("%d runs, want %d", e.runner.count(), runs)
	}
}

// An open question from another owner's agent gets its own record and run: the default Claude
// Code command with the session id kwak picked, in the responder folder, which gets the default
// AGENTS.md. The run's SessionStart shows the question; ⏳ and 👀 come from the responder account,
// its reply posts as that account and mentions the asker, and gives 💬. A follow-up from the asker
// resumes the same session; a message of another agent in the thread does not reach it.
func TestResponderAnswersAnOpenQuestion(t *testing.T) {
	e := responderEnv(t)
	q := event("$q", bob, e.clk.Now(), text("where is the deploy guide?"))
	e.room.events["$q"] = q
	e.deliver(q)
	recs := e.respRecords()
	if len(recs) != 1 {
		t.Fatalf("%d responder records", len(recs))
	}
	s := recs[0]
	if _, err := wire.ValidSessionID(s.SessionID); err != nil || s.SessionID[14] != '4' || s.Cwd != e.responderDir() || s.Ended == nil {
		t.Fatalf("record %+v", s)
	}
	e.runResponder(1)
	r := e.runner.runs[0]
	want := []string{"/opt/bin/claude", "-p", responderPrompt, "--session-id", s.SessionID, "--model", "sonnet",
		"--tools", "Read,Grep,Glob", "--allowedTools", "Read,Grep,Glob,mcp__kwak__reply,mcp__kwak__thread",
		"--strict-mcp-config", "--mcp-config", resumeMCPConfig, "--permission-mode", "dontAsk"}
	if !slices.Equal(r.argv, want) || r.dir != e.responderDir() {
		t.Fatalf("argv %q in %s", r.argv, r.dir)
	}
	for _, a := range r.argv {
		if strings.Contains(a, "deploy guide") {
			t.Fatal("the question is in the argv")
		}
	}
	if b, err := os.ReadFile(filepath.Join(e.responderDir(), "AGENTS.md")); err != nil || string(b) != ResponderAgents {
		t.Fatalf("AGENTS.md: %q %v", b, err)
	}

	e.runTree(500)
	out := e.start(510, "claude", s.SessionID, "startup", s.Cwd)
	if !strings.Contains(out.Text, "where is the deploy guide?") || !strings.Contains(out.Text, "open question, new thread $q") {
		t.Fatalf("the run's SessionStart printed %q", out.Text)
	}
	resp := wiretest.ResponderUser
	e.waitReactions(resp+" $q ⏳", resp+" $q 👀")
	c := e.mcp(e.child(proc.ID{PID: 500, Start: 500}, 520).PID, "claude", "")
	if res := e.tool(c, "reply", `{"thread":"$q","text":"docs/deploy.md"}`); res.IsError {
		t.Fatalf("%+v", res)
	}
	e.waitReactions(resp + " $q 💬")
	sent := e.room.sentContents()
	if len(sent) != 1 || e.room.sent[0].token != e.respTok() || !slices.Equal(mentionsOf(sent[0]), []string{bob}) {
		t.Fatalf("sent %v as %s", sent, e.room.sent[0].token)
	}
	e.hook(510, hookReq("claude", wire.EventSessionEnd, s.SessionID, s.Cwd))
	e.procs.Kill(500)
	e.endRun(0)

	e.deliver(event("$f1", mallory, e.clk.Now(), inThread(text("I know too"), "$q")))
	e.deliver(event("$f2", bob, e.clk.Now(), inThread(text("and the staging one?"), "$q")))
	if items := e.items(s.Key); len(items) != 1 || items[0].EventID != "$f2" {
		t.Fatalf("the record holds %+v", items)
	}
	e.clk.Advance(resumeDelay)
	e.l.deadlines()
	if e.runner.count() != 2 {
		t.Fatalf("%d runs after the follow-up", e.runner.count())
	}
	if a := e.runner.runs[1].argv; a[3] != "--resume" || a[4] != s.SessionID || e.runner.runs[1].dir != s.Cwd {
		t.Fatalf("follow-up argv %q", a)
	}
	if n := len(e.respRecords()); n != 1 {
		t.Fatalf("%d records after the follow-up", n)
	}
}

func mentionsOf(m map[string]any) []string {
	var out []string
	if mm, ok := m["m.mentions"].(map[string]any); ok {
		ids, _ := mm["user_ids"].([]any)
		for _, id := range ids {
			out = append(out, id.(string))
		}
	}
	return out
}

// Route gives the responder an open question only from a session or helper agent of another
// owner, unless the owner's own agents are answered too (decision 46, "no matter the machine").
// A person, the registrar bot, a listener or responder account, a question with mentions, and
// anything while the responder is off reach no responder.
func TestRouteToTheResponder(t *testing.T) {
	ts := time.Now()
	own := "@agent." + wiretest.Owner + ".0718a1b2c3d4e5f6:" + wiretest.ServerName
	idx := &Index{Room: wiretest.RoomID, Mine: map[string]Agent{}, Roots: map[string]string{}, Threads: map[string]Thread{},
		Volunteers: map[string]string{}, RespThreads: map[string]respThread{"$old": {Session: "claude:x", Asker: bob}},
		Responder: true, Owner: wiretest.Owner, ResponderUser: wiretest.ResponderUser}
	for _, c := range []struct {
		name    string
		ev      func() (string, map[string]any)
		session string
		want    bool
	}{
		{"another owner's agent", func() (string, map[string]any) { return bob, text("q?") }, "", true},
		{"another owner's helper", func() (string, map[string]any) {
			return "@agent.bob.0718a1b2c3d4e5f6.tests:" + wiretest.ServerName, text("q?")
		}, "", true},
		{"the owner's own agent", func() (string, map[string]any) { return own, text("q?") }, "", false},
		{"a person", func() (string, map[string]any) { return human, text("q?") }, "", false},
		{"the bot", func() (string, map[string]any) { return wiretest.Bot, text("q?") }, "", false},
		{"a listener account", func() (string, map[string]any) { return wiretest.StandingUser, text("q?") }, "", false},
		{"a responder account", func() (string, map[string]any) {
			return "@agent.bob.calm-otter.responder:" + wiretest.ServerName, text("q?")
		}, "", false},
		{"a question with mentions", func() (string, map[string]any) { return bob, text("q?", mallory) }, "", false},
		{"the asker's follow-up", func() (string, map[string]any) { return bob, inThread(text("more?"), "$old") }, "claude:x", true},
		{"someone else in that thread", func() (string, map[string]any) { return mallory, inThread(text("me too"), "$old") }, "", false},
	} {
		sender, content := c.ev()
		ds := Route(event("$e-"+c.name, sender, ts, content), idx)
		got := slices.IndexFunc(ds, func(d Delivery) bool { return d.Responder })
		if (got >= 0) != c.want {
			t.Fatalf("%s: %+v", c.name, ds)
		}
		if got >= 0 && (ds[got].Session != c.session || ds[got].UserID != wiretest.ResponderUser || ds[got].Reason != render.OpenQuestion) {
			t.Fatalf("%s: %+v", c.name, ds[got])
		}
	}
	idx.OwnAgents = true
	if ds := Route(event("$own", own, ts, text("q?")), idx); len(ds) != 1 || !ds[0].Responder {
		t.Fatalf("own agents on: %+v", ds)
	}
	idx.Responder = false
	for _, ev := range []string{"$off1", "$off2"} {
		content := text("q?")
		if ev == "$off2" {
			content = inThread(text("more?"), "$old")
		}
		if ds := Route(event(ev, bob, ts, content), idx); len(ds) != 0 {
			t.Fatalf("responder off: %+v", ds)
		}
	}
}

// Two questions get two records with their own session ids, and both runs post with one token:
// POST /responder is called once, because each call logs the previous token out.
func TestResponderRunsShareOneAccount(t *testing.T) {
	e := responderEnv(t)
	e.deliver(event("$q1", bob, e.clk.Now(), text("one?")), event("$q2", mallory, e.clk.Now(), text("two?")))
	recs := e.respRecords()
	if len(recs) != 2 || recs[0].SessionID == recs[1].SessionID {
		t.Fatalf("records %+v", recs)
	}
	e.runResponder(2)
	if n := e.reg.responderCalls(); n != 1 {
		t.Fatalf("%d POST /responder calls", n)
	}
	e.l.mu.Lock()
	a, b := e.l.st.Sessions[recs[0].Key].Agent, e.l.st.Sessions[recs[1].Key].Agent
	e.l.mu.Unlock()
	if a != wiretest.ResponderUser || b != a {
		t.Fatalf("agents %s %s", a, b)
	}
	// Routed again (a replay after a crash), a question keeps its record and starts nothing new.
	e.deliver(event("$q1", bob, e.clk.Now(), text("one?")))
	if n := len(e.respRecords()); n != 2 {
		t.Fatalf("%d records after a replay", n)
	}
}

// A run starts only once the responder account has a token. A registrar without the route (an
// older one answers 404) starts no run, kwak status shows why, and the listener asks again only
// after a minute.
func TestResponderWaitsForItsAccount(t *testing.T) {
	e := responderEnv(t)
	e.reg.respErr = &wire.APIError{Status: 404, Errcode: "M_UNRECOGNIZED"}
	e.deliver(event("$q", bob, e.clk.Now(), text("one?")))
	e.clk.Advance(resumeDelay)
	e.l.deadlines()
	eventually(t, "the account request", func() bool { return e.reg.responderCalls() == 1 })
	eventually(t, "the failure", func() bool { return e.l.status(&client{pidOK: true}).ResponderError != "" })
	for range 3 {
		e.clk.Advance(deadlineEvery)
		e.l.deadlines()
	}
	time.Sleep(20 * time.Millisecond)
	if e.runner.count() != 0 || e.reg.responderCalls() != 1 {
		t.Fatalf("%d runs, %d calls", e.runner.count(), e.reg.responderCalls())
	}
	if st := e.l.status(&client{pidOK: true}); st.ResponderError != "registrar answered HTTP 404 M_UNRECOGNIZED" || st.Responder != "on" {
		t.Fatalf("status %+v", st)
	}
	e.reg.mu.Lock()
	e.reg.respErr = nil
	e.reg.mu.Unlock()
	e.clk.Advance(respRetry)
	e.l.deadlines()
	eventually(t, "the account", func() bool { return e.respTok() != "" })
	e.l.deadlines()
	if e.runner.count() != 1 {
		t.Fatalf("%d runs once the account works", e.runner.count())
	}
}

// The command, the prompt and the folder come from config.json, with {claude}, {prompt},
// {session} and {dir} filled in; a command without {session}, or one that does not start with an
// absolute path, starts no run. responder_command and responder_continue go together: with only one
// of them set, no run starts, not even the first one, so a follow-up never falls back to kwak's
// own command.
func TestResponderCommandFromConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-responder")
	e := responderEnv(t, func(c *MachineConfig) {
		c.ResponderDir, c.ResponderPrompt = dir, "answer it"
		c.ResponderCommand = []string{"/usr/bin/codex", "exec", "-C", "{dir}", "--id={session}", "{prompt}"}
		c.ResponderContinue = []string{"/usr/bin/codex", "exec", "resume", "{session}", "{prompt}"}
	})
	e.deliver(event("$q", bob, e.clk.Now(), text("one?")))
	e.runResponder(1)
	s := e.respRecords()[0]
	want := []string{"/usr/bin/codex", "exec", "-C", dir, "--id=" + s.SessionID, "answer it"}
	if r := e.runner.runs[0]; !slices.Equal(r.argv, want) || r.dir != dir {
		t.Fatalf("argv %q in %s", r.argv, r.dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}

	good := []string{"/usr/bin/x", "{session}"}
	for name, c := range map[string]struct{ cmd, cont []string }{
		"no session":           {[]string{"/usr/bin/x", "{prompt}"}, good},
		"relative path":        {[]string{"x", "{session}"}, good},
		"only the command":     {good, nil},
		"only the continue":    {nil, good},
		"continue, no session": {good, []string{"/usr/bin/x", "{prompt}"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := responderEnv(t, func(m *MachineConfig) { m.ResponderCommand, m.ResponderContinue = c.cmd, c.cont })
			e.deliver(event("$q", bob, e.clk.Now(), text("one?")))
			e.clk.Advance(resumeDelay)
			e.l.deadlines()
			if e.runner.count() != 0 {
				t.Fatal("a run started")
			}
			e.waitLog("background resume skipped", "responder")
		})
	}
}

// EnsureResponderDir writes AGENTS.md only when there is none: the user's file stays as it is.
func TestEnsureResponderDirKeepsTheUsersFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	if err := EnsureResponderDir(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", fi.Mode())
	}
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "my rules")
	if err := EnsureResponderDir(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md")); string(b) != "my rules" {
		t.Fatalf("AGENTS.md is now %q", b)
	}
	if err := EnsureResponderDir("relative"); err == nil {
		t.Fatal("a relative folder was taken")
	}
}

// A responder record never closes or pings the shared account: after its run's session ends and
// the close grace runs out, POST /agents/close is not called for it, and the alive pings leave it
// out, so a run never loses its token to them.
func TestResponderAccountIsNeverClosedOrPinged(t *testing.T) {
	e := responderEnv(t)
	e.deliver(event("$q", bob, e.clk.Now(), text("one?")))
	e.runResponder(1)
	s := e.respRecords()[0]
	e.runTree(500)
	e.start(510, "claude", s.SessionID, "startup", s.Cwd)
	e.l.alive(pingReconcile)
	e.l.alive(pingDaily)
	e.l.alive(pingUse, s.Key)
	e.hook(510, hookReq("claude", wire.EventSessionEnd, s.SessionID, s.Cwd))
	e.clk.Advance(closeGrace + time.Second)
	e.l.deadlines()
	e.reg.mu.Lock()
	closes, alives := e.reg.closes, e.reg.alives
	e.reg.mu.Unlock()
	for _, ids := range append(closes, alives...) {
		if slices.Contains(ids, wiretest.ResponderUser) {
			t.Fatalf("closes %v, alives %v", closes, alives)
		}
	}
	e.l.mu.Lock()
	closed := e.l.st.Sessions[s.Key].AgentClosed
	e.l.mu.Unlock()
	if e.respTok() == "" || closed {
		t.Fatal("the responder lost its token")
	}
}

// All responder records share one folder; a person's session started there does not take their
// questions (5.5's hand-over is for ended sessions of people).
func TestResponderQuestionsAreNotHandedOver(t *testing.T) {
	e := responderEnv(t)
	e.deliver(event("$q", bob, e.clk.Now(), text("one?")))
	if err := EnsureResponderDir(e.responderDir()); err != nil {
		t.Fatal(err)
	}
	e.claudeSession(100, sid1, e.responderDir())
	if items := e.items(k1); len(items) != 0 {
		t.Fatalf("a person's session got %+v", items)
	}
	if items := e.items(e.respRecords()[0].Key); len(items) != 1 {
		t.Fatalf("the record holds %+v", items)
	}
}

// A responder run may use only reply, thread and status.
func TestResponderToolsAreFenced(t *testing.T) {
	e := responderEnv(t)
	e.deliver(event("$q", bob, e.clk.Now(), text("one?")))
	e.runResponder(1)
	s := e.respRecords()[0]
	e.runTree(500)
	e.start(510, "claude", s.SessionID, "startup", s.Cwd)
	c := e.mcp(e.child(proc.ID{PID: 500, Start: 500}, 520).PID, "claude", "")
	for _, tool := range []string{"send", "helper", "volunteer"} {
		if r := e.tool(c, tool, `{"text":"x","name":"x","on":true}`); !r.IsError || !strings.Contains(r.Text, "a responder run answers with reply") {
			t.Fatalf("%s: %+v", tool, r)
		}
	}
	if r := e.tool(c, "status", `{}`); r.IsError {
		t.Fatalf("status: %+v", r)
	}
}

// Turned off, the responder takes no new question, and a question that waits starts no run, even
// with the account ready.
func TestResponderOffStartsNothing(t *testing.T) {
	e := responderEnv(t)
	e.deliver(event("$q", bob, e.clk.Now(), text("one?")))
	if err := e.l.mintResponder(e.respRecords()[0].Key); err != nil {
		t.Fatal(err)
	}
	e.editConf(func(c *MachineConfig) { c.Responder = false })
	e.deliver(event("$q2", bob, e.clk.Now(), text("two?")))
	e.clk.Advance(resumeDelay)
	e.l.deadlines()
	e.l.deadlines()
	time.Sleep(20 * time.Millisecond)
	if n := len(e.respRecords()); n != 1 || e.runner.count() != 0 {
		t.Fatalf("%d records, %d runs", n, e.runner.count())
	}
	if st := e.l.status(&client{pidOK: true}); st.Responder != "off" {
		t.Fatalf("status %q", st.Responder)
	}
}

// The time limits come from config.json: responder_timeout for a responder run and
// resume_timeout for a background run, each 15 minutes when missing (decision 46).
func TestRunTimeouts(t *testing.T) {
	e := responderEnv(t, func(c *MachineConfig) { c.ResponderTimeout = "5m" })
	e.deliver(event("$q", bob, e.clk.Now(), text("one?")))
	e.runResponder(1)
	ctx := e.runner.runs[0].ctx
	e.clk.Advance(5*time.Minute - time.Second)
	time.Sleep(10 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("stopped before its limit")
	}
	e.clk.Advance(time.Second)
	eventually(t, "the 5 minute limit", func() bool { return ctx.Err() != nil })

	e2 := newEnv(t)
	e2.editConf(func(c *MachineConfig) { c.ResumeTimeout = "20m" })
	e2.setResume(wire.ResumeReplies)
	e2.connect()
	dir := t.TempDir()
	e2.endedClaude(100, sid1, dir, "$root")
	e2.deliver(event("$r", bob, e2.clk.Now(), inThread(text("answer"), "$root")))
	e2.resumeNow()
	ctx2 := e2.runner.runs[0].ctx
	e2.clk.Advance(wire.DefaultRunTimeout)
	time.Sleep(10 * time.Millisecond)
	if ctx2.Err() != nil {
		t.Fatal("resume_timeout 20m stopped at 15 minutes")
	}
	e2.clk.Advance(5 * time.Minute)
	eventually(t, "the 20 minute limit", func() bool { return ctx2.Err() != nil })
}

// A follow-up in a responder thread that the run was shown and did not answer gets 👍 at a clean
// end, like a reply in a session's own thread (decision 45, Q55); the question itself, a root,
// never does.
func TestResponderThumbsUpOnFollowUps(t *testing.T) {
	e := responderEnv(t)
	q := event("$q", bob, e.clk.Now(), text("one?"))
	e.room.events["$q"] = q
	e.deliver(q, event("$thanks", bob, e.clk.Now(), inThread(text("thanks!"), "$q")))
	e.runResponder(1)
	s := e.respRecords()[0]
	e.runTree(500)
	e.start(510, "claude", s.SessionID, "startup", s.Cwd)
	resp := wiretest.ResponderUser
	e.waitReactions(resp+" $q 👀", resp+" $thanks 👀")
	e.endRun(0)
	e.waitReactions(resp + " $thanks 👍")
	e.noReaction(resp + " $q 👍")
}

// A run whose SessionStart never reached kwak was shown nothing: its end is logged as such.
func TestResponderRunWithoutHooksIsLogged(t *testing.T) {
	e := responderEnv(t)
	e.deliver(event("$q", bob, e.clk.Now(), text("one?")))
	e.runResponder(1)
	e.endRun(0)
	e.waitLog("background resume ended without its session start")
}

// state.json from a kwak before decision 46 loads: it has no responder threads, and its sessions
// carry the old volunteer flag.
func TestStateBeforeTheResponderLoads(t *testing.T) {
	dir := t.TempDir()
	old := `{"version":1,"sessions":{"claude:` + sid1 + `":{"key":"claude:` + sid1 + `","harness":"claude","session_id":"` + sid1 +
		`","volunteer":true,"inbox":{"items":[]}}},"mine":{},"roots":{}}`
	writeFile(t, filepath.Join(dir, stateFile), old)
	st, reset, err := loadState(dir)
	if err != nil || reset || st.RespThreads == nil || st.Sessions["claude:"+sid1] == nil {
		t.Fatalf("%+v %v %v", st, reset, err)
	}
	b, _ := json.Marshal(newState())
	if strings.Contains(string(b), "responder") {
		t.Fatalf("an empty state writes a responder key: %s", b)
	}
}

// An open question with a secret in it is held from the responder's record like from any inbox:
// one notice, and ⛔ from the responder account, which a record that never ran may mint (5.16).
func TestResponderHeldQuestion(t *testing.T) {
	e := responderEnv(t)
	e.deliver(event("$q", bob, e.clk.Now(), text("is "+fakeToken+" still valid?")))
	s := e.respRecords()[0]
	if items := e.items(s.Key); len(items) != 1 || items[0].EventID != heldID("$q") {
		t.Fatalf("record holds %+v", items)
	}
	e.waitReactions(wiretest.ResponderUser + " $q ⛔")
}

// A responder record is pruned 90 days after its session ended, and its thread goes with it, so a
// later message in that thread reaches no record.
func TestResponderRecordIsPruned(t *testing.T) {
	e := responderEnv(t)
	e.deliver(event("$q", bob, e.clk.Now(), text("one?")))
	e.l.mu.Lock()
	for _, s := range e.l.st.Sessions {
		s.Inbox.Items = nil
	}
	e.l.mu.Unlock()
	e.clk.Advance(keepEnded + time.Minute)
	e.l.liveness()
	e.l.mu.Lock()
	n, threads := len(e.l.st.Sessions), len(e.l.st.RespThreads)
	e.l.mu.Unlock()
	if n != 0 || threads != 0 {
		t.Fatalf("%d records, %d threads after 90 days", n, threads)
	}
}

// responder_dir is read with each batch, so a new folder applies to the next question with no
// restart; resume_exe, the {claude} of the command, is read at start and at reload, as for
// background resume. A question's record keeps the folder it was made in.
func TestResponderReadsItsFolderLive(t *testing.T) {
	e := responderEnv(t)
	dir := filepath.Join(t.TempDir(), "elsewhere")
	e.editConf(func(c *MachineConfig) {
		c.ResponderDir = dir
		c.ResumeExe = map[wire.Harness]string{wire.HarnessClaude: "/new/claude"}
	})
	e.deliver(event("$q", bob, e.clk.Now(), text("one?")))
	if s := e.respRecords()[0]; s.Cwd != dir {
		t.Fatalf("record in %s", s.Cwd)
	}
	e.runResponder(1)
	if r := e.runner.runs[0]; r.dir != dir || r.argv[0] != "/opt/bin/claude" {
		t.Fatalf("argv %q in %s", r.argv, r.dir)
	}
}

// A follow-up is a reply in the asker's thread, never in one the responder started, also before
// the responder account has an id.
func TestResponderFollowUpIsNotOwnThread(t *testing.T) {
	e := responderEnv(t)
	e.reg.respErr = errors.New("down")
	e.deliver(event("$q", bob, e.clk.Now(), text("one?")), event("$f", bob, e.clk.Now(), inThread(text("more?"), "$q")))
	items := e.items(e.respRecords()[0].Key)
	if len(items) != 2 || items[1].EventID != "$f" || items[1].OwnThread {
		t.Fatalf("items %+v", items)
	}
}

// The owner's own agents are skipped through the whole listener path, with the owner taken from
// config.json, from any machine; responder_own_agents, read live, lets them in (decision 46).
func TestResponderSkipsTheOwnersAgents(t *testing.T) {
	e := responderEnv(t)
	own := "@agent." + wiretest.Owner + ".0718a1b2c3d4e5f6:" + wiretest.ServerName
	e.deliver(event("$own", own, e.clk.Now(), text("one?")))
	if n := len(e.respRecords()); n != 0 {
		t.Fatalf("%d records for the owner's own agent", n)
	}
	e.editConf(func(c *MachineConfig) { c.ResponderOwnAgents = true })
	e.deliver(event("$own2", own, e.clk.Now(), text("two?")))
	if n := len(e.respRecords()); n != 1 {
		t.Fatalf("%d records with responder_own_agents on", n)
	}
}

// A message for the responder older than the question timeout (15 minutes by default), as the
// back-fill brings after a night asleep, gets no record and no run: the asker was already told
// nobody answered (Q68). A fresh one in the same batch gets its record. An old follow-up in that
// thread does not reach the record.
func TestResponderSkipsOldMessages(t *testing.T) {
	e := responderEnv(t)
	old := e.clk.Now().Add(-48 * time.Hour)
	e.deliver(event("$old", bob, old, text("old?")), event("$new", mallory, e.clk.Now(), text("new?")))
	recs := e.respRecords()
	if len(recs) != 1 || len(e.items(recs[0].Key)) != 1 || e.items(recs[0].Key)[0].EventID != "$new" {
		t.Fatalf("records %+v", recs)
	}
	e.waitLog("responder: message too old", "$old")
	e.deliver(event("$f", mallory, e.clk.Now().Add(-time.Hour), inThread(text("more?"), "$new")))
	if items := e.items(recs[0].Key); len(items) != 1 {
		t.Fatalf("an old follow-up reached the record: %+v", items)
	}
	e.runResponder(1)
}

// A record made before the account had a token gets the account at its run's start, when another
// record's request minted it: its reactions and its reply need that token.
func TestResponderRunTakesTheAccountMintedForAnother(t *testing.T) {
	e := responderEnv(t)
	e.deliver(event("$q", bob, e.clk.Now(), text("one?")))
	s := e.respRecords()[0]
	if s.Agent != "" {
		t.Fatalf("agent %q before any token", s.Agent)
	}
	if err := e.l.mintResponder("another record"); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(resumeDelay)
	e.l.deadlines()
	if e.runner.count() != 1 {
		t.Fatalf("%d runs", e.runner.count())
	}
	e.l.mu.Lock()
	agent := e.l.st.Sessions[s.Key].Agent
	e.l.mu.Unlock()
	if agent != wiretest.ResponderUser {
		t.Fatalf("the run's agent is %q", agent)
	}
}

// A session id is a version 4 UUID with the RFC 4122 variant, even from random bytes that are all
// zero.
func TestNewSessionIDIsAVersion4UUID(t *testing.T) {
	l := &Listener{d: Deps{Rand: bytes.NewReader(make([]byte, 16))}}
	if id := l.newSessionID(); id != "00000000-0000-4000-8000-000000000000" {
		t.Fatalf("id %s", id)
	}
}
