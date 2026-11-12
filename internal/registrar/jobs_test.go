package registrar

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/wire"
	wt "github.com/fcanela/kwak/internal/wire/wiretest"
)

func TestIdleCloseExemptsStanding(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	b := f.mustAgent(p.key, wire.HarnessCodex, wt.CodexSession, "")
	f.clock.Advance(10 * 24 * time.Hour)
	f.callJSON("POST", wire.PathAgentsAlive, p.key, wire.AliveRequest{UserIDs: []string{b.UserID}, Touch: true}, nil)
	f.clock.Advance(5 * 24 * time.Hour)
	f.s.runJobs(t.Context())
	st := f.state()
	if r := st.Agents[claudeLP]; r.Closed == nil || r.CloseReason != "idle" || f.hs.live(a.UserID) != nil {
		t.Fatalf("idle agent %+v", r)
	}
	if r := st.Agents[codexLP]; r.Closed != nil {
		t.Fatalf("an agent alive 5 days ago was closed: %+v", r)
	}
	if lp := f.listenerLP(p.id); st.Agents[lp].Closed != nil || f.hs.live(mx(lp)) == nil {
		t.Fatal("listener account closed by the idle job")
	}
}

func TestKeyWarnAndExpiry(t *testing.T) {
	f, p := pairedFixture(t)
	dm := f.dm(mx("fcanela"))
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	f.clock.Advance(75 * 24 * time.Hour)
	f.callJSON("POST", wire.PathAgentsAlive, p.key, wire.AliveRequest{UserIDs: []string{a.UserID}, Touch: true}, nil)
	f.s.runJobs(t.Context())
	if n := f.hs.noticesIn(dm); strings.Contains(strings.Join(n, "\n"), "expires on") {
		t.Fatal("warned 15 days early")
	}
	f.clock.Advance(2 * 24 * time.Hour)
	f.callJSON("POST", wire.PathAgentsAlive, p.key, wire.AliveRequest{UserIDs: []string{a.UserID}, Touch: true}, nil)
	f.s.runJobs(t.Context())
	f.s.runJobs(t.Context())
	var warns []string
	for _, n := range f.hs.noticesIn(dm) {
		if strings.Contains(n, "expires on") {
			warns = append(warns, n)
		}
	}
	want := "Your kwak key for laptop (" + p.id + ") expires on 2027-01-31. Run kwak pair on that machine to renew it."
	if len(warns) != 1 || warns[0] != want {
		t.Fatalf("warnings %q, want one %q", warns, want)
	}
	f.clock.Advance(13 * 24 * time.Hour)
	if c := f.callJSON("GET", wire.PathMe, p.key, nil, nil); c != 401 {
		t.Fatalf("expired key before the job: %d", c)
	}
	f.s.runJobs(t.Context())
	st := f.state()
	if pr := st.Pairings[p.id]; pr.Revoked == nil || pr.RevokeReason != "expired" {
		t.Fatalf("pairing %+v", pr)
	}
	if st.Agents[claudeLP].Closed == nil || f.hs.live(a.UserID) != nil {
		t.Fatal("expiry left the pairing's agent open")
	}
}

func TestRevokeEndsEveryTokenOfThePairing(t *testing.T) {
	f, p := pairedFixture(t)
	other := f.pair("fcanela", "", true)
	dm := f.dm(mx("fcanela"))
	var sr wire.Standing
	f.callJSON("POST", wire.PathStanding, p.key, struct{}{}, &sr)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	h := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "tests")
	b := f.mustAgent(other.key, wire.HarnessCodex, wt.CodexSession, "")
	out := f.say(mx("fcanela"), dm, "revoke "+p.id)
	if len(out) != 1 || !strings.HasPrefix(out[0], "Revoked pairing "+p.id) {
		t.Fatalf("reply %q", out)
	}
	for _, uid := range []string{a.UserID, h.UserID} {
		if f.hs.live(uid) != nil || f.hs.member(testRoom, uid) {
			t.Errorf("%s still has a token or membership", uid)
		}
	}
	for _, tok := range f.hs.live(sr.UserID) {
		if tok == sr.AccessToken {
			t.Error("the pairing's standing token survived")
		}
	}
	if f.hs.live(b.UserID) == nil {
		t.Error("another pairing's agent was closed")
	}
	if c := f.callJSON("GET", wire.PathMe, p.key, nil, nil); c != 401 {
		t.Errorf("revoked key: %d", c)
	}
	// A closed name is never minted for anyone else, and the revoked pairing cannot reopen it.
	if code, _, e := f.agent(other.key, wire.HarnessClaude, wt.ClaudeSession, ""); code != 403 || e.Errcode != wire.ErrNotYours {
		t.Errorf("another pairing reopening: %d %+v", code, e)
	}
	out = f.say(mx("fcanela"), dm, "revoke all")
	if len(out) != 1 || out[0] != "Revoked 1 pairing(s) of fcanela." || f.hs.live(b.UserID) != nil {
		t.Fatalf("revoke all: %q", out)
	}
}

