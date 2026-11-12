package listener

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

func (e *env) phase() (string, string) {
	e.l.mu.Lock()
	defer e.l.mu.Unlock()
	return e.l.phase, e.l.detail
}

func (e *env) supervise() {
	go e.l.supervise(e.l.bg)
}

func TestNotPaired(t *testing.T) {
	e := newEnv(t, unpairedOpt)
	e.supervise()
	if p, d := e.phase(); p != wire.StateUnpaired || d != textNotPaired {
		t.Fatalf("%s %q", p, d)
	}
	h := e.claudeTree(100)
	e.child(h, 110)
	if r := e.start(110, "claude", sid1, "startup", cwd); r.Err != wire.IPCErrUnpaired {
		t.Fatalf("hook: %+v", r)
	}
	c := e.mcp(e.child(h, 120).PID, "claude", "")
	if r := e.tool(c, "status", `{}`); r.Text != textNotPaired || !r.IsError {
		t.Fatalf("tool: %+v", r)
	}
	if e.reg.agentCalls() != 0 {
		t.Fatal("minted while unpaired")
	}
}

func TestKeyMissingIsUnpaired(t *testing.T) {
	e := newEnv(t)
	e.sec.Delete(secretsKey)
	e.supervise()
	eventually(t, "unpaired", func() bool { _, d := e.phase(); return d == textKeyMissing })
}

// The reload ping is read-only; the agent without a token is minted at its next use (spec 5.1).
func TestReloadLeavesUnpaired(t *testing.T) {
	e := newEnv(t)
	e.sec.Delete(secretsKey)
	e.supervise()
	eventually(t, "unpaired", func() bool { p, _ := e.phase(); return p == wire.StateUnpaired })
	e.fixtureProcs()
	e.claudeLive()
	e.l.mu.Lock()
	e.l.tokens = map[string]string{}
	e.l.mu.Unlock()
	if r := e.l.reload(); r.OK || r.Err != wire.IPCErrUnpaired || r.Detail != textKeyMissing {
		t.Fatalf("reload without a key: %+v", r)
	}
	e.sec.Set(secretsKey, wiretest.OwnerKey)
	if r := e.l.reload(); !r.OK {
		t.Fatalf("reload: %+v", r)
	}
	if p, _ := e.phase(); p == wire.StateUnpaired {
		t.Fatal("still unpaired")
	}
	eventually(t, "reload alive", func() bool { return e.reg.aliveCalls() == 1 })
	if e.reg.agentCalls() != 0 || e.reg.touches[0] {
		t.Fatalf("reload minted (%d) or touched (%v)", e.reg.agentCalls(), e.reg.touches)
	}
	if _, tok, et := e.l.token(context.Background(), k1, ""); tok == "" || et != "" {
		t.Fatalf("token %q %q", tok, et)
	}
	if n := e.reg.agentCalls(); n != 1 {
		t.Fatalf("%d POST /agents", n)
	}
}

func (f *fakeReg) aliveCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.alives)
}

