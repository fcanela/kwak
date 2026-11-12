package registrar

import (
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
	wt "github.com/fcanela/kwak/internal/wire/wiretest"
)

const (
	claudeLP = "agent.fcanela.4f2a9c1e7b034d5a"
	codexLP  = "agent.fcanela.8261713321e78fea"
)

func pairedFixture(t *testing.T) (*fixture, paired) {
	f := newFixture(t)
	f.addOwner("fcanela")
	return f, f.pair("fcanela", "", true)
}

func TestAgentNewRotateReopen(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	if a.UserID != mx(claudeLP) || !a.New || a.Reopened || a.DeviceID != "KWAK" || a.RoomID != testRoom {
		t.Fatalf("new %+v", a)
	}
	if !f.hs.member(testRoom, a.UserID) {
		t.Error("agent not joined")
	}
	f.hs.mu.Lock()
	display := f.hs.users[a.UserID].display
	f.hs.mu.Unlock()
	if display != claudeLP {
		t.Errorf("display name %q", display)
	}
	rec := f.state().Agents[claudeLP]
	if rec.PendingSince != nil || rec.Pairing != p.id || rec.Session != "4f2a9c1e7b034d5a8c0d1e2f3a4b5c6d" ||
		rec.MintedIP != testIP || !rec.MintedAt.Equal(t0) {
		t.Errorf("record %+v", rec)
	}

	b := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	if b.UserID != a.UserID || b.New || b.Reopened || b.AccessToken == a.AccessToken {
		t.Fatalf("rotate %+v", b)
	}
	if live := f.hs.live(a.UserID); len(live) != 1 || live[0] != b.AccessToken {
		t.Fatalf("live tokens %v, want only %s", live, b.AccessToken)
	}

	f.mustClose(p.key, a.UserID)
	if f.hs.live(a.UserID) != nil || f.hs.member(testRoom, a.UserID) {
		t.Fatal("close left a token or the membership")
	}
	if rec := f.state().Agents[claudeLP]; rec.Closed == nil || rec.CloseReason != "session_ended" || !rec.CleanupDone {
		t.Fatalf("closed record %+v", rec)
	}
	c := f.mustAgent(p.key, wire.HarnessClaude, strings.ToUpper(wt.ClaudeSession), "")
	if c.UserID != a.UserID || !c.Reopened || c.New {
		t.Fatalf("reopen %+v", c)
	}
	if live := f.hs.live(a.UserID); len(live) != 1 || live[0] != c.AccessToken || !f.hs.member(testRoom, a.UserID) {
		t.Fatalf("after reopen: tokens %v, member %v", live, f.hs.member(testRoom, a.UserID))
	}
	if rec := f.state().Agents[claudeLP]; rec.Closed != nil || rec.CloseReason != "" {
		t.Fatalf("reopened record %+v", rec)
	}
}

func TestAgentCodexTakesTheLastHex(t *testing.T) {
	f, p := pairedFixture(t)
	if a := f.mustAgent(p.key, wire.HarnessCodex, wt.CodexSession, ""); a.UserID != mx(codexLP) {
		t.Fatalf("codex agent %s", a.UserID)
	}
}

func TestAgentClashLadder(t *testing.T) {
	f, p := pairedFixture(t)
	hex, _ := wire.SessionHex(wt.ClaudeSession)
	cands := wire.Candidates("fcanela", wire.HarnessClaude, hex)
	f.hs.addHuman(mx(cands[0]))
	f.hs.addHuman(mx(cands[1]))
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	if a.UserID != mx(cands[2]) || !a.New {
		t.Fatalf("agent %s, want %s", a.UserID, cands[2])
	}
	st := f.state()
	for _, lp := range cands[:2] {
		if r := st.Agents[lp]; r == nil || r.CloseReason != "clash" || r.Session != "" {
			t.Errorf("clash record %s: %+v", lp, r)
		}
	}
	if b := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, ""); b.UserID != a.UserID || b.New {
		t.Fatalf("the session lost its name after a clash: %+v", b)
	}
	f.reopen()
	if b := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, ""); b.UserID != a.UserID {
		t.Fatalf("after a restart: %s", b.UserID)
	}
}

