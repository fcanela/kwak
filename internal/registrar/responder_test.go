package registrar

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/wire"
	wt "github.com/fcanela/kwak/internal/wire/wiretest"
)

// The responder account of a pairing (decision 46, spec 3.12).

func (f *fixture) responder(key string) (int, wire.Agent) {
	f.t.Helper()
	var a wire.Agent
	code := f.callJSON("POST", wire.PathResponder, key, struct{}{}, &a)
	return code, a
}

// POST /responder makes agent.<owner>.<pair>.responder, the pairing's listener name with
// "responder" at the end, named and joined to the agents room. A second call keeps the account and
// gives a new token, which logs the old one out. The account is never handed to POST /agents/close
// or alive, the idle job leaves it open, and a revoke closes it and takes it out of the room.
func TestResponderAccount(t *testing.T) {
	f := newFixture(t)
	f.seed()
	want := mx(wire.ResponderOf(f.listenerLP(wt.PairingID)))
	if want != "@agent.fcanela.brave-heron.responder:"+testServer {
		t.Fatalf("responder name %s", want)
	}
	code, a := f.responder(wt.OwnerKey)
	if code != 200 || a.UserID != want || !a.New || a.Reopened || a.RoomID != testRoom || a.AccessToken == "" {
		t.Fatalf("first call: %d %+v %s", code, a, f.last)
	}
	lp := wire.ResponderOf(f.listenerLP(wt.PairingID))
	rec := f.state().Agents[lp]
	if rec == nil || rec.Kind != wire.KindResponder || rec.Pairing != wt.PairingID || !rec.Open() || rec.Session != "" {
		t.Fatalf("record %+v", rec)
	}
	if !f.hs.member(testRoom, want) || f.hs.users[want].display != lp {
		t.Fatalf("member %v, display %q", f.hs.member(testRoom, want), f.hs.users[want].display)
	}

	code, b := f.responder(wt.OwnerKey)
	if code != 200 || b.UserID != want || b.New || b.Reopened || b.AccessToken == a.AccessToken {
		t.Fatalf("second call: %d %+v", code, b)
	}
	if live := f.hs.live(want); slices.Contains(live, a.AccessToken) || !slices.Contains(live, b.AccessToken) {
		t.Fatalf("live tokens %v", live)
	}

	if code, _ := f.callRaw("POST", wire.PathAgentsClose, wt.OwnerKey, wire.CloseRequest{UserIDs: []string{want}}, nil); code != 403 {
		t.Fatalf("close of the responder: %d", code)
	}
	var alive wire.AliveResponse
	f.callJSON("POST", wire.PathAgentsAlive, wt.OwnerKey, wire.AliveRequest{UserIDs: []string{want}}, &alive)
	if len(alive.Unknown) != 1 {
		t.Fatalf("alive %+v", alive)
	}
	var me wire.Me
	if f.callJSON("GET", wire.PathMe, wt.OwnerKey, nil, &me); me.AgentsOpen != 0 {
		t.Fatalf("GET /me counts the responder as an agent: %+v", me)
	}
	f.clock.Advance(f.s.Cfg.IdleAfter.D() + time.Hour)
	f.s.runJobs(t.Context())
	if !f.state().Agents[lp].Open() {
		t.Fatal("the idle job closed the responder")
	}

	if code := f.callJSON("POST", wire.PathUnpair, wt.OwnerKey, struct{}{}, nil); code != 200 {
		t.Fatalf("unpair: %d", code)
	}
	rec = f.state().Agents[lp]
	if rec.Closed == nil || f.hs.member(testRoom, want) || f.hs.live(want) != nil {
		t.Fatalf("after a revoke: %+v, member %v, live %v", rec, f.hs.member(testRoom, want), f.hs.live(want))
	}
	if code, _ := f.responder(wt.OwnerKey); code != 401 {
		t.Fatalf("a revoked key got a responder: %d", code)
	}
}

// A test pairing's responder is agent.<owner>.<pair>-test.responder and lives in the test room
// only (decisions 39, 40 and 46).
func TestResponderOfATestPairing(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(false)
	f.addOwner(wt.Owner)
	f.seedKind(wt.PairingID, wt.Owner, wt.OwnerKey, true, true)
	code, a := f.responder(wt.OwnerKey)
	if code != 200 || a.UserID != "@agent.fcanela.calm-otter-test.responder:"+testServer || a.RoomID != testRoomID {
		t.Fatalf("%d %+v %s", code, a, f.last)
	}
	if !f.hs.member(testRoomID, a.UserID) || f.hs.member(testRoom, a.UserID) {
		t.Fatal("a test responder must be in the test room only")
	}
}

// A pairing made before decision 42 has no listener name: POST /responder gives it one first, as
// POST /standing would, and names the responder after it.
func TestResponderOfAnOldPairing(t *testing.T) {
	f := newFixture(t)
	f.addOwner(wt.Owner)
	f.seedLegacy(wt.PairingID, wt.Owner, wt.OwnerKey, false)
	code, a := f.responder(wt.OwnerKey)
	if code != 200 || a.UserID != mx(wire.ResponderOf(f.listenerLP(wt.PairingID))) {
		t.Fatalf("%d %+v %s", code, a, f.last)
	}
}