// A re-pair reloads the listener; live agents keep their tokens (spec 4 step 6, E20). Agents the
// registrar lists as closed lose theirs and reopen at the session's next hook.
func TestReloadKeepsLiveTokens(t *testing.T) {
	e := newEnv(t)
	e.fixtureProcs()
	s := e.claudeLive()
	e.addHelper(s, "tests", "@helper:"+wiretest.ServerName)
	e.l.mu.Lock()
	e.l.tokens[wiretest.ClaudeAgent] = "tok-live-agent"
	e.l.mu.Unlock()
	e.supervise()
	eventually(t, "startup alive", func() bool { return e.reg.aliveCalls() == 1 })
	tokens := func() (string, string) {
		e.l.mu.Lock()
		defer e.l.mu.Unlock()
		return e.l.tokens[wiretest.ClaudeAgent], e.l.tokens["@helper:"+wiretest.ServerName]
	}
	if r := e.l.reload(); !r.OK {
		t.Fatalf("reload: %+v", r)
	}
	eventually(t, "reload alive", func() bool { return e.reg.aliveCalls() == 2 })
	time.Sleep(50 * time.Millisecond)
	if n := e.reg.agentCalls(); n != 0 {
		t.Fatalf("%d POST /agents after a reload of live agents", n)
	}
	if a, h := tokens(); a != "tok-live-agent" || h != "tok-helper-0001" {
		t.Fatalf("tokens changed: %s %s", a, h)
	}
	e.reg.mu.Lock()
	e.reg.closed[wiretest.ClaudeAgent] = true
	e.reg.mu.Unlock()
	if r := e.l.reload(); !r.OK {
		t.Fatalf("reload: %+v", r)
	}
	eventually(t, "closed seen", func() bool {
		e.l.mu.Lock()
		defer e.l.mu.Unlock()
		return e.l.st.Sessions[k1].AgentClosed && e.l.tokens[wiretest.ClaudeAgent] == ""
	})
	if n := e.reg.agentCalls(); n != 0 {
		t.Fatalf("reload reopened a closed agent with no use: %d calls", n)
	}
	e.hook(claudeHk.PID, hookReq(wire.HarnessClaude, wire.EventPrompt, sid1, wiretest.ClaudeCwd))
	eventually(t, "reopen", func() bool { return e.reg.agentCalls() == 1 })
	e.waitAgent(k1)
	e.reg.mu.Lock()
	minted := e.reg.tokens[0]
	e.reg.mu.Unlock()
	if a, h := tokens(); a != minted || h != "tok-helper-0001" {
		t.Fatalf("closed agent kept its token or the helper lost its own: %s %s", a, h)
	}
}

func TestReloadRereadsConfig(t *testing.T) {
	e := newEnv(t)
	e.supervise()
	eventually(t, "connect", func() bool { return e.reg.standingCalls() == 1 })
	c := machineConf()
	c.PairingID = "aaaa0000"
	// A re-pair writes the new pairing's own keyring item next to config.json (decision 40).
	c.KeyringUser = secrets.PairingKeyName(wiretest.ServerName, "aaaa0000")
	e.sec.Set(c.KeyringUser, wiretest.OwnerKey)
	writeConf(t, e.confPath, c)
	if r := e.l.reload(); !r.OK {
		t.Fatal(r)
	}
	if st := e.l.status(&client{pidOK: true}); st.PairingID != "aaaa0000" {
		t.Fatalf("pairing %s", st.PairingID)
	}
}

func Test401RereadsKeyringFirst(t *testing.T) {
	e := newEnv(t)
	e.supervise()
	eventually(t, "connect", func() bool { p, _ := e.phase(); return p == wire.StateConnecting && e.reg.standingCalls() == 1 })
	e.reg.mu.Lock()
	e.reg.keys = map[string]bool{"kwak_ok_new": true}
	e.reg.mu.Unlock()
	e.sec.Set(secretsKey, "kwak_ok_new")
	e.l.regFailed(&wire.APIError{Status: 401, Errcode: wire.ErrBadKey})
	eventually(t, "re-read", func() bool {
		e.reg.mu.Lock()
		defer e.reg.mu.Unlock()
		return e.reg.standings == 2 && e.reg.key == "kwak_ok_new"
	})
	if p, _ := e.phase(); p == wire.StateUnpaired {
		t.Fatal("unpaired although the keyring had the new key")
	}
	e.reg.mu.Lock()
	e.reg.keys = map[string]bool{}
	e.reg.mu.Unlock()
	e.l.regFailed(&wire.APIError{Status: 401, Errcode: wire.ErrBadKey})
	eventually(t, "unpaired", func() bool { _, d := e.phase(); return d == textUnpaired })
}

func TestKeyringLockedWaits(t *testing.T) {
	e := newEnv(t)
	e.sec.Fail(secrets.ErrTimeout)
	e.supervise()
	eventually(t, "waiting", func() bool { p, _ := e.phase(); return p == wire.StateWaitingKeyring })
	e.fixtureProcs()
	e.claudeLive()
	c := e.mcp(claudeMCP.PID, "claude", "")
	if r := e.tool(c, "status", `{}`); r.Text != textKeyring {
		t.Fatalf("%+v", r)
	}
	e.sec.Fail(nil)
	eventually(t, "retry timer", func() bool { return e.clk.waiting() > 0 })
	e.clk.Advance(keyringRetry)
	eventually(t, "connected", func() bool { p, _ := e.phase(); return p == wire.StateConnecting && e.reg.standingCalls() == 1 })
}