func TestAgentCrashSafePending(t *testing.T) {
	f, p := pairedFixture(t)
	// tuwunel registers the account, but the registrar never reads the answer.
	f.hs.failFor["Register"] = map[string]error{mx(claudeLP): mErr(502, "")}
	if code, _, _ := f.agent(p.key, wire.HarnessClaude, wt.ClaudeSession, ""); code != 502 {
		t.Fatalf("first create: %d", code)
	}
	if rec := f.state().Agents[claudeLP]; rec == nil || rec.PendingSince == nil {
		t.Fatalf("no pending record written before /register: %+v", rec)
	}
	delete(f.hs.failFor, "Register")
	f.hs.mu.Lock()
	f.hs.users[mx(claudeLP)] = &fakeUser{tokens: map[string][]string{"KWAK": {"tok-lost"}}, display: "x 💕"}
	f.hs.mu.Unlock()
	f.reopen()
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	if a.UserID != mx(claudeLP) || !a.New {
		t.Fatalf("finished %+v", a)
	}
	if live := f.hs.live(a.UserID); len(live) != 1 || live[0] != a.AccessToken {
		t.Fatalf("tokens %v: the lost register token must be logged out", live)
	}
	if rec := f.state().Agents[claudeLP]; rec.PendingSince != nil || !f.hs.member(testRoom, a.UserID) {
		t.Fatalf("record %+v", rec)
	}
}

func TestAgentRequestIDReplay(t *testing.T) {
	f, p := pairedFixture(t)
	req := wire.AgentRequest{RequestID: wt.RequestID, Harness: wire.HarnessClaude, Session: wt.ClaudeSession}
	_, a, _ := f.agentReq(p.key, req)
	logins := f.hs.callCount("Login @agent")
	_, b, _ := f.agentReq(p.key, req)
	if b != a || f.hs.callCount("Login @agent") != logins || f.hs.callCount("Register @agent.fcanela.4") != 1 {
		t.Fatalf("replay minted again: %+v vs %+v", a, b)
	}
	if live := f.hs.live(a.UserID); len(live) != 1 || live[0] != a.AccessToken {
		t.Fatalf("tokens %v", live)
	}
	f.clock.Advance(11 * time.Minute)
	_, c, _ := f.agentReq(p.key, req)
	if c.AccessToken == a.AccessToken {
		t.Fatal("a request_id older than 10 minutes was replayed")
	}
	// Another pairing replaying the id gets its own answer, never this one's token.
	other := f.pair("fcanela", "", true)
	if code, d, e := f.agentReq(other.key, req); code != 403 || e.Errcode != wire.ErrNotYours || d.AccessToken != "" {
		t.Fatalf("other pairing: %d %+v %+v", code, d, e)
	}
}

func TestAgentConcurrentRequestsForOneSession(t *testing.T) {
	f, p := pairedFixture(t)
	f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	var mu sync.Mutex
	inside := 0
	f.hs.before["LogoutAll"] = func(string) {
		mu.Lock()
		inside++
		if inside > 1 {
			t.Error("two requests for one session interleave their network calls")
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	f.hs.before["Login"] = func(string) {
		mu.Lock()
		inside--
		mu.Unlock()
	}

	// Same request_id: one mint, both answers carry the one live token.
	req := wire.AgentRequest{RequestID: wt.RequestID, Harness: wire.HarnessClaude, Session: wt.ClaudeSession}
	var wg sync.WaitGroup
	res := make([]wire.Agent, 8)
	for i := range res {
		wg.Go(func() { _, res[i], _ = f.agentReq(p.key, req) })
	}
	wg.Wait()
	for _, r := range res {
		if r.AccessToken != res[0].AccessToken {
			t.Fatalf("answers differ: %+v", res)
		}
	}
	if live := f.hs.live(mx(claudeLP)); len(live) != 1 || live[0] != res[0].AccessToken {
		t.Fatalf("live %v, want only %s", live, res[0].AccessToken)
	}

	// Different request_ids: serialised, one live token at the end, the last answer's.
	var last string
	var lmu sync.Mutex
	for range 6 {
		wg.Go(func() {
			_, a, _ := f.agent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
			lmu.Lock()
			last = max(last, a.AccessToken)
			lmu.Unlock()
		})
	}
	wg.Wait()
	if live := f.hs.live(mx(claudeLP)); len(live) != 1 || live[0] != last {
		t.Fatalf("live %v, want only the newest %s", live, last)
	}
}

func TestAgentRemintNoticeFromAnotherIP(t *testing.T) {
	f, p := pairedFixture(t)
	dm := f.dm(mx("fcanela"))
	f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	base := len(f.hs.noticesIn(dm))
	f.ip = "198.51.100.7"
	f.clock.Advance(3 * time.Minute)
	f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	got := f.hs.noticesIn(dm)[base:]
	var remint []string
	for _, n := range got {
		if strings.HasPrefix(n, "Agent "+claudeLP) {
			remint = append(remint, n)
		}
	}
	if len(remint) != 1 || !strings.Contains(remint[0], "from 198.51.100.7, 3m0s after one from "+testIP) {
		t.Fatalf("notices %q", got)
	}
	base = len(f.hs.noticesIn(dm))
	f.clock.Advance(11 * time.Minute)
	f.ip = testIP
	f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	for _, n := range f.hs.noticesIn(dm)[base:] {
		if strings.HasPrefix(n, "Agent ") {
			t.Fatalf("notice after 10 minutes: %q", n)
		}
	}
}

func TestAgentHelpers(t *testing.T) {
	f, p := pairedFixture(t)
	parent := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	h := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "tests")
	if h.UserID != wt.HelperAgent || !h.New {
		t.Fatalf("helper %+v", h)
	}
	if rec := f.state().Agents[claudeLP+".tests"]; rec.Kind != wire.KindHelper || rec.Parent != claudeLP {
		t.Fatalf("helper record %+v", rec)
	}
	var out wire.CloseResponse
	f.callJSON("POST", wire.PathAgentsClose, p.key, wire.CloseRequest{UserIDs: []string{parent.UserID}, Reason: "session_ended"}, &out)
	if len(out.Closed) != 2 || out.Closed[1] != h.UserID {
		t.Fatalf("closed %v: helpers close with their parent", out.Closed)
	}
	if f.hs.live(h.UserID) != nil {
		t.Fatal("helper token alive after its parent closed")
	}
	if code, _, e := f.agent(p.key, wire.HarnessClaude, wt.ClaudeSession, "tests"); code != 403 || e.Errcode != wire.ErrParentClosed {
		t.Fatalf("helper of a closed parent: %d %+v", code, e)
	}
}

func TestAgentSetupFailureClosesAgain(t *testing.T) {
	f, p := pairedFixture(t)
	f.hs.failFor["JoinAs"] = map[string]error{mx(claudeLP): mErr(403, "M_FORBIDDEN")}
	code, _, e := f.agent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	if code != 502 || e.Errcode != wire.ErrHomeserver || e.Message != "M_FORBIDDEN" {
		t.Fatalf("%d %+v", code, e)
	}
	if rec := f.state().Agents[claudeLP]; rec.Closed == nil || f.hs.live(mx(claudeLP)) != nil {
		t.Fatalf("not closed again: %+v", rec)
	}
	delete(f.hs.failFor, "JoinAs")
	if a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, ""); a.UserID != mx(claudeLP) || !a.Reopened {
		t.Fatalf("retry %+v", a)
	}
}