// A same-kind re-pair takes the old pairing's listener name, and its confirmation moves the
// responder account along with it; the new key gets the same account.
func TestRepairMovesTheResponder(t *testing.T) {
	f := newFixture(t)
	f.addOwner(wt.Owner)
	old := f.pair(wt.Owner, "", true)
	_, a := f.responder(old.key)
	r := f.pair(wt.Owner, old.key, true)
	lp := wire.ResponderOf(f.listenerLP(r.id))
	if rec := f.state().Agents[lp]; mx(lp) != a.UserID || rec.Pairing != r.id || !rec.Open() {
		t.Fatalf("after the re-pair: %s %+v", lp, rec)
	}
	if code, b := f.responder(r.key); code != 200 || b.UserID != a.UserID {
		t.Fatalf("%d %+v", code, b)
	}
	if code, _ := f.responder(old.key); code != 401 {
		t.Fatalf("the replaced key: %d", code)
	}
}

// A failed join closes the new account again (setup_failed) and answers 502; the next call opens
// it again for its own live pairing.
func TestResponderSetupFailureThenReopen(t *testing.T) {
	f := newFixture(t)
	f.seed()
	lp := wire.ResponderOf(f.listenerLP(wt.PairingID))
	f.hs.failFor["JoinAs"] = map[string]error{mx(lp): mErr(500, "M_UNKNOWN")}
	if code, _ := f.responder(wt.OwnerKey); code != 502 {
		t.Fatalf("failed join: %d", code)
	}
	if rec := f.state().Agents[lp]; rec.Closed == nil {
		t.Fatalf("record after a failed join %+v", rec)
	}
	delete(f.hs.failFor, "JoinAs")
	code, a := f.responder(wt.OwnerKey)
	if code != 200 || !a.Reopened || !f.hs.member(testRoom, a.UserID) || !f.state().Agents[lp].Open() {
		t.Fatalf("reopen: %d %+v", code, a)
	}
}

// A revoke that lands while the account is made wins: nothing is answered and the account is
// closed again before the answer.
func TestRevokeDuringResponderJoin(t *testing.T) {
	f := newFixture(t)
	f.seed()
	lp := wire.ResponderOf(f.listenerLP(wt.PairingID))
	f.hook("JoinAs", mx(lp), func() { f.markRevoked(wt.PairingID) })
	if code, _ := f.responder(wt.OwnerKey); code != 401 {
		t.Fatalf("%d", code)
	}
	if rec := f.state().Agents[lp]; rec.Closed == nil || f.hs.member(testRoom, mx(lp)) || f.hs.live(mx(lp)) != nil {
		t.Fatalf("record %+v, member %v", rec, f.hs.member(testRoom, mx(lp)))
	}
}

// A re-pair whose listener name changed (the old name was given up) does not take the old
// responder along: the old account stays with the replaced pairing and closes with it, so no
// account of a name the new pairing never uses stays open.
func TestRepairWithANewNameClosesTheOldResponder(t *testing.T) {
	f := newFixture(t)
	f.addOwner(wt.Owner)
	old := f.pair(wt.Owner, "", true)
	_, a := f.responder(old.key)
	r := f.pair(wt.Owner, old.key, false)
	if err := f.s.Store.Update(func(st *State) error {
		lp, err := f.s.freshListener(st, wt.Owner, false)
		st.Pairings[r.id].Listener = lp
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code := f.callJSON("POST", wire.PathStanding, r.key, struct{}{}, nil); code != 200 {
		t.Fatalf("standing: %d %s", code, f.last)
	}
	lp := a.UserID[1 : len(a.UserID)-len(":"+testServer)]
	if rec := f.state().Agents[lp]; rec.Pairing != old.id || rec.Closed == nil || f.hs.member(testRoom, a.UserID) {
		t.Fatalf("old responder after the re-pair: %+v", rec)
	}
	if code, b := f.responder(r.key); code != 200 || b.UserID == a.UserID || b.UserID != mx(wire.ResponderOf(f.listenerLP(r.id))) {
		t.Fatalf("%d %+v", code, b)
	}
}

// A record under the responder's name that is not this pairing's responder (another pairing's,
// or another kind) is never handed out: 403 KWAK_NOT_YOURS, the record stays as it was and no
// token is made.
func TestResponderNameHeldByAnotherRecord(t *testing.T) {
	for name, held := range map[string]Agent{
		"another pairing": {Owner: wt.Owner, Kind: wire.KindResponder, Pairing: "0a0b0c0d"},
		"another kind":    {Owner: wt.Owner, Kind: wire.KindSession, Pairing: wt.PairingID, Session: "0123456789abcdef0123456789abcdef"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.seed()
			lp := wire.ResponderOf(f.listenerLP(wt.PairingID))
			if err := f.s.Store.Update(func(st *State) error {
				a := held
				st.putAgent(lp, &a)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if code, _ := f.responder(wt.OwnerKey); code != 403 || !strings.Contains(string(f.last), wire.ErrNotYours) {
				t.Fatalf("%d %s", code, f.last)
			}
			if rec := f.state().Agents[lp]; rec.Pairing != held.Pairing || rec.Kind != held.Kind || f.hs.live(mx(lp)) != nil {
				t.Fatalf("record %+v, live %v", rec, f.hs.live(mx(lp)))
			}
		})
	}
}