func TestOwnerRevokesOnlyOwnPairings(t *testing.T) {
	f := newFixture(t)
	f.addOwner("fcanela")
	bobDM := f.addOwner("bob")
	p := f.pair("fcanela", "", true)
	if out := f.say(mx("bob"), bobDM, "revoke "+p.id); len(out) != 1 || !strings.Contains(out[0], "No pairing") {
		t.Fatalf("bob revoking fcanela's pairing: %q", out)
	}
	if f.state().Pairings[p.id].Revoked != nil {
		t.Fatal("revoked")
	}
	if out := f.say(testAdmin, f.dm(testAdmin), "revoke fcanela all"); len(out) != 1 || !strings.HasPrefix(out[0], "Revoked 1") {
		t.Fatalf("admin revoke: %q", out)
	}
}

func TestOwnerRemoval(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	f.removeOwner("fcanela")
	st := f.state()
	if st.Owners["fcanela"].Removed == nil || st.Pairings[p.id].Revoked == nil {
		t.Fatal("owner or pairing left")
	}
	for _, uid := range []string{a.UserID, f.listenerUID(p.id)} {
		if f.hs.live(uid) != nil || f.hs.member(testRoom, uid) {
			t.Errorf("%s still live", uid)
		}
	}
	if r := st.Agents[claudeLP]; r == nil || r.Closed == nil {
		t.Fatal("the record must stay so the name stays reserved")
	}
	var e wire.APIError
	code, body := f.callRaw("POST", wire.PathStanding, p.key, struct{}{}, nil)
	if code != 403 || !strings.Contains(string(body), wire.ErrOwnerRemoved) {
		t.Fatalf("removed owner's key: %d %s %v", code, body, e)
	}
	if r := st.Agents[f.listenerLP(p.id)]; r == nil || r.Closed == nil || r.CloseReason != "revoked" {
		t.Fatalf("listener account record after owner remove: %+v", r)
	}
	// Restored: the next pairing gets a new listener account; the old one stays closed.
	f.say(testAdmin, f.dm(testAdmin), "owner add --restore "+mx("fcanela"))
	np := f.pair("fcanela", "", true)
	if c := f.callJSON("GET", wire.PathMe, p.key, nil, nil); c != 401 {
		t.Errorf("old pairing after restore: %d", c)
	}
	if lp := f.listenerLP(np.id); lp == f.listenerLP(p.id) || !f.hs.member(testRoom, mx(lp)) || f.state().Agents[lp].Closed != nil {
		t.Error("no new listener account after the restore")
	}
	if f.hs.member(testRoom, f.listenerUID(p.id)) {
		t.Error("the old listener account came back")
	}
}

func TestCleanupRetries(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	f.hs.fail["LogoutAll"] = mErr(502, "")
	f.mustClose(p.key, a.UserID)
	if r := f.state().Agents[claudeLP]; r.Closed == nil || r.CleanupDone {
		t.Fatalf("record %+v", r)
	}
	if code, _, _ := f.agent(p.key, wire.HarnessClaude, wt.ClaudeSession, ""); code != 502 {
		t.Fatal("reopen should fail while tuwunel fails")
	}
	delete(f.hs.fail, "LogoutAll")
	f.s.retryCleanups(t.Context())
	if r := f.state().Agents[claudeLP]; !r.CleanupDone || f.hs.live(a.UserID) != nil {
		t.Fatalf("after retry %+v", r)
	}
}

func TestPruneOldClosedRecords(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	f.mustClose(p.key, a.UserID)
	f.clock.Advance(89 * 24 * time.Hour)
	f.s.runJobs(t.Context())
	if f.state().Agents[claudeLP] == nil {
		t.Fatal("pruned early")
	}
	f.clock.Advance(2 * 24 * time.Hour)
	f.s.runJobs(t.Context())
	if f.state().Agents[claudeLP] != nil {
		t.Fatal("not pruned")
	}
	// The session comes back: tuwunel says M_USER_IN_USE, so it takes the next longer name.
	np := f.pair("fcanela", "", true)
	b := f.mustAgent(np.key, wire.HarnessClaude, wt.ClaudeSession, "")
	if b.UserID != mx(claudeLP+"8c0d") {
		t.Fatalf("after prune %s", b.UserID)
	}
}

func TestRunJobsStopsAndFlushes(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	f.clock.Advance(time.Minute)
	f.callJSON("POST", wire.PathAgentsAlive, p.key, wire.AliveRequest{UserIDs: []string{a.UserID}, Touch: true}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- f.s.RunJobs(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(f.path)
	if err != nil {
		t.Fatal(err)
	}
	st.View(func(s *State) {
		if !s.Agents[claudeLP].LastActive.Equal(t0.Add(time.Minute)) {
			t.Errorf("not flushed: %v", s.Agents[claudeLP].LastActive)
		}
	})
}

func TestStartRetriesUntilTheRoomExists(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	st, _ := OpenStore(cfg.State)
	hs := newFakeHS()
	delete(hs.aliases, testAlias)
	s := &Server{Cfg: cfg, Store: st, HS: hs, Clock: &fakeClock{t: t0}, Rand: newFixedRand()}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := s.Start(ctx); err == nil {
		t.Fatal("Start returned without a room")
	}
	hs.aliases[testAlias] = testRoom
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.roomID() != testRoom || !hs.member(testRoom, mx("registrar")) {
		t.Fatal("room not joined")
	}
}