func TestAgentInvalidInputs(t *testing.T) {
	f, p := pairedFixture(t)
	before := f.hs.callCount("Register")
	for _, s := range []string{"-4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6", "4f2a9c1e/7b03-4d5a-8c0d-1e2f3a4b5c6d", "..", "", "4f2a9c1e7b034d5a8c0d1e2f3a4b5c6d"} {
		if code, _, e := f.agent(p.key, wire.HarnessClaude, s, ""); code != 400 || e.Errcode != wire.ErrBadSession {
			t.Errorf("session %q: %d %+v", s, code, e)
		}
	}
	if f.hs.callCount("Register") != before {
		t.Fatal("a bad request reached tuwunel")
	}
}

func TestCloseRefusesOtherPairingsWholeBatch(t *testing.T) {
	f, p := pairedFixture(t)
	other := f.pair("fcanela", "", true)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	b := f.mustAgent(other.key, wire.HarnessCodex, wt.CodexSession, "")
	if c := f.callJSON("POST", wire.PathAgentsClose, p.key, wire.CloseRequest{UserIDs: []string{a.UserID, b.UserID}}, nil); c != 403 {
		t.Fatalf("close: %d", c)
	}
	if st := f.state(); st.Agents[claudeLP].Closed != nil || st.Agents[codexLP].Closed != nil {
		t.Fatal("a refused batch closed something")
	}
}

func TestAliveTouchesOnlyThisPairing(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	f.clock.Advance(time.Hour)
	var out wire.AliveResponse
	f.callJSON("POST", wire.PathAgentsAlive, p.key, wire.AliveRequest{UserIDs: []string{a.UserID, f.listenerUID(p.id)}, Touch: true}, &out)
	if len(out.Unknown) != 1 || len(out.Closed) != 0 {
		t.Fatalf("alive %+v", out)
	}
	if rec := f.state().Agents[claudeLP]; !rec.LastActive.Equal(t0.Add(time.Hour)) {
		t.Fatalf("last_active %v", rec.LastActive)
	}
}

func TestAliveWithoutTouchLeavesLastActive(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	before := f.state().Agents[claudeLP].LastActive
	f.clock.Advance(time.Hour)
	var out wire.AliveResponse
	f.callJSON("POST", wire.PathAgentsAlive, p.key, wire.AliveRequest{UserIDs: []string{a.UserID}}, &out)
	if len(out.Unknown) != 0 || len(out.Closed) != 0 {
		t.Fatalf("alive %+v", out)
	}
	if rec := f.state().Agents[claudeLP]; !rec.LastActive.Equal(before) {
		t.Fatalf("a read-only ping moved last_active to %v", rec.LastActive)
	}
}

