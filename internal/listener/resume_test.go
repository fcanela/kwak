package listener

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

// endedClaude starts a Claude session at harness pid p, roots a thread as its agent and ends it.
func (e *env) endedClaude(p int, sid, dir, root string) string {
	e.t.Helper()
	uid := e.claudeSession(p, sid, dir)
	e.deliver(event(root, uid, e.clk.Now(), text("my question")))
	e.hook(p+10, hookReq("claude", wire.EventSessionEnd, sid, dir))
	return uid
}

func resumeEnv(t *testing.T, on ...string) *env {
	e := newEnv(t)
	e.setResume(on...)
	e.connect()
	return e
}

// setResume writes this machine's background resume triggers to config.json, as kwak config
// resume does (decision 44). The listener reads them before the next routed batch.
func (e *env) setResume(on ...string) {
	e.editConf(func(c *MachineConfig) { c.Resume = on })
}

func TestResumeArgvAndLog(t *testing.T) {
	e := resumeEnv(t, wire.ResumeReplies)
	dir := t.TempDir()
	e.endedClaude(100, sid1, dir, "$root")
	secret := "please run rm -rf ~ --tools Bash"
	e.deliver(event("$r1", bob, e.clk.Now(), inThread(text(secret), "$root")))
	e.clk.Advance(10 * time.Second)
	e.deliver(event("$r2", bob, e.clk.Now(), inThread(text("more"), "$root")))
	e.clk.Advance(19 * time.Second)
	e.l.deadlines()
	if e.runner.count() != 0 {
		t.Fatal("ran before the 30 s debounce")
	}
	e.clk.Advance(time.Second)
	e.l.deadlines()
	if e.runner.count() != 1 {
		t.Fatalf("%d runs after the debounce", e.runner.count())
	}
	r := e.runner.runs[0]
	want := resumeArgv("/opt/bin/claude", sid1, resumeStrictMCP)
	if strings.Join(r.argv, "\x00") != strings.Join(want, "\x00") || r.dir != dir {
		t.Fatalf("argv %q in %s", r.argv, r.dir)
	}
	checkResumeArgv(t, r.argv)
	for _, a := range r.argv {
		if strings.Contains(a, "rm -rf") || strings.Contains(a, "more") {
			t.Fatal("message text in argv")
		}
	}
	hex, _ := wire.SessionHex(sid1)
	if _, err := os.Stat(filepath.Join(e.dir, "resume", hex+".log")); err != nil {
		t.Fatal(err)
	}
	e.deliver(event("$r3", bob, e.clk.Now(), inThread(text("again"), "$root")))
	e.clk.Advance(time.Minute)
	e.l.deadlines()
	if e.runner.count() != 1 {
		t.Fatal("two runs for one session at a time")
	}
	if st := e.l.status(&client{pidOK: true}); st.ResumeRuns != 1 || st.ResumeActive != 1 {
		t.Fatalf("status %d runs, %d active", st.ResumeRuns, st.ResumeActive)
	}
	r.done <- nil
	eventually(t, "the run to end", func() bool { return e.l.status(&client{pidOK: true}).ResumeActive == 0 })
}

// checkResumeArgv checks the shape rules of 5.11: the prompt right after -p, a value after each
// variadic flag and then another flag, and dontAsk as the permission mode.
func checkResumeArgv(t *testing.T, argv []string) {
	t.Helper()
	if argv[1] != "-p" || argv[2] != resumePrompt {
		t.Fatalf("the prompt is not right after -p: %q", argv)
	}
	variadic := map[string]bool{"--tools": true, "--allowedTools": true, "--mcp-config": true}
	for i, a := range argv {
		if variadic[a] && (i+2 >= len(argv) || !strings.HasPrefix(argv[i+2], "--")) {
			t.Fatalf("no flag follows the list of %s: %q", a, argv)
		}
	}
	if n := len(argv); argv[n-2] != "--permission-mode" || argv[n-1] != "dontAsk" {
		t.Fatalf("the run is not in dontAsk mode: %q", argv)
	}
}

