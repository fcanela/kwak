package registrar

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/wire"
	wt "github.com/fcanela/kwak/internal/wire/wiretest"
)

// Listener accounts per pairing: decisions 42 and 43, spec 3.2, 3.4, 3.6, 3.9 and 3.10.

// seedLegacy writes state as a build before decision 42 left it: the owner's per-owner standing
// account of the kind, registered and joined to its room, and a confirmed pairing with no listener
// name whose device holds a live token on that account. It returns the device's token.
func (f *fixture) seedLegacy(id, owner, key string, test bool) string {
	f.t.Helper()
	lp := wt.LegacyStandingLocalpart(owner, test)
	room := testRoom
	if test {
		room = testRoomID
	}
	if f.state().Agents[lp] == nil {
		now := f.clock.Now()
		if err := f.s.Store.Update(func(st *State) error {
			st.putAgent(lp, &Agent{Owner: owner, Kind: wire.KindStanding, Created: now, LastActive: now})
			return nil
		}); err != nil {
			f.t.Fatal(err)
		}
		if _, err := f.hs.Register(f.t.Context(), lp, "KWAK-old"); err != nil {
			f.t.Fatal(err)
		}
		f.hs.LogoutAll(f.t.Context(), mx(lp))
		f.hs.join(room, mx(lp))
	}
	now := f.clock.Now()
	if err := f.s.Store.Update(func(st *State) error {
		st.Pairings[id] = &Pairing{Owner: owner, Machine: wt.Machine, OS: "linux", IP: testIP, KeySHA256: sha(key),
			Created: now, Confirmed: &now, Expires: now.Add(f.s.Cfg.KeyLifetime.D()),
			IPs: map[string]time.Time{testIP: now}, LastIP: testIP, Test: test}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	l, err := f.hs.Login(f.t.Context(), mx(lp), "KWAK-"+id)
	if err != nil {
		f.t.Fatal(err)
	}
	return l.AccessToken
}

const (
	legacyPairing2 = "5e6f7a8b"
	legacyKey2     = "kwak_ok_UFFSU1RVVldYWVpbXF1eX2BhYmNkZWZnaGlqa2xtbm9wcXI"
)

// A pairing made before decision 42 gets its own account at its next POST /standing, the ordinary
// lazy creation of decision 42. That call does not touch the old per-owner account: the job pass
// closes it (decision 43), which kills the token of every listener still on it, and each of them
// then gets its own account at its next POST /standing.
func TestStandingGivesAnOldPairingItsOwnAccount(t *testing.T) {
	f := newFixture(t)
	f.addOwner(wt.Owner)
	f.seedLegacy(wt.PairingID, wt.Owner, wt.OwnerKey, false)
	otherTok := f.seedLegacy(legacyPairing2, wt.Owner, legacyKey2, false)
	legacy := wt.LegacyStanding

	var sr wire.Standing
	if c := f.callJSON("POST", wire.PathStanding, wt.OwnerKey, struct{}{}, &sr); c != 200 {
		t.Fatalf("standing: %d %s", c, f.last)
	}
	lp := f.listenerLP(wt.PairingID)
	if sr.UserID != mx(lp) || sr.UserID == legacy || !f.hs.member(testRoom, sr.UserID) {
		t.Fatalf("standing %+v, listener %s", sr, lp)
	}
	if a := f.state().Agents[lp]; a == nil || a.Pairing != wt.PairingID || !a.Open() {
		t.Fatalf("new account record %+v", a)
	}
	if a := f.state().Agents[wt.LegacyStandingLocalpart(wt.Owner, false)]; a.Closed != nil || f.hs.callCount("LogoutDevice "+legacy) != 0 {
		t.Fatalf("POST /standing touched the old account: %+v, %v", a, f.hs.callLog())
	}

	f.s.runJobs(t.Context())
	a := f.state().Agents[wt.LegacyStandingLocalpart(wt.Owner, false)]
	if a.Closed == nil || a.CloseReason != closeReasonRetired || !a.CleanupDone || f.hs.member(testRoom, legacy) || f.hs.live(legacy) != nil {
		t.Fatalf("old account after a job pass: %+v, live %v (the other listener had %s)", a, f.hs.live(legacy), otherTok)
	}
	if a := f.state().Agents[lp]; !a.Open() || f.hs.live(sr.UserID) == nil {
		t.Fatalf("the job pass touched the new account: %+v", a)
	}

	// The other listener lost its token, calls POST /standing again, and gets its own account.
	var sr2 wire.Standing
	if c := f.callJSON("POST", wire.PathStanding, legacyKey2, struct{}{}, &sr2); c != 200 {
		t.Fatalf("second standing: %d %s", c, f.last)
	}
	if sr2.UserID == sr.UserID || sr2.UserID == legacy || sr2.UserID != f.listenerUID(legacyPairing2) || !f.hs.member(testRoom, sr2.UserID) {
		t.Fatalf("second standing %+v", sr2)
	}
	// A second call keeps the account and its name.
	var again wire.Standing
	if c := f.callJSON("POST", wire.PathStanding, wt.OwnerKey, struct{}{}, &again); c != 200 || again.UserID != sr.UserID {
		t.Fatalf("again: %d %+v", c, again)
	}
}

// The job pass (which also runs at the registrar's start) closes both old per-owner accounts of
// an owner, whatever pairing still has no account of its own: they leave every room, the report
// rooms too, and every device is logged out (decision 43). It makes no account for a pairing:
// that waits for the pairing's own POST /standing, which then joins only the room of its kind.
func TestJobsCloseTheOldAccounts(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(false)
	f.addOwner(wt.Owner)
	normalTok := f.seedLegacy(wt.PairingID, wt.Owner, wt.OwnerKey, false)
	testTok := f.seedLegacy(legacyPairing2, wt.Owner, legacyKey2, true)
	legacyNormal, legacyTest := mx(wt.LegacyStandingLocalpart(wt.Owner, false)), mx(wt.LegacyStandingLocalpart(wt.Owner, true))
	f.hs.join(reportRoom, legacyNormal)
	f.hs.join(testReportID, legacyTest)
	registers := f.hs.callCount("Register ")

	f.s.runJobs(t.Context())
	st := f.state()
	for _, uid := range []string{legacyNormal, legacyTest} {
		a := st.Agents[strings.TrimSuffix(strings.TrimPrefix(uid, "@"), ":"+testServer)]
		if a.Closed == nil || a.CloseReason != closeReasonRetired || !a.CleanupDone || f.hs.live(uid) != nil {
			t.Fatalf("%s after a job pass: %+v, live %v", uid, a, f.hs.live(uid))
		}
		for _, room := range []string{testRoom, reportRoom, testRoomID, testReportID} {
			if f.hs.member(room, uid) {
				t.Fatalf("%s is still in %s", uid, room)
			}
		}
	}
	if len(f.deviceTokens(legacyNormal, "KWAK-"+wt.PairingID)) != 0 || len(f.deviceTokens(legacyTest, "KWAK-"+legacyPairing2)) != 0 {
		t.Fatalf("old tokens %s and %s still live", normalTok, testTok)
	}
	for _, id := range []string{wt.PairingID, legacyPairing2} {
		if p := st.Pairings[id]; p.Listener != "" || p.Revoked != nil {
			t.Fatalf("the job pass changed pairing %s: %+v", id, p)
		}
	}
	if n := f.hs.callCount("Register ") - registers; n != 0 {
		t.Fatalf("the job pass registered %d accounts", n)
	}

	// The test pairing's listener calls POST /standing after losing its token: its own account,
	// in the test room only.
	var sr wire.Standing
	if c := f.callJSON("POST", wire.PathStanding, legacyKey2, struct{}{}, &sr); c != 200 {
		t.Fatalf("standing: %d %s", c, f.last)
	}
	if n, err := wire.ParseLocalpart(strings.TrimSuffix(strings.TrimPrefix(sr.UserID, "@"), ":"+testServer)); err != nil || !n.Test || n.Pair == "" ||
		!f.hs.member(testRoomID, sr.UserID) || f.hs.member(testRoom, sr.UserID) {
		t.Fatalf("test pairing's account %+v", sr)
	}
	// A second pass changes nothing.
	calls := len(f.hs.callLog())
	f.s.runJobs(t.Context())
	for _, c := range f.hs.callLog()[calls:] {
		if strings.Contains(c, legacyNormal) || strings.Contains(c, legacyTest) {
			t.Fatalf("a second pass called %s", c)
		}
	}
}

// A close of an old account whose leave fails is marked in the state at once and finished by
// the cleanup retries; its devices are logged out only once the leave worked, like any close.
func TestJobsFinishAFailedCloseOfAnOldAccount(t *testing.T) {
	f := newFixture(t)
	f.addOwner(wt.Owner)
	f.seedLegacy(wt.PairingID, wt.Owner, wt.OwnerKey, false)
	legacy := wt.LegacyStanding
	lp := wt.LegacyStandingLocalpart(wt.Owner, false)
	f.hs.mu.Lock()
	f.hs.failFor["LeaveAs"] = map[string]error{legacy: mErr(502, "M_UNKNOWN")}
	f.hs.mu.Unlock()
	f.s.runJobs(t.Context())
	if a := f.state().Agents[lp]; a.Closed == nil || a.CleanupDone || !f.hs.member(testRoom, legacy) {
		t.Fatalf("after a failed leave: %+v", a)
	}
	f.hs.mu.Lock()
	delete(f.hs.failFor, "LeaveAs")
	f.hs.mu.Unlock()
	f.s.retryCleanups(t.Context())
	if a := f.state().Agents[lp]; !a.CleanupDone || f.hs.member(testRoom, legacy) || f.hs.live(legacy) != nil {
		t.Fatalf("after the retry: %+v, live %v", a, f.hs.live(legacy))
	}
}

// A revoke of a pairing that never got a name has no device of its own to log out: its old token
// is on the per-owner account, which the job pass closes with every device. The logout counts as
// done at once, so the jobs do not retry it forever.
func TestRevokeOfAnOldPairing(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner(wt.Owner)
	tok := f.seedLegacy(wt.PairingID, wt.Owner, wt.OwnerKey, false)
	if out := f.say(mx(wt.Owner), dm, "revoke "+wt.PairingID); len(out) != 1 || !strings.HasPrefix(out[0], "Revoked pairing") {
		t.Fatalf("revoke %q", out)
	}
	if p := f.state().Pairings[wt.PairingID]; !p.LogoutDone || p.Listener != "" {
		t.Fatalf("revoked old pairing %+v", p)
	}
	f.s.runJobs(t.Context())
	if n := f.hs.callCount("LogoutDevice "); n != 0 {
		t.Fatalf("%d device logouts for a pairing with no account: %v", n, f.hs.callLog())
	}
	if a := f.state().Agents[wt.LegacyStandingLocalpart(wt.Owner, false)]; a.Closed == nil || f.hs.live(wt.LegacyStanding) != nil {
		t.Fatalf("old account %+v keeps token %s", a, tok)
	}
}

// A same-kind re-pair of an old pairing (no name) gets a fresh name; the confirmation moves the
// agents and revokes the old pairing, whose device logout is done at once.
func TestRepairOfAnOldPairing(t *testing.T) {
	f := newFixture(t)
	f.addOwner(wt.Owner)
	f.seedLegacy(wt.PairingID, wt.Owner, wt.OwnerKey, false)
	a := f.mustAgent(wt.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
	np := f.pair(wt.Owner, wt.OwnerKey, true)
	st := f.state()
	lp := st.Pairings[np.id].Listener
	if lp == "" || st.Agents[lp].Pairing != np.id || st.Agents[claudeLP].Pairing != np.id || f.hs.live(a.UserID) == nil {
		t.Fatalf("re-pair of an old pairing: %q %+v", lp, st.Agents[lp])
	}
	if p := st.Pairings[wt.PairingID]; p.Revoked == nil || p.RevokeReason != "replaced" || !p.LogoutDone {
		t.Fatalf("old pairing %+v", p)
	}
}

// Every pairing of one owner gets a word pair that owner never had, closed ones included, and a
// same-kind re-pair keeps its name.
func TestListenerNamesAreNeverReused(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	seen := map[string]bool{}
	var last paired
	for i := range 12 {
		p := f.pair("fcanela", "", true)
		lp := f.listenerLP(p.id)
		n, _ := wire.ParseLocalpart(lp)
		if seen[n.Pair] {
			t.Fatalf("pairing %d reused %s", i, n.Pair)
		}
		seen[n.Pair] = true
		if i%2 == 0 {
			f.say(mx("fcanela"), dm, "revoke "+p.id)
		}
		last = p
	}
	r := f.pair("fcanela", last.key, true)
	if f.listenerLP(r.id) != f.listenerLP(last.id) {
		t.Fatalf("same-kind re-pair changed the name: %s, was %s", f.listenerLP(r.id), f.listenerLP(last.id))
	}
	machines := f.say(mx("fcanela"), dm, "machines")
	n, _ := wire.ParseLocalpart(f.listenerLP(r.id))
	if len(machines) != 1 || !strings.Contains(machines[0], r.id+": laptop (linux) as "+n.Pair+", created") {
		t.Fatalf("machines %q", machines)
	}
}

// A fresh name that tuwunel already has (a record pruned long ago) is marked as a clash and the
// pairing gets another name; the bot's answer then is out of date, POST /standing has the truth.
func TestListenerNameClash(t *testing.T) {
	f := newFixture(t)
	f.addOwner("fcanela")
	p := f.pair("fcanela", "", false)
	taken := f.listenerLP(p.id)
	f.hs.addHuman(mx(taken))
	var sr wire.Standing
	if c := f.callJSON("POST", wire.PathStanding, p.key, struct{}{}, &sr); c != 200 {
		t.Fatalf("standing: %d %s", c, f.last)
	}
	st := f.state()
	lp := st.Pairings[p.id].Listener
	if lp == taken || sr.UserID != mx(lp) || !st.Agents[lp].Open() {
		t.Fatalf("after the clash: %s, standing %+v", lp, sr)
	}
	if a := st.Agents[taken]; a == nil || a.CloseReason != "clash" || a.Pairing != "" || f.hs.member(testRoom, mx(taken)) {
		t.Fatalf("clash record %+v", a)
	}
}

// A record left pending by a registrar that stopped after /register is finished by the next
// POST /standing; a failed join leaves it pending and answers 502, and the key stays unconfirmed.
func TestListenerPendingIsFinished(t *testing.T) {
	f := newFixture(t)
	f.addOwner("fcanela")
	p := f.pair("fcanela", "", false)
	lp := f.listenerLP(p.id)
	f.hs.mu.Lock()
	f.hs.failFor["JoinAs"] = map[string]error{mx(lp): mErr(500, "M_UNKNOWN")}
	f.hs.mu.Unlock()
	code, body := f.callRaw("POST", wire.PathStanding, p.key, struct{}{}, nil)
	if code != 502 || !strings.Contains(string(body), wire.ErrHomeserver) {
		t.Fatalf("standing with a failing join: %d %s", code, body)
	}
	st := f.state()
	if a := st.Agents[lp]; a == nil || a.PendingSince == nil || st.Pairings[p.id].Confirmed != nil {
		t.Fatalf("after a failed join: %+v, pairing %+v", a, st.Pairings[p.id])
	}
	f.reopen() // as after a restart
	f.hs.mu.Lock()
	delete(f.hs.failFor, "JoinAs")
	f.hs.mu.Unlock()
	var sr wire.Standing
	if c := f.callJSON("POST", wire.PathStanding, p.key, struct{}{}, &sr); c != 200 || sr.UserID != mx(lp) {
		t.Fatalf("standing after the restart: %d %+v %s", c, sr, f.last)
	}
	if a := f.state().Agents[lp]; !a.Open() || !f.hs.member(testRoom, mx(lp)) || f.hs.callCount("Register "+mx(lp)) != 2 {
		t.Fatalf("finished record %+v, %v", a, f.hs.callLog())
	}
}

// An unconfirmed re-pair took the old pairing's name; when the old pairing is revoked before the
// new key is confirmed, the old account closes with it and the new pairing gets a fresh name.
func TestRepairWhoseOldPairingWasRevokedFirst(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	old := f.pair("fcanela", "", true)
	np := f.pair("fcanela", old.key, false)
	if f.listenerLP(np.id) != f.listenerLP(old.id) {
		t.Fatal("the re-pair did not take the old name")
	}
	f.say(mx("fcanela"), dm, "revoke "+old.id)
	var sr wire.Standing
	if c := f.callJSON("POST", wire.PathStanding, np.key, struct{}{}, &sr); c != 200 {
		t.Fatalf("standing: %d %s", c, f.last)
	}
	st := f.state()
	if lp := st.Pairings[np.id].Listener; lp == f.listenerLP(old.id) || sr.UserID != mx(lp) || !st.Agents[lp].Open() {
		t.Fatalf("new pairing's account %s, standing %+v", lp, sr)
	}
	if a := st.Agents[f.listenerLP(old.id)]; a.Closed == nil || a.Pairing != old.id {
		t.Fatalf("old account %+v", a)
	}
}

// An unconfirmed re-pair that is never confirmed is revoked by the jobs; the account it would have
// taken stays with the old pairing.
func TestUnconfirmedRepairKeepsTheOldAccount(t *testing.T) {
	f := newFixture(t)
	f.addOwner("fcanela")
	old := f.pair("fcanela", "", true)
	f.pair("fcanela", old.key, false)
	f.clock.Advance(11 * time.Minute)
	f.s.runJobs(t.Context())
	lp := f.listenerLP(old.id)
	if a := f.state().Agents[lp]; !a.Open() || a.Pairing != old.id || f.hs.live(mx(lp)) == nil || !f.hs.member(testRoom, mx(lp)) {
		t.Fatalf("old account after the unconfirmed re-pair was revoked: %+v", a)
	}
}

// "listener" is a reserved helper name (decision 42).
func TestHelperNamedListenerIsRefused(t *testing.T) {
	f, p := pairedFixture(t)
	f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	code, _, e := f.agent(p.key, wire.HarnessClaude, wt.ClaudeSession, wire.ListenerPart)
	if code != 400 || e.Errcode != wire.ErrBadRequest {
		t.Fatalf("helper named listener: %d %+v", code, e)
	}
}

// A state.json from before decision 42 (no "listener" keys) still loads, and a pairing's
// "listener" key is written only once it has a name.
func TestStateWithoutListenerNames(t *testing.T) {
	f := newFixture(t)
	f.addOwner(wt.Owner)
	f.seedLegacy(wt.PairingID, wt.Owner, wt.OwnerKey, false)
	raw := mustRead(t, f.path)
	if strings.Contains(string(raw), `"listener"`) {
		t.Fatalf("a pairing with no name wrote the key:\n%s", raw)
	}
	f.reopen()
	f.callJSON("POST", wire.PathStanding, wt.OwnerKey, struct{}{}, nil)
	var st struct {
		Pairings map[string]map[string]any `json:"pairings"`
	}
	if err := json.Unmarshal(mustRead(t, f.path), &st); err != nil {
		t.Fatal(err)
	}
	if lp, _ := st.Pairings[wt.PairingID]["listener"].(string); lp == "" || !strings.HasSuffix(lp, ".listener") {
		t.Fatalf("pairing after its first POST /standing: %v", st.Pairings[wt.PairingID])
	}
}

// The old per-owner account stays a standing account to the API: no route but POST /standing
// touches it, and that one never hands it out.
func TestOldAccountIsNeverHandedOut(t *testing.T) {
	f := newFixture(t)
	f.addOwner(wt.Owner)
	f.seedLegacy(wt.PairingID, wt.Owner, wt.OwnerKey, false)
	var cr wire.CloseResponse
	if c := f.callJSON("POST", wire.PathAgentsClose, wt.OwnerKey, wire.CloseRequest{UserIDs: []string{wt.LegacyStanding}}, &cr); c != 403 {
		t.Fatalf("close of the old account: %d", c)
	}
	var sr wire.Standing
	f.callJSON("POST", wire.PathStanding, wt.OwnerKey, struct{}{}, &sr)
	if sr.UserID == wt.LegacyStanding || sr.UserID != f.listenerUID(wt.PairingID) {
		t.Fatalf("POST /standing handed out %s", sr.UserID)
	}
}

// Two re-pairs A and B of one key both take its name. When A is confirmed through another route
// while B's POST /standing is between its login and its confirmation, the account moves to A. B
// must not keep a token on A's account: it gets an account of its own, and its revoke leaves no
// token anywhere.
func TestTwoRepairsOfOneKeyKeepTheirAccountsApart(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner(wt.Owner)
	x := f.pair(wt.Owner, "", true)
	lp := f.listenerLP(x.id)
	a := f.pair(wt.Owner, x.key, false)
	b := f.pair(wt.Owner, x.key, false)
	if f.listenerLP(a.id) != lp || f.listenerLP(b.id) != lp {
		t.Fatalf("names %s %s %s", lp, f.listenerLP(a.id), f.listenerLP(b.id))
	}
	done := make(chan struct{})
	f.hook("Login", mx(lp), func() {
		go func() {
			defer close(done)
			f.callRaw("GET", wire.PathMe, a.key, nil, nil)
		}()
		f.waitState(func(st *State) bool { return st.Pairings[a.id].Confirmed != nil })
	})
	var sr wire.Standing
	code := f.callJSON("POST", wire.PathStanding, b.key, struct{}{}, &sr)
	<-done
	st := f.state()
	if st.Agents[lp].Pairing != a.id {
		t.Fatalf("setup: the account is held by %s, want A %s", st.Agents[lp].Pairing, a.id)
	}
	if toks := f.deviceTokens(mx(lp), "KWAK-"+b.id); len(toks) != 0 {
		t.Fatalf("B keeps tokens %v on A's account", toks)
	}
	if code != 200 || sr.UserID == mx(lp) || sr.UserID != f.listenerUID(b.id) || !st.hasOwnListener(b.id) {
		t.Fatalf("B's standing: %d %+v, listener %s", code, sr, st.Pairings[b.id].Listener)
	}
	if out := f.say(mx(wt.Owner), dm, "revoke "+b.id); len(out) != 1 {
		t.Fatalf("revoke %q", out)
	}
	for _, uid := range []string{mx(lp), sr.UserID} {
		if toks := f.deviceTokens(uid, "KWAK-"+b.id); len(toks) != 0 {
			t.Fatalf("revoked B keeps tokens %v on %s", toks, uid)
		}
	}
}

// A revoke that lands while POST /standing makes the pairing's account (here during a name clash,
// so the second name is chosen after the revoke) leaves no open account in the room.
func TestRevokeDuringListenerCreateLeavesNoAccount(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner(wt.Owner)
	p := f.pair(wt.Owner, "", false)
	taken := f.listenerLP(p.id)
	f.hs.addHuman(mx(taken))
	done := make(chan struct{})
	f.hook("Register", mx(taken), func() {
		go func() {
			defer close(done)
			f.say(mx(wt.Owner), dm, "revoke "+p.id)
		}()
		f.waitState(revoked(p.id))
	})
	code, _ := f.callRaw("POST", wire.PathStanding, p.key, struct{}{}, nil)
	<-done
	if code == 200 {
		t.Fatal("POST /standing of a revoked pairing answered 200")
	}
	check := func(when string) {
		for lp, a := range f.state().Agents {
			if a.Pairing == p.id && a.Closed == nil {
				t.Fatalf("%s: open account %s of the revoked pairing: %+v", when, lp, a)
			}
			if a.Kind == wire.KindStanding && a.Owner == wt.Owner && f.hs.member(testRoom, mx(lp)) {
				t.Fatalf("%s: %s is still in the room", when, lp)
			}
		}
	}
	check("after the revoke")
	f.s.runJobs(t.Context())
	check("after a job pass")
}

// The four guards against a revoke while POST /standing makes the pairing's account overlap, so
// each one has a test of its own below, where it alone decides the outcome.

// A revoke that lands before the account's record is written (here during a name clash) stops the
// call there: no record of the pairing is written and the second name is never registered. This
// is the pairingErr check in makeListener, before the pending record.
func TestRevokeBeforeTheListenerRecordMakesNoAccount(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner(wt.Owner)
	p := f.pair(wt.Owner, "", false)
	taken := f.listenerLP(p.id)
	f.hs.addHuman(mx(taken))
	done := make(chan struct{})
	f.hook("Register", mx(taken), func() {
		go func() {
			defer close(done)
			f.say(mx(wt.Owner), dm, "revoke "+p.id)
		}()
		f.waitState(revoked(p.id))
	})
	registers := f.hs.callCount("Register ")
	code, _ := f.callRaw("POST", wire.PathStanding, p.key, struct{}{}, nil)
	<-done
	if code != 401 {
		t.Fatalf("POST /standing of a revoked pairing: %d", code)
	}
	for lp, a := range f.state().Agents {
		if a.Pairing == p.id {
			t.Fatalf("the revoked pairing got a record %s: %+v", lp, a)
		}
	}
	if n := f.hs.callCount("Register ") - registers; n != 1 {
		t.Fatalf("%d registrations, want only the clash: %v", n, f.hs.callLog())
	}
}

// markRevoked marks pairing id revoked the way revoke does before it takes the standing lock. The
// revoke's own close of the pairing's accounts comes only once POST /standing lets go of that
// lock, so these tests leave it out and look at what the call itself does.
func (f *fixture) markRevoked(id string) {
	f.t.Helper()
	now := f.clock.Now()
	if err := f.s.Store.Update(func(st *State) error {
		p := st.Pairings[id]
		p.Revoked, p.RevokeReason, p.LogoutDone = &now, "revoked", false
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

// listenerGone fails the test unless the listener account lp is closed, out of the room and holds
// no token.
func (f *fixture) listenerGone(lp, when string) {
	f.t.Helper()
	a := f.state().Agents[lp]
	if a == nil || a.Closed == nil || f.hs.member(testRoom, mx(lp)) || f.hs.live(mx(lp)) != nil {
		f.t.Fatalf("%s: account %s %+v, in the room %v, live %v", when, lp, a, f.hs.member(testRoom, mx(lp)), f.hs.live(mx(lp)))
	}
}

// A revoke that lands after the record is written, while the account joins the room: the call
// logs nothing in (the revoked check after the join), and it closes the account before it answers
// (closeRevokedListeners when ensureListener fails), not only at the next job pass.
func TestRevokeDuringListenerJoinClosesTheAccountBeforeTheAnswer(t *testing.T) {
	f := newFixture(t)
	f.addOwner(wt.Owner)
	p := f.pair(wt.Owner, "", false)
	lp := f.listenerLP(p.id)
	f.hook("JoinAs", mx(lp), func() { f.markRevoked(p.id) })
	if code, _ := f.callRaw("POST", wire.PathStanding, p.key, struct{}{}, nil); code != 401 {
		t.Fatalf("POST /standing of a revoked pairing: %d", code)
	}
	if n := f.hs.callCount("Login " + mx(lp)); n != 0 {
		t.Fatalf("%d logins on the account of a revoked pairing", n)
	}
	f.listenerGone(lp, "when POST /standing answered")
}

// A revoke that lands after the account is ready, during the login: the confirmation sees it,
// undoes the login and closes the account before the call answers (closeRevokedListeners after a
// revoked confirmation), not only at the next job pass.
func TestRevokeDuringListenerLoginClosesTheAccountBeforeTheAnswer(t *testing.T) {
	f := newFixture(t)
	f.addOwner(wt.Owner)
	p := f.pair(wt.Owner, "", false)
	lp := f.listenerLP(p.id)
	f.hook("Login", mx(lp), func() { f.markRevoked(p.id) })
	if code, _ := f.callRaw("POST", wire.PathStanding, p.key, struct{}{}, nil); code != 401 {
		t.Fatalf("POST /standing of a revoked pairing: %d", code)
	}
	if f.state().Pairings[p.id].Confirmed != nil {
		t.Fatal("the revoked pairing was confirmed")
	}
	f.listenerGone(lp, "when POST /standing answered")
}

// The jobs close a listener account still held by a revoked pairing: here the registrar stopped
// after a re-pair's confirmation revoked the old pairing, before that pairing's account was closed.
func TestJobsCloseTheAccountOfAReplacedPairing(t *testing.T) {
	f := newFixture(t)
	f.addOwner(wt.Owner)
	f.seedLegacy(wt.PairingID, wt.Owner, wt.OwnerKey, false)
	np := f.pair(wt.Owner, wt.OwnerKey, false)
	if c := f.callJSON("POST", wire.PathStanding, wt.OwnerKey, struct{}{}, nil); c != 200 {
		t.Fatalf("old pairing's standing: %d %s", c, f.last)
	}
	old := f.listenerLP(wt.PairingID)
	if old == f.listenerLP(np.id) {
		t.Fatal("setup: the re-pair took the old pairing's name")
	}
	// The confirmation, and then a stop before replaced() ran.
	if err := f.s.Store.Update(func(st *State) error {
		f.s.confirmLocked(st, np.id, st.Pairings[np.id], f.clock.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	f.s.runJobs(t.Context())
	a := f.state().Agents[old]
	if a.Closed == nil || a.Pairing != wt.PairingID || f.hs.member(testRoom, mx(old)) || f.hs.live(mx(old)) != nil {
		t.Fatalf("old pairing's account after the restart: %+v, live %v", a, f.hs.live(mx(old)))
	}
}

// A re-pair P of an old pairing O gets a fresh name; O gets its own account at its POST /standing
// before P is confirmed. P's confirmation closes O's account (it does not move to P), and P keeps
// its own.
func TestRepairLeftoverListenerClosed(t *testing.T) {
	f := newFixture(t)
	f.addOwner(wt.Owner)
	f.seedLegacy(wt.PairingID, wt.Owner, wt.OwnerKey, false)
	np := f.pair(wt.Owner, wt.OwnerKey, false)
	if c := f.callJSON("POST", wire.PathStanding, wt.OwnerKey, struct{}{}, nil); c != 200 {
		t.Fatalf("old pairing's standing: %d %s", c, f.last)
	}
	old := f.listenerLP(wt.PairingID)
	var sr wire.Standing
	if c := f.callJSON("POST", wire.PathStanding, np.key, struct{}{}, &sr); c != 200 {
		t.Fatalf("re-pair's standing: %d %s", c, f.last)
	}
	st := f.state()
	if a := st.Agents[old]; a.Closed == nil || a.Pairing != wt.PairingID || f.hs.member(testRoom, mx(old)) || f.hs.live(mx(old)) != nil {
		t.Fatalf("old pairing's account after the re-pair: %+v, live %v", a, f.hs.live(mx(old)))
	}
	if sr.UserID == mx(old) || sr.UserID != f.listenerUID(np.id) || !st.hasOwnListener(np.id) {
		t.Fatalf("re-pair's account %+v", sr)
	}
}

// A word pair remembered only by an agent record (a clash record, or a pairing long pruned) is
// never handed out again.
func TestPairUsedCountsAgentRecords(t *testing.T) {
	now := time.Now()
	st := &State{Pairings: map[string]*Pairing{}, Agents: map[string]*Agent{
		wire.ListenerLocalpart(wt.Owner, "brave-heron", false): {Owner: wt.Owner, Kind: wire.KindStanding, Closed: &now, CloseReason: "clash"},
		wire.ListenerLocalpart(wt.Owner, "calm-otter", true):   {Owner: wt.Owner, Kind: wire.KindStanding},
	}}
	for _, pair := range []string{"brave-heron", "calm-otter"} {
		if !st.pairUsed(wt.Owner, pair) {
			t.Errorf("%s counts as unused", pair)
		}
	}
	if st.pairUsed(wt.Owner, "quick-horse") || st.pairUsed("other", "brave-heron") {
		t.Error("an unused pair counts as used")
	}
}

// hasOwnListener reports whether pairing id holds an open listener account of its own.
func (st *State) hasOwnListener(id string) bool {
	p := st.Pairings[id]
	if p == nil || p.Listener == "" {
		return false
	}
	a := st.Agents[p.Listener]
	return a != nil && a.Pairing == id && a.Open()
}