// Two machines of one owner have two listener accounts (decision 42), one device each. Revoking
// one machine closes its account and leaves the other's token alone.
func TestStandingLogsOutOnlyItsDevice(t *testing.T) {
	f, p := pairedFixture(t)
	other := f.pair("fcanela", "", true)
	var s1, s2 wire.Standing
	f.callJSON("POST", wire.PathStanding, p.key, struct{}{}, &s1)
	f.callJSON("POST", wire.PathStanding, other.key, struct{}{}, &s2)
	if s1.UserID == s2.UserID || s1.UserID != f.listenerUID(p.id) || s2.UserID != f.listenerUID(other.id) {
		t.Fatalf("two machines share a listener account: %s, %s", s1.UserID, s2.UserID)
	}
	if live := f.hs.live(s1.UserID); len(live) != 1 || live[0] != s1.AccessToken {
		t.Fatalf("first machine, live %v", live)
	}
	f.say(mx("fcanela"), f.dm(mx("fcanela")), "revoke "+p.id)
	if live := f.hs.live(s1.UserID); len(live) != 0 || f.hs.member(testRoom, s1.UserID) {
		t.Fatalf("revoked machine's account: live %v", live)
	}
	if a := f.state().Agents[f.listenerLP(p.id)]; a.Closed == nil || a.CloseReason != "revoked" || !a.CleanupDone {
		t.Fatalf("revoked machine's account record %+v", a)
	}
	if live := f.hs.live(s2.UserID); len(live) != 1 || live[0] != s2.AccessToken || !f.hs.member(testRoom, s2.UserID) {
		t.Fatalf("the other machine: live %v, want only %s", live, s2.AccessToken)
	}
}

func TestStandingDeviceLogoutError(t *testing.T) {
	f, p := pairedFixture(t)
	f.hs.fail["LogoutDevice"] = mErr(404, "M_NOT_FOUND")
	if c := f.callJSON("POST", wire.PathStanding, p.key, struct{}{}, nil); c != 200 {
		t.Fatalf("a device that does not exist yet: %d", c)
	}
	f.hs.fail["LogoutDevice"] = &matrix.Error{Status: 502}
	if c := f.callJSON("POST", wire.PathStanding, p.key, struct{}{}, nil); c != 502 {
		t.Fatalf("a failed logout must not mint a second token: %d", c)
	}
}

func TestNoTokenOnDiskOrInLogs(t *testing.T) {
	f := newFixture(t)
	var logs syncBuffer
	f.s.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.addOwner("fcanela")
	pr := f.startPair()
	f.say(mx("fcanela"), f.dm(mx("fcanela")), "pair "+pr.Code)
	var cr wire.ClaimResponse
	f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, &cr)
	var sr wire.Standing
	f.callJSON("POST", wire.PathStanding, cr.OwnerKey, struct{}{}, &sr)
	a := f.mustAgent(cr.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
	f.clock.Advance(time.Minute)
	f.callJSON("POST", wire.PathAgentsAlive, cr.OwnerKey, wire.AliveRequest{UserIDs: []string{a.UserID}, Touch: true}, nil)
	f.s.Store.Flush()
	f.hs.fail["SetDisplayName"] = mErr(500, "M_UNKNOWN")
	f.agent(cr.OwnerKey, wire.HarnessCodex, wt.CodexSession, "")
	f.say(mx("fcanela"), f.dm(mx("fcanela")), "revoke all")
	disk := string(mustRead(t, f.path))
	for name, secret := range map[string]string{"owner key": cr.OwnerKey, "claim": pr.Claim, "standing token": sr.AccessToken,
		"agent token": a.AccessToken, "hs_token": testHS} {
		if strings.Contains(disk, secret) {
			t.Errorf("%s in state.json (I5)", name)
		}
		if strings.Contains(logs.String(), secret) {
			t.Errorf("%s in the log (I4)", name)
		}
	}
	if !strings.Contains(logs.String(), "agent token") {
		t.Fatal("the log captured nothing; the check is empty")
	}
}

func TestReopenOfARecordClosedBeforeRegister(t *testing.T) {
	f, p := pairedFixture(t)
	f.hs.failFor["Register"] = map[string]error{mx(claudeLP): &matrix.Error{Status: 502}}
	f.agent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	delete(f.hs.failFor, "Register")
	out := f.say(testAdmin, f.dm(testAdmin), "close "+mx(claudeLP))
	if len(out) != 1 || !strings.HasPrefix(out[0], "Closed") {
		t.Fatalf("admin close: %q", out)
	}
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	if a.UserID != mx(claudeLP) || !a.Reopened || f.hs.live(a.UserID) == nil {
		t.Fatalf("reopen %+v", a)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