// Both fences of 5.11: strict MCP with a kwak-only server, and the fallback that keeps the
// plugin's MCP servers and relies on dontAsk alone (resumeStrictMCP false).
func TestResumeArgvFences(t *testing.T) {
	for strict, want := range map[bool][]string{
		true: {"/opt/bin/claude", "-p", resumePrompt, "--resume", sid1, "--tools", "",
			"--allowedTools", "mcp__kwak__send,mcp__kwak__reply,mcp__kwak__thread",
			"--strict-mcp-config", "--mcp-config", `{"mcpServers":{"kwak":{"command":"kwak","args":["mcp","--harness","claude"]}}}`,
			"--permission-mode", "dontAsk"},
		false: {"/opt/bin/claude", "-p", resumePrompt, "--resume", sid1, "--tools", "",
			"--allowedTools", "mcp__plugin_kwak_kwak__send,mcp__plugin_kwak_kwak__reply,mcp__plugin_kwak_kwak__thread",
			"--permission-mode", "dontAsk"},
	} {
		argv := resumeArgv("/opt/bin/claude", sid1, strict)
		if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("strict %v: argv %q", strict, argv)
		}
		checkResumeArgv(t, argv)
	}
}

func TestResumeMaxPerMachine(t *testing.T) {
	e := resumeEnv(t, wire.ResumeReplies)
	for i, sid := range []string{sid1, sid2, sid3} {
		root := "$root" + sid[:4]
		e.endedClaude(100*(i+1), sid, t.TempDir(), root)
		e.deliver(event("$re"+sid[:4], bob, e.clk.Now(), inThread(text("x"), root)))
	}
	e.clk.Advance(resumeDelay)
	e.l.deadlines()
	if e.runner.count() != 2 {
		t.Fatalf("%d runs, resume_max is 2", e.runner.count())
	}
	e.runner.runs[0].done <- nil
	eventually(t, "a slot", func() bool { return e.l.status(&client{pidOK: true}).ResumeActive == 1 })
	e.clk.Advance(deadlineEvery)
	e.l.deadlines()
	if e.runner.count() != 3 {
		t.Fatalf("the third run did not start in the freed slot: %d", e.runner.count())
	}
}

func TestResumeTriggers(t *testing.T) {
	off, replies, mentions := []string(nil), []string{wire.ResumeReplies}, []string{wire.ResumeMentions}
	all := []string{wire.ResumeReplies, wire.ResumeMentions}
	cases := []struct {
		on      []string
		harness wire.Harness
		reason  string
		runs    int
	}{
		{off, "claude", "reply", 0},
		{off, "claude", "mention", 0},
		{[]string{"open", "sometimes"}, "claude", "reply", 0}, // words this kwak does not know are off
		{replies, "claude", "reply", 1},
		{replies, "claude", "mention", 0},
		{replies, "claude", "reply+mention", 1},
		{replies, "claude", "mention elsewhere", 0},
		{mentions, "claude", "reply", 0},
		{mentions, "claude", "reply+mention", 0}, // a mention in its own thread is a reply
		{mentions, "claude", "mention", 1},
		{mentions, "claude", "mention elsewhere", 1},
		{all, "claude", "reply", 1},
		{all, "claude", "reply+mention", 1},
		{all, "claude", "mention", 1},
		{all, "claude", "open", 0},
		{all, "codex", "reply", 0},
	}
	for _, c := range cases {
		e := resumeEnv(t, c.on...)
		dir := t.TempDir()
		var uid, key string
		if c.harness == "claude" {
			uid = e.endedClaude(100, sid1, dir, "$root")
			key = k1
		} else {
			codex := proc.ID{PID: 300, Start: 300}
			e.procs.Line(codex, proc.ID{PID: 299, Start: 299})
			e.child(e.child(codex, 310), 311)
			e.start(311, "codex", cx1, "startup", dir)
			key = sessionKey("codex", cx1)
			uid = e.waitAgent(key)
			e.deliver(event("$root", uid, e.clk.Now(), text("q")))
			e.hook(311, hookReq("codex", wire.EventSessionEnd, cx1, dir))
		}
		e.l.mu.Lock()
		e.l.st.Sessions[key].Volunteer = true
		e.l.reindexLocked()
		e.l.idx.Volunteers[key] = uid
		e.l.mu.Unlock()
		switch c.reason {
		case "reply":
			e.deliver(event("$x", bob, e.clk.Now(), inThread(text("a"), "$root")))
		case "mention":
			e.deliver(event("$x", bob, e.clk.Now(), text("hey", uid)))
		case "reply+mention":
			e.deliver(event("$x", bob, e.clk.Now(), inThread(text("a", uid), "$root")))
		case "mention elsewhere":
			e.deliver(event("$x", bob, e.clk.Now(), inThread(text("a", uid), "$other")))
		case "open":
			e.deliver(event("$x", bob, e.clk.Now(), text("anyone?")))
		}
		e.clk.Advance(resumeDelay)
		e.l.deadlines()
		if e.runner.count() != c.runs {
			t.Errorf("%q %s %s: %d runs, want %d", c.on, c.harness, c.reason, e.runner.count(), c.runs)
		}
	}
}