func TestOwnerRemoved(t *testing.T) {
	e := newEnv(t)
	e.reg.standErr = &wire.APIError{Status: 403, Errcode: wire.ErrOwnerRemoved}
	e.supervise()
	eventually(t, "unpaired", func() bool { _, d := e.phase(); return d == textRemoved })
}

func TestConfigNewerStops(t *testing.T) {
	e := newEnv(t)
	c := machineConf()
	c.Version = ConfigVersion + 1
	writeConf(t, e.confPath, c)
	_, err := New(Config{ConfigPath: e.confPath}, t.TempDir(), Deps{Secrets: e.sec, Registrar: e.reg, Matrix: e.room.client, Procs: e.procs, Clock: e.clk})
	if !errors.Is(err, ErrConfigNewer) || err.Error() != "kwak is older than its config; go install a newer kwak" {
		t.Fatalf("%v", err)
	}
}

func TestRegistrarErrcodes(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		text    string
		retried bool
		noAgent bool
		phase   string
	}{
		{"not yours", &wire.APIError{Status: 403, Errcode: wire.ErrNotYours}, textNotYours, false, true, ""},
		{"bad session", &wire.APIError{Status: 400, Errcode: wire.ErrBadSession}, textBadSession, false, true, ""},
		{"names", &wire.APIError{Status: 409, Errcode: wire.ErrNamesExhausted}, textNames, false, true, ""},
		{"homeserver", &wire.APIError{Status: 502, Errcode: wire.ErrHomeserver, Message: "M_UNKNOWN"},
			"kwak has no agent for this session yet (registrar answered HTTP 502 KWAK_HOMESERVER); retrying", true, false, ""},
		{"proxy 502", &wire.APIError{Status: 502}, wiretest.TextNoAgentYet, true, false, ""},
		{"network", errors.New("dial tcp: refused"), "kwak has no agent for this session yet (registrar unreachable); retrying", true, false, ""},
		{"removed", &wire.APIError{Status: 403, Errcode: wire.ErrOwnerRemoved}, textRemoved, false, false, wire.StateUnpaired},
		{"bad key", &wire.APIError{Status: 401, Errcode: wire.ErrBadKey}, textUnpaired, false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.connect()
			e.fixtureProcs()
			e.live("claude", sid1, cwd, claudeH, []proc.ID{claudeH, claudeSh}, "")
			fails := true
			e.reg.agentErr = func(wire.AgentRequest) error {
				if fails {
					return c.err
				}
				return nil
			}
			_, _, text := e.l.token(context.Background(), k1, "")
			if text != c.text {
				t.Fatalf("text %q, want %q", text, c.text)
			}
			if s := e.session(k1); (s.NoAgent != "") != c.noAgent {
				t.Fatalf("no-agent %q", s.NoAgent)
			}
			if p, _ := e.phase(); c.phase != "" && p != c.phase {
				t.Fatalf("phase %s", p)
			}
			if c.name == "bad key" && len(e.l.recheck) != 1 {
				t.Fatal("401 did not ask for a keyring re-read")
			}
			fails = false
			eventually(t, "the retry timer", func() bool { return !c.retried || e.clk.waiting() >= 2 })
			e.clk.Advance(2 * time.Minute)
			time.Sleep(20 * time.Millisecond)
			want := 1
			if c.retried {
				want = 2
				eventually(t, "the retry", func() bool { return e.reg.agentCalls() == 2 })
				if reqs := e.reg.agentReqs(); reqs[0].RequestID == reqs[1].RequestID {
					t.Fatal("the retry reused the request_id")
				}
			}
			if n := e.reg.agentCalls(); n != want {
				t.Fatalf("%d calls, want %d", n, want)
			}
		})
	}
}

func TestParentClosedEnsuresParentThenRetries(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.fixtureProcs()
	e.claudeLive()
	first := true
	e.reg.agentErr = func(r wire.AgentRequest) error {
		if r.Helper != "" && first {
			first = false
			return &wire.APIError{Status: 403, Errcode: wire.ErrParentClosed}
		}
		return nil
	}
	uid, _, text := e.l.token(context.Background(), k1, "tests")
	if text != "" || uid != wiretest.HelperAgent {
		t.Fatalf("%q %q", uid, text)
	}
	if n := e.reg.agentCalls(); n != 3 {
		t.Fatalf("%d calls, want helper, parent, helper", n)
	}
}