// The triggers are a machine setting in config.json (decision 44). The listener reads them before
// each routed batch, so kwak config resume needs no restart; a file it cannot read keeps the
// triggers read last; kwak status shows them. The registrar's settings play no part.
func TestResumeSetOnTheMachine(t *testing.T) {
	e := resumeEnv(t)
	if st := e.l.status(&client{pidOK: true}); st.Resume != "off" {
		t.Fatalf("status resume %q, want off", st.Resume)
	}
	e.endedClaude(100, sid1, t.TempDir(), "$root")
	e.deliver(event("$r1", bob, e.clk.Now(), inThread(text("a"), "$root")))
	e.resumeNow()
	if e.runner.count() != 0 {
		t.Fatal("a run with no triggers in config.json")
	}
	e.setResume(wire.ResumeReplies)
	e.deliver(event("$r2", bob, e.clk.Now(), inThread(text("b"), "$root")))
	e.resumeNow()
	if e.runner.count() != 1 {
		t.Fatalf("%d runs after kwak config resume replies, want 1 with no restart", e.runner.count())
	}
	if err := os.WriteFile(e.confPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := e.l.status(&client{pidOK: true}); st.Resume != "replies" {
		t.Fatalf("status resume %q with a broken config.json, want the triggers read last", st.Resume)
	}
}

// A message that waits for its incoming check uses the triggers in config.json when the check
// ends, not the ones read before it started: kwak config resume off during the check means no run,
// and kwak config resume replies during the check means one.
func TestResumeTriggersReadWhenTheCheckEnds(t *testing.T) {
	for _, c := range []struct {
		before, during []string
		runs           int
	}{
		{[]string{wire.ResumeReplies}, nil, 0},
		{nil, []string{wire.ResumeReplies}, 1},
	} {
		e := resumeEnv(t, c.before...)
		e.endedClaude(100, sid1, t.TempDir(), "$root")
		e.setChecks("", checkIn)
		gate := make(chan struct{})
		e.check.gate = gate
		e.deliver(event("$r1", bob, e.clk.Now(), inThread(text("a"), "$root")))
		eventually(t, "the check to start", func() bool { return len(e.check.calls()) == 1 })
		e.setResume(c.during...)
		close(gate)
		eventually(t, "the delivery", func() bool {
			e.l.mu.Lock()
			defer e.l.mu.Unlock()
			return len(e.l.st.Checking) == 0
		})
		e.resumeNow()
		if e.runner.count() != c.runs {
			t.Errorf("resume %q, then %q during the check: %d runs, want %d", c.before, c.during, e.runner.count(), c.runs)
		}
	}
}

func TestNoResumeWithToolsConnected(t *testing.T) {
	e := resumeEnv(t, wire.ResumeReplies, wire.ResumeMentions)
	uid := e.claudeSession(100, sid1, t.TempDir())
	e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	e.deliver(event("$x", bob, e.clk.Now(), text("hey", uid)))
	e.clk.Advance(time.Minute)
	e.l.deadlines()
	if e.runner.count() != 0 {
		t.Fatal("resumed a live session")
	}
}

// TestI1NoSecrets runs a session over the real socket and checks that no token or key appears in
// any reply or in state.json.
func TestI1NoSecrets(t *testing.T) {
	e := newEnv(t)
	e.fixtureProcs()
	e.connect()
	e.l.mu.Lock()
	e.l.phase = wire.StateSyncing
	e.l.mu.Unlock()
	var replies []string
	exchange := func(pid int, lines ...string) {
		e.pid.Store(int64(pid))
		conn := e.serve(t)
		r := bufio.NewReader(conn)
		for _, l := range lines {
			writeRaw(t, conn, l)
			replies = append(replies, readRaw(t, r))
		}
	}
	hook := func(event string) string {
		return `{"v":1,"kwak":"0.1.0","op":"hook","harness":"claude","event":"` + event + `","session_id":"` + sid1 +
			`","cwd":"` + wiretest.ClaudeCwd + `","source":"startup"}`
	}
	exchange(claudeHk.PID, hook("session-start"))
	e.waitAgent(k1)
	e.deliver(event("$q", bob, e.clk.Now(), text("q", wiretest.ClaudeAgent)))
	exchange(claudeHk.PID, hook("prompt"))
	if !strings.Contains(replies[len(replies)-1], "| q") {
		t.Fatalf("the prompt hook delivered nothing: %s", replies[len(replies)-1])
	}
	exchange(claudeMCP.PID,
		`{"v":1,"kwak":"0.1.0","op":"mcp_hello","harness":"claude","env_session":"","chain":null}`,
		`{"v":1,"op":"tool","id":1,"name":"status","args":{}}`,
		`{"v":1,"op":"tool","id":2,"name":"helper","args":{"name":"tests"}}`,
		`{"v":1,"op":"tool","id":3,"name":"send","args":{"text":"hi","to":["agent.bob.0718a1b2c3d4e5f6"]}}`,
		`{"v":1,"op":"status"}`, `{"v":1,"op":"inbox"}`)
	e.l.save()
	data, err := os.ReadFile(filepath.Join(e.dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	secrets := append([]string{wiretest.StandingToken, wiretest.OwnerKey}, e.reg.tokens...)
	if len(e.reg.tokens) < 2 {
		t.Fatalf("only %d agent tokens minted", len(e.reg.tokens))
	}
	for _, s := range secrets {
		for _, r := range replies {
			if strings.Contains(r, s) {
				t.Errorf("reply holds %q: %s", s, r)
			}
		}
		if strings.Contains(string(data), s) {
			t.Errorf("state.json holds %q", s)
		}
	}
}

func perHourOpt(n int) opt {
	return func(e *env, _ *Config) {
		c := machineConf()
		c.ResumeMax, c.ResumePerHour = 5, n
		writeConf(e.t, e.confPath, c)
	}
}

// The hourly limit of decision 38 counts run starts on this machine in a rolling hour. A run over
// it waits like a run with no free slot, also once the earlier runs have ended, and starts when
// the hour allows it. The log says so once, and kwak status shows the limit while it holds.
func TestResumePerHourLimit(t *testing.T) {
	e := newEnv(t, perHourOpt(2))
	e.setResume(wire.ResumeReplies)
	e.connect()
	t0 := e.clk.Now()
	for i, sid := range []string{sid1, sid2, sid3} {
		root := "$root" + sid[:4]
		e.endedClaude(100*(i+1), sid, t.TempDir(), root)
		e.deliver(event("$re"+sid[:4], bob, e.clk.Now(), inThread(text("x"), root)))
	}
	e.clk.Advance(resumeDelay)
	e.l.deadlines()
	if e.runner.count() != 2 {
		t.Fatalf("%d runs, the hourly limit is 2", e.runner.count())
	}
	e.waitLog("background resume waiting: hourly limit", "limit=2", "count=2")
	st := e.l.status(&client{pidOK: true})
	if next := t0.Add(resumeDelay + time.Hour).UTC(); st.ResumeHourLimit != 2 || !st.ResumeNextAt.Equal(next) {
		t.Fatalf("status limit %d, next %v, want %v", st.ResumeHourLimit, st.ResumeNextAt, next)
	}
	for i := 0; i < 2; i++ {
		e.runner.mu.Lock()
		r := e.runner.runs[i]
		e.runner.mu.Unlock()
		r.done <- nil
	}
	eventually(t, "both runs to end", func() bool { return e.l.status(&client{pidOK: true}).ResumeActive == 0 })
	for i := 0; i < 5; i++ {
		e.clk.Advance(deadlineEvery)
		e.l.deadlines()
	}
	if e.runner.count() != 2 {
		t.Fatalf("a run started over the hourly limit with free slots: %d runs", e.runner.count())
	}
	if n := strings.Count(e.logs.String(), "background resume waiting: hourly limit"); n != 1 {
		t.Fatalf("the hourly limit was logged %d times", n)
	}
	e.clk.Advance(t0.Add(resumeDelay + time.Hour).Sub(e.clk.Now()))
	e.l.deadlines()
	if e.runner.count() != 3 {
		t.Fatalf("the waiting run did not start when the hour allowed it: %d runs", e.runner.count())
	}
	if st := e.l.status(&client{pidOK: true}); st.ResumeHourLimit != 0 || !st.ResumeNextAt.IsZero() {
		t.Fatalf("status still shows the limit: %+v", st)
	}
}

// The hourly limit is logged again once a run has started since (5.11): fill the limit, let the
// hour pass so the waiting run starts, then fill it again.
func TestResumePerHourLogsAgainAfterARun(t *testing.T) {
	e := newEnv(t, perHourOpt(2))
	e.setResume(wire.ResumeReplies)
	e.connect()
	t0 := e.clk.Now()
	sids := []string{sid1, sid2, sid3}
	for i, sid := range sids {
		root := "$root" + sid[:4]
		e.endedClaude(100*(i+1), sid, t.TempDir(), root)
		e.deliver(event("$re"+sid[:4], bob, e.clk.Now(), inThread(text("x"), root)))
	}
	e.clk.Advance(resumeDelay)
	e.l.deadlines()
	e.waitLog("background resume waiting: hourly limit", "limit=2", "count=2")
	e.clk.Advance(t0.Add(resumeDelay + time.Hour).Sub(e.clk.Now()))
	e.l.deadlines()
	if e.runner.count() != 3 {
		t.Fatalf("the waiting run did not start when the hour allowed it: %d runs", e.runner.count())
	}
	for i := 0; i < 3; i++ {
		e.runner.mu.Lock()
		r := e.runner.runs[i]
		e.runner.mu.Unlock()
		r.done <- nil
	}
	eventually(t, "the runs to end", func() bool { return e.l.status(&client{pidOK: true}).ResumeActive == 0 })
	for _, sid := range sids[:2] {
		e.deliver(event("$again"+sid[:4], bob, e.clk.Now(), inThread(text("y"), "$root"+sid[:4])))
	}
	e.clk.Advance(resumeDelay)
	e.l.deadlines()
	if e.runner.count() != 4 {
		t.Fatalf("%d runs; one more fits in the hour", e.runner.count())
	}
	eventually(t, "the second hourly limit line", func() bool {
		return strings.Count(e.logs.String(), "background resume waiting: hourly limit") == 2
	})
}

// After a reload lowers resume_per_hour under the runs already started this hour, kwak status
// gives the time when enough of them are an hour old to bring the count under the new limit.
func TestResumePerHourLoweredByReload(t *testing.T) {
	e := newEnv(t, perHourOpt(5))
	e.setResume(wire.ResumeReplies)
	e.connect()
	var starts []time.Time
	for i, sid := range []string{sid1, sid2, sid3} {
		root := "$root" + sid[:4]
		e.endedClaude(100*(i+1), sid, t.TempDir(), root)
		e.deliver(event("$re"+sid[:4], bob, e.clk.Now(), inThread(text("x"), root)))
		e.clk.Advance(resumeDelay)
		e.l.deadlines()
		if e.runner.count() != i+1 {
			t.Fatalf("%d runs, want %d", e.runner.count(), i+1)
		}
		starts = append(starts, e.clk.Now())
		e.clk.Advance(time.Minute)
	}
	c := machineConf()
	c.ResumeMax, c.ResumePerHour = 5, 2
	writeConf(t, e.confPath, c)
	e.connect()
	// Three starts and a limit of 2: the count falls under the limit when the second start is an
	// hour old, not the first.
	st := e.l.status(&client{pidOK: true})
	if next := starts[1].Add(time.Hour).UTC(); st.ResumeHourLimit != 2 || !st.ResumeNextAt.Equal(next) {
		t.Fatalf("status limit %d, next %v, want %v", st.ResumeHourLimit, st.ResumeNextAt, next)
	}
}

// Without resume_per_hour in config.json the limit is 300 (decision 38).
func TestResumePerHourDefault(t *testing.T) {
	e := newEnv(t)
	if got := e.l.resumePerHour(); got != 300 {
		t.Fatalf("limit %d", got)
	}
	e = newEnv(t, perHourOpt(7))
	if got := e.l.resumePerHour(); got != 7 {
		t.Fatalf("limit %d", got)
	}
}