func TestDeadlinesFireAtStartup(t *testing.T) {
	e := newEnv(t)
	e.reg.me.Settings.Resume = wire.ResumeReplies
	e.connect()
	dir := t.TempDir()
	uid := e.claudeSession(100, sid1, dir)
	c := e.mcp(e.child(proc.ID{PID: 100, Start: 100}, 120).PID, "claude", "")
	e.room.ids = []string{"$ask"}
	e.tool(c, "send", `{"text":"q","to":["agent.bob.0718a1b2c3d4e5f6"]}`)
	e.l.mcpClosed(c)
	e.procs.Kill(100)
	e.l.liveness()
	e.deliver(event("$r", mallory, e.clk.Now(), inThread(text("re"), "$other")))
	e.l.mu.Lock()
	e.l.st.Roots["$other"] = uid
	e.l.mu.Unlock()
	e.deliver(event("$r2", mallory, e.clk.Now(), inThread(text("re"), "$other")))
	if s := e.session(k1); s.CloseDue == nil || s.ResumeDue == nil {
		t.Fatalf("deadlines not set: %+v", s)
	}
	e.l.save()
	e.l.stop()

	e.clk.Advance(time.Hour)
	e.l = e.build(Config{ConfigPath: e.confPath})
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- e.l.Run(ctx, ln) }()
	eventually(t, "connect", func() bool {
		e.l.mu.Lock()
		defer e.l.mu.Unlock()
		return e.l.standing != ""
	})
	eventually(t, "close, resume and timeout at startup", func() bool {
		e.clk.Advance(deadlineEvery)
		e.reg.mu.Lock()
		closes := len(e.reg.closes)
		e.reg.mu.Unlock()
		e.l.mu.Lock()
		notice := false
		for _, it := range e.l.st.Sessions[k1].Inbox.Items {
			notice = notice || it.Reason == render.Notice
		}
		e.l.mu.Unlock()
		return closes == 1 && e.runner.count() == 1 && notice
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWallClockJumpChecksAgents(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	e.l.deadlines()
	e.clk.Advance(deadlineEvery)
	e.l.deadlines()
	time.Sleep(10 * time.Millisecond)
	e.reg.mu.Lock()
	n := len(e.reg.alives)
	e.reg.mu.Unlock()
	if n != 0 {
		t.Fatal("alive without a jump")
	}
	e.reg.mu.Lock()
	e.reg.closed[wiretest.ClaudeAgent] = true
	e.reg.mu.Unlock()
	e.clk.Advance(3 * time.Hour)
	e.l.deadlines()
	eventually(t, "alive after the jump", func() bool {
		e.reg.mu.Lock()
		defer e.reg.mu.Unlock()
		return len(e.reg.alives) == 1
	})
	eventually(t, "closed seen", func() bool {
		e.l.mu.Lock()
		defer e.l.mu.Unlock()
		return e.l.st.Sessions[k1].AgentClosed
	})
	if n := e.reg.agentCalls(); n != 1 || e.reg.touches[0] {
		t.Fatalf("the jump ping minted (%d calls) or touched", n)
	}
	e.prompt(100, sid1, cwd)
	eventually(t, "reopen", func() bool { return e.reg.agentCalls() == 2 })
}

func TestStateResetShowsInStatus(t *testing.T) {
	e := newEnv(t)
	e.l.save()
	e.l = e.build(Config{ConfigPath: e.confPath})
	if e.l.stateReset {
		t.Fatal("reset on a good file")
	}
	writeFile(t, filepath.Join(e.dir, stateFile), "{not json")
	e.l = e.build(Config{ConfigPath: e.confPath})
	if !e.l.stateReset || !e.l.idx.Incomplete {
		t.Fatal("a corrupt state.json is not a reset")
	}
	e.l.save()
	removeFile(t, filepath.Join(e.dir, stateFile))
	e.l = e.build(Config{ConfigPath: e.confPath})
	if !e.l.stateReset {
		t.Fatal("a lost state.json is not a reset")
	}
}

func TestStatusOfEndedSessions(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.claudeSession(100, sid1, cwd)
	e.hook(110, hookReq("claude", wire.EventSessionEnd, sid1, cwd))
	if st := e.l.status(&client{pidOK: true}); len(st.Sessions) != 0 {
		t.Fatalf("ended session without items listed: %+v", st.Sessions)
	}
	if r := e.l.inbox(); len(r.Sessions) != 0 {
		t.Fatal(r)
	}
	e.deliver(event("$x", bob, e.clk.Now(), text("x", wiretest.ClaudeAgent)))
	st := e.l.status(&client{pidOK: true})
	if len(st.Sessions) != 1 || st.Sessions[0].Ended.IsZero() || st.Sessions[0].Inbox != 1 {
		t.Fatalf("%+v", st.Sessions)
	}
	if r := e.l.inbox(); len(r.Sessions) != 1 || !strings.HasPrefix(r.Sessions[0].Resume, "claude --resume "+sid1) {
		t.Fatalf("%+v", r)
	}
}

func TestReloadRacesToolHelpers(t *testing.T) {
	e := newEnv(t)
	e.connect()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			e.l.connect(context.Background())
		}
	}()
	for i := 0; i < 20; i++ {
		if _, msg := e.l.normalizeTo([]string{"agent.bob.0718a1b2c3d4e5f6"}); msg != "" {
			t.Fatal(msg)
		}
		e.l.short(bob)
	}
	<-done
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// A suspend cannot be faked in a test: Go has no way to build a time whose monotonic reading
// disagrees with its wall reading. This checks that the listener never keeps a monotonic reading,
// so the jump check and the deadlines compare wall times.
func TestClockIsWallOnly(t *testing.T) {
	e := newEnv(t)
	l, err := New(Config{ConfigPath: e.confPath, CheckDir: func(string) bool { return true }}, t.TempDir(),
		Deps{Secrets: e.sec, Registrar: e.reg, Matrix: e.room.client, Procs: e.procs, Clock: systemClock{}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.stop()
	l.deadlines()
	h := e.claudeTree(100)
	e.child(h, 110)
	ch := e.chainOf(110)
	l.hookOp(hookReq("claude", wire.EventSessionStart, sid1, cwd), ch, false)
	l.hookOp(hookReq("claude", wire.EventSessionEnd, sid1, cwd), ch, false)
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.st.Sessions[k1]
	for name, v := range map[string]time.Time{"now": l.d.Clock.Now(), "last tick": l.lastTick, "close due": *s.CloseDue} {
		if strings.Contains(v.String(), " m=") {
			t.Errorf("%s keeps a monotonic reading: %s", name, v)
		}
	}
}

func TestOwnerRemovedStopsSync(t *testing.T) {
	e := newEnv(t)
	e.supervise()
	syncs := func() int {
		e.room.mu.Lock()
		defer e.room.mu.Unlock()
		return len(e.room.since)
	}
	eventually(t, "first sync", func() bool { return syncs() == 1 })
	e.reg.mu.Lock()
	e.reg.standErr = &wire.APIError{Status: 403, Errcode: wire.ErrOwnerRemoved}
	e.reg.mu.Unlock()
	e.l.regFailed(&wire.APIError{Status: 403, Errcode: wire.ErrOwnerRemoved})
	e.room.syncs <- syncResp("s1", false, "")
	time.Sleep(20 * time.Millisecond)
	if p, d := e.phase(); p != wire.StateUnpaired || d != textRemoved {
		t.Fatalf("%s %q", p, d)
	}
	if n := syncs(); n != 1 {
		t.Fatalf("%d syncs after the owner was removed", n)
	}
}

func TestReloadWithNewerConfigStops(t *testing.T) {
	e := newEnv(t)
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() { done <- e.l.Run(context.Background(), ln) }()
	eventually(t, "the supervisor's first connect", func() bool { return e.reg.standingCalls() == 1 })
	c := machineConf()
	c.Version = ConfigVersion + 1
	writeConf(t, e.confPath, c)
	if r := e.l.reload(); r.OK || r.Detail != ErrConfigNewer.Error() {
		t.Fatalf("reload: %+v", r)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrConfigNewer) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
