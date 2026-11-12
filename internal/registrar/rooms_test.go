package registrar

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
	wt "github.com/fcanela/kwak/internal/wire/wiretest"
)

// Side rooms and test pairings: decisions 39 and 40, spec 3.1, 3.4, 3.5, 3.6, 3.10 and 3.11.

const claudeSession2 = "0718a1b2-c3d4-e5f6-8a9b-0c1d2e3f4a5b"

func TestConfigSideRooms(t *testing.T) {
	good := testConfig(t.TempDir())
	good.ReportRoom, good.TestRoom, good.TestReportRoom = reportAlias, testRoomAlias, testReportAlias
	if err := good.Check(); err != nil {
		t.Fatalf("three side rooms: %v", err)
	}
	none := testConfig(t.TempDir())
	if err := none.Check(); err != nil {
		t.Fatalf("no side room: %v", err)
	}
	bad := []struct {
		name string
		set  func(c *Config)
		want string
	}{
		{"not an alias", func(c *Config) { c.ReportRoom = "!roomid:" + testServer }, "report_room"},
		{"another server", func(c *Config) { c.TestRoom = "#agents-test:elsewhere.org" }, "test_room"},
		{"no name", func(c *Config) { c.TestReportRoom = "#:" + testServer }, "test_report_room"},
		{"the agents room again", func(c *Config) { c.ReportRoom = testAlias }, "is also room"},
		{"two side rooms the same", func(c *Config) { c.TestRoom, c.TestReportRoom = testRoomAlias, testRoomAlias }, "is also test_room"},
		// Test rooms by name (decision 41): the two test rooms end in "-test", the two others do not.
		{"test room on the agents room", func(c *Config) { c.TestRoom = testAlias }, `test_room "#agents:kwak.test" must end in "-test"`},
		{"test room without -test", func(c *Config) { c.TestRoom = "#agents-tests:" + testServer }, "test_room"},
		{"test report room on the report room", func(c *Config) { c.TestReportRoom = reportAlias }, `test_report_room "#agents-report:kwak.test" must end in "-test"`},
		{"agents room named as a test room", func(c *Config) { c.Room = testRoomAlias }, `room "#agents-test:kwak.test" must not end in "-test"`},
		{"report room named as a test room", func(c *Config) { c.ReportRoom = testReportAlias }, `report_room "#agents-report-test:kwak.test" must not end in "-test"`},
	}
	for _, b := range bad {
		c := testConfig(t.TempDir())
		b.set(&c)
		if err := c.Check(); err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%s: %v, want an error naming %q", b.name, err, b.want)
		}
	}
}

// The "-test" rule reads the alias's local part only: a server name that ends in "-test" makes no
// room a test room (decision 41).
func TestTestRoomName(t *testing.T) {
	for alias, want := range map[string]bool{
		"#agents-test:kwak.example.org":        true,
		"#agents-report-test:kwak.example.org": true,
		"#agents:kwak.example.org":             false,
		"#agents:kwak-test":                    false,
		"#agents-tests:kwak.example.org":       false,
		"#agents-test":                         true,
	} {
		if got := testName(alias); got != want {
			t.Errorf("testName(%q) = %v, want %v", alias, got, want)
		}
	}
	c := testConfig(t.TempDir())
	c.ServerName, c.Room, c.Admins = "kwak-test", "#agents:kwak-test", []string{"@admin:kwak-test"}
	if err := c.Check(); err != nil {
		t.Fatalf("a server name ending in -test: %v", err)
	}
}

// LoadConfig stays strict: the three new keys load, a misspelt one stops the registrar.
func TestLoadConfigSideRoomKeys(t *testing.T) {
	dir := t.TempDir()
	reg, err := wire.GenRegistration("http://kwak-registrar:8009", testServer, newFixedRand())
	if err != nil {
		t.Fatal(err)
	}
	regPath := filepath.Join(dir, "kwak.yaml")
	if err := os.WriteFile(regPath, reg, 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(extra string) string {
		p := filepath.Join(dir, "config.json")
		body := `{"server_name":"` + testServer + `","homeserver":"http://tuwunel:8008","registration":"` + regPath +
			`","state":"` + filepath.Join(dir, "state.json") + `","room":"` + testAlias + `"` + extra + `}`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, _, err := LoadConfig(write(`,"report_room":"` + reportAlias + `","test_room":"` + testRoomAlias +
		`","test_report_room":"` + testReportAlias + `"`))
	if err != nil || c.ReportRoom != reportAlias || c.TestRoom != testRoomAlias || c.TestReportRoom != testReportAlias {
		t.Fatalf("side keys: %+v %v", c, err)
	}
	if _, _, err := LoadConfig(write(`,"reports_room":"` + reportAlias + `"`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("misspelt key: %v", err)
	}
}

// A side room that does not exist yet never stops the registrar: Start returns, the agents room
// works, and the side room is tried again with a growing wait until it exists.
func TestMissingSideRoomDoesNotBlockStart(t *testing.T) {
	f := newFixture(t)
	f.s.Cfg.ReportRoom = reportAlias
	f.s.smu.Lock()
	f.s.initSide()
	f.s.smu.Unlock()
	f.s.joinSideRooms(t.Context(), true)
	if id, ok := f.s.sideID(nil, sideReport); !ok || id != "" {
		t.Fatalf("report room %q %v before it exists", id, ok)
	}
	resolves := func() int { return f.hs.callCount("ResolveAlias " + reportAlias) }
	n := resolves()
	f.s.joinSideRooms(t.Context(), false)
	if resolves() != n {
		t.Fatal("tried again before the wait")
	}
	f.clock.Advance(roomRetry)
	f.s.joinSideRooms(t.Context(), false)
	if resolves() != n+1 {
		t.Fatal("not tried after the wait")
	}
	f.clock.Advance(roomRetry) // the wait doubled to 10 s
	f.s.joinSideRooms(t.Context(), false)
	if resolves() != n+1 {
		t.Fatal("the wait did not grow")
	}
	f.hs.addRoom(reportAlias, reportRoom, false)
	f.clock.Advance(roomRetry)
	f.s.joinSideRooms(t.Context(), false)
	if id, _ := f.s.sideID(nil, sideReport); id != reportRoom || !f.hs.member(reportRoom, mx("registrar")) {
		t.Fatalf("report room %q not joined once it exists", id)
	}
	// The agents room kept working all along.
	f.addOwner("fcanela")
	p := f.pair("fcanela", "", true)
	if a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, ""); a.RoomID != testRoom {
		t.Fatalf("agent room %s", a.RoomID)
	}
}

// A report room in the config that is not there yet never keeps a closed agent in the agents room:
// the leave of the room of its kind comes first, and the cleanup finishes (decision 6).
func TestCloseWithAReportRoomNotThereYet(t *testing.T) {
	f := newFixture(t)
	f.s.Cfg.ReportRoom = reportAlias
	f.s.smu.Lock()
	f.s.initSide()
	f.s.smu.Unlock()
	f.addOwner("fcanela")
	p := f.pair("fcanela", "", true)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	f.mustClose(p.key, a.UserID)
	if f.hs.member(testRoom, a.UserID) {
		t.Fatal("closed agent still in the agents room")
	}
	if rec := f.state().Agents[claudeLP]; rec == nil || !rec.CleanupDone {
		t.Fatalf("cleanup not done: %+v", rec)
	}
}

// RunJobs (through Start) joins the configured side rooms; a later try is joinSideRooms on a 5 s
// ticker, whose waits TestMissingSideRoomDoesNotBlockStart covers.
func TestRunJobsJoinsSideRooms(t *testing.T) {
	f := newFixture(t)
	f.s.Cfg.TestRoom = testRoomAlias
	f.s.Cfg.JobsEvery = Duration(time.Hour)
	f.s.smu.Lock()
	f.s.initSide()
	f.s.smu.Unlock()
	f.hs.addRoom(testRoomAlias, testRoomID, false)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { done <- f.s.RunJobs(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !f.hs.member(testRoomID, mx("registrar")) {
		if time.Now().After(deadline) {
			t.Fatal("test room never joined")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTestPairing(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(false)
	f.addOwner("fcanela")
	p, reply, cr := f.pairWith("fcanela", "", "pair test", true)
	// The test pairing's own listener account (decision 42): agent.fcanela.<pair>-test.listener.
	testStandingLP := f.listenerLP(p.id)
	n, err := wire.ParseLocalpart(testStandingLP)
	if err != nil || n.Pair == "" || !n.Test || !strings.HasSuffix(testStandingLP, "-test.listener") {
		t.Fatalf("test listener name %q: %+v %v", testStandingLP, n, err)
	}
	if !strings.Contains(reply, " as "+n.Pair+"-test (test pairing "+p.id+"), ") || !strings.Contains(reply, "This is a test pairing") ||
		!strings.Contains(reply, "its listener account "+testStandingLP+" join only") ||
		!strings.Contains(reply, testRoomAlias) || !strings.Contains(reply, testReportAlias) || !strings.Contains(reply, "never "+testAlias) {
		t.Fatalf("bot reply %q", reply)
	}
	if !cr.Test || cr.RoomID != testRoomID || cr.StandingUser != mx(testStandingLP) {
		t.Fatalf("claim %+v", cr)
	}
	st := f.state()
	if pr := st.Pairings[p.id]; pr == nil || !pr.Test {
		t.Fatalf("pairing %+v", pr)
	}
	if a := st.Agents[testStandingLP]; a == nil || a.Kind != wire.KindStanding || a.Owner != "fcanela" || a.Pairing != p.id || !a.Open() {
		t.Fatalf("test standing record %+v", a)
	}
	if !f.hs.member(testRoomID, mx(testStandingLP)) || f.hs.member(testRoom, mx(testStandingLP)) {
		t.Fatal("the test standing account must be in the test room only")
	}
	for lp, a := range st.Agents {
		if a.Kind == wire.KindStanding && lp != testStandingLP {
			t.Fatalf("a test pairing made another standing account: %s", lp)
		}
	}

	var sr wire.Standing
	if c := f.callJSON("POST", wire.PathStanding, p.key, struct{}{}, &sr); c != 200 ||
		sr.UserID != mx(testStandingLP) || sr.DeviceID != "KWAK-"+p.id {
		t.Fatalf("standing %d %+v", c, sr)
	}
	var me wire.Me
	if c := f.callJSON("GET", wire.PathMe, p.key, nil, &me); c != 200 || !me.Test {
		t.Fatalf("me %d %+v", c, me)
	}

	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	h := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "tests")
	for _, uid := range []string{a.UserID, h.UserID} {
		if !f.hs.member(testRoomID, uid) || f.hs.member(testRoom, uid) {
			t.Fatalf("%s must be in the test room only", uid)
		}
	}
	if a.RoomID != testRoomID || h.RoomID != testRoomID {
		t.Fatalf("room ids %s %s", a.RoomID, h.RoomID)
	}
	f.mustClose(p.key, a.UserID)
	if f.hs.member(testRoomID, a.UserID) || f.hs.member(testRoomID, h.UserID) {
		t.Fatal("closed agents still in the test room")
	}
	if rec := f.state().Agents[claudeLP]; rec == nil || !rec.CleanupDone {
		t.Fatalf("cleanup not done: %+v", rec)
	}
	// A closed test agent reopens into the test room again.
	if r := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, ""); !r.Reopened || !f.hs.member(testRoomID, r.UserID) || f.hs.member(testRoom, r.UserID) {
		t.Fatalf("reopen %+v", r)
	}

	agents := f.say(mx("fcanela"), f.dm(mx("fcanela")), "agents")
	if len(agents) != 1 || !strings.Contains(agents[0], "(laptop, test): "+claudeLP) {
		t.Fatalf("agents %q", agents)
	}
	machines := f.say(mx("fcanela"), f.dm(mx("fcanela")), "machines")
	if len(machines) != 1 || !strings.Contains(machines[0], p.id+" [test]: laptop (linux) as "+n.Pair+"-test, created") {
		t.Fatalf("machines %q", machines)
	}
	help := f.say(mx("fcanela"), f.dm(mx("fcanela")), "help")
	if len(help) != 1 || !strings.Contains(help[0], "pair test <CODE>") {
		t.Fatalf("help %q", help)
	}
}

// A normal and a test pairing of one owner side by side: two standing accounts, each in its own
// room, each pairing's agents in its own room.
func TestNormalAndTestPairingsSideBySide(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(false)
	f.addOwner("fcanela")
	normal := f.pair("fcanela", "", true)
	test, _, _ := f.pairWith("fcanela", "", "pair test", true)
	na := f.mustAgent(normal.key, wire.HarnessClaude, wt.ClaudeSession, "")
	ta := f.mustAgent(test.key, wire.HarnessClaude, claudeSession2, "")
	if !f.hs.member(testRoom, na.UserID) || f.hs.member(testRoomID, na.UserID) {
		t.Fatal("normal agent not in the agents room only")
	}
	if !f.hs.member(testRoomID, ta.UserID) || f.hs.member(testRoom, ta.UserID) {
		t.Fatal("test agent not in the test room only")
	}
	normalLP, testStandingLP := f.listenerLP(normal.id), f.listenerLP(test.id)
	if normalLP == testStandingLP || strings.TrimSuffix(normalLP, ".listener") == strings.TrimSuffix(testStandingLP, "-test.listener") {
		t.Fatalf("one owner's two pairings share a word pair: %s, %s", normalLP, testStandingLP)
	}
	if !f.hs.member(testRoom, mx(normalLP)) || f.hs.member(testRoomID, mx(normalLP)) {
		t.Fatal("normal standing account not in the agents room only")
	}
	if !f.hs.member(testRoomID, mx(testStandingLP)) || f.hs.member(testRoom, mx(testStandingLP)) {
		t.Fatal("test standing account not in the test room only")
	}
	// The same session cannot be minted by the other pairing.
	if code, _, e := f.agent(test.key, wire.HarnessClaude, wt.ClaudeSession, ""); code != 403 || e.Errcode != wire.ErrNotYours {
		t.Fatalf("session of the normal pairing from the test pairing: %d %+v", code, e)
	}
	// Revoking the test pairing logs out the device of the test standing account only.
	out := f.say(mx("fcanela"), f.dm(mx("fcanela")), "revoke "+test.id)
	if len(out) != 1 || !strings.HasPrefix(out[0], "Revoked pairing") {
		t.Fatalf("revoke %q", out)
	}
	if f.hs.callCount("LogoutDevice "+mx(testStandingLP)+"/KWAK-"+test.id) == 0 {
		t.Fatalf("test standing device not logged out: %v", f.hs.callLog())
	}
	if f.hs.callCount("LogoutDevice "+mx(normalLP)+"/KWAK-"+test.id) != 0 {
		t.Fatal("logged out a device of the normal standing account")
	}
	if !f.state().Pairings[test.id].LogoutDone || f.hs.member(testRoomID, ta.UserID) {
		t.Fatal("test pairing not cleaned up")
	}
	// The revoke closes the test pairing's own listener account (decision 42): it leaves the test
	// room and has no live token. The normal one stays.
	if a := f.state().Agents[testStandingLP]; a.Closed == nil || !a.CleanupDone || f.hs.member(testRoomID, mx(testStandingLP)) ||
		f.hs.live(mx(testStandingLP)) != nil {
		t.Fatalf("test listener account after the revoke: %+v", a)
	}
	if a := f.state().Agents[normalLP]; !a.Open() || f.hs.live(mx(normalLP)) == nil {
		t.Fatalf("normal listener account after the test revoke: %+v", a)
	}
	if f.state().Agents[na.UserID[1:strings.Index(na.UserID, ":")]].Closed != nil {
		t.Fatal("the normal pairing's agent was closed")
	}
}

func TestPairTestNeedsATestRoom(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	pr := f.startPair()
	out := f.say(mx("fcanela"), dm, "pair test "+pr.Code)
	if len(out) != 1 || !strings.Contains(out[0], "Test pairings are off on this server") {
		t.Fatalf("reply %q", out)
	}
	// The code was not used up: a normal pairing still works with it.
	out = f.say(mx("fcanela"), dm, "pair "+pr.Code)
	if len(out) != 1 || !strings.HasPrefix(out[0], "Paired machine 'laptop' (linux) as ") || !strings.Contains(out[0], " (pairing ") {
		t.Fatalf("normal pair after a refused test pair: %q", out)
	}
}

// A test room in the config that does not exist yet: `pair test` fails and says to try again, and
// works once the room exists. A test pairing never falls back to the agents room.
func TestPairTestBeforeTheTestRoomExists(t *testing.T) {
	f := newFixture(t)
	f.s.Cfg.TestRoom = testRoomAlias
	f.s.smu.Lock()
	f.s.initSide()
	f.s.smu.Unlock()
	dm := f.addOwner("fcanela")
	pr := f.startPair()
	out := f.say(mx("fcanela"), dm, "pair test "+pr.Code)
	if len(out) != 1 || !strings.Contains(out[0], "That failed: the test room is not joined yet. Try again.") {
		t.Fatalf("reply %q", out)
	}
	if len(f.state().Pairings) != 0 {
		t.Fatal("a refused pair test made a pairing")
	}
	f.hs.addRoom(testRoomAlias, testRoomID, false)
	out = f.say(mx("fcanela"), dm, "pair test "+pr.Code)
	if len(out) != 1 || !strings.Contains(out[0], " (test pairing ") {
		t.Fatalf("second try %q", out)
	}
	var cr wire.ClaimResponse
	if c := f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, &cr); c != 200 {
		t.Fatalf("claim: %d", c)
	}
	if c := f.callJSON("POST", wire.PathStanding, cr.OwnerKey, struct{}{}, nil); c != 200 {
		t.Fatalf("standing: %d", c)
	}
	testStandingLP := f.listenerLP(cr.PairingID)
	if !f.hs.member(testRoomID, mx(testStandingLP)) || f.hs.member(testRoom, mx(testStandingLP)) {
		t.Fatal("test standing account not in the test room only")
	}
}

// "pair test" from a config holding a normal pairing (or "pair" from a test one) is a new pairing
// of the asked kind (decision 41): the old pairing is not moved and not revoked, so its agents stay
// in their room (decision 39), and the reply says in one line that it stays live and how to end it.
func TestRepairAcrossKindsIsANewPairing(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(false)
	f.addOwner("fcanela")
	old := f.pair("fcanela", "", true)
	a := f.mustAgent(old.key, wire.HarnessClaude, wt.ClaudeSession, "")
	p, reply, _ := f.pairWith("fcanela", old.key, "pair test", true)
	line := "The earlier pairing " + old.id + " stays live until you revoke it or it expires; to end it, send: revoke " + old.id
	if !strings.Contains(reply, " (test pairing "+p.id+"), ") || !strings.HasSuffix(reply, "\n"+line) {
		t.Fatalf("reply %q", reply)
	}
	// A pairing of the other kind is a new pairing with a new listener account (decision 42).
	if f.listenerLP(p.id) == f.listenerLP(old.id) || !f.state().Agents[f.listenerLP(old.id)].Open() {
		t.Fatalf("cross-kind re-pair took the old listener account: %s", f.listenerLP(p.id))
	}
	st := f.state()
	if st.Pairings[p.id].Replaces != "" || st.Pairings[old.id].Revoked != nil {
		t.Fatalf("old pairing touched: new %+v old %+v", st.Pairings[p.id], st.Pairings[old.id])
	}
	if rec := st.Agents[claudeLP]; rec.Pairing != old.id || rec.Closed != nil || !f.hs.member(testRoom, a.UserID) {
		t.Fatalf("agent moved: %+v", rec)
	}
	// And the other way: a normal pairing sent with a test key replaces nothing.
	q, reply, _ := f.pairWith("fcanela", p.key, "pair", true)
	if !strings.Contains(reply, "The earlier pairing "+p.id+" stays live") || f.state().Pairings[q.id].Replaces != "" ||
		f.state().Pairings[p.id].Revoked != nil || f.state().Pairings[q.id].Test {
		t.Fatalf("normal pair with a test key: %q", reply)
	}
}

// TestConfirmKeepsAPairingOfTheOtherKind checks the kind guard in confirmReplace on its own: a
// state.json pairing whose Replaces names a pairing of the other kind (which bindLocked never
// writes) confirms without moving or revoking the old one (decision 41). The same-kind row shows
// the confirm path does reach confirmReplace.
func TestConfirmKeepsAPairingOfTheOtherKind(t *testing.T) {
	for _, tc := range []struct {
		name             string
		oldTest, newTest bool
		wantReplaced     bool
	}{
		{"normal replaces test", true, false, false},
		{"test replaces normal", false, true, false},
		{"same kind replaces", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.sideRooms(false)
			f.addOwner(wt.Owner)
			f.seedKind(wt.OldPairingID, wt.Owner, wt.OldOwnerKey, true, tc.oldTest)
			f.mustAgent(wt.OldOwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
			f.seedKind(wt.PairingID, wt.Owner, wt.OwnerKey, false, tc.newTest)
			if err := f.s.Store.Update(func(st *State) error {
				st.Pairings[wt.PairingID].Replaces = wt.OldPairingID
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if c := f.callJSON("POST", wire.PathStanding, wt.OwnerKey, struct{}{}, nil); c != 200 {
				t.Fatalf("standing: %d", c)
			}
			st := f.state()
			if st.Pairings[wt.PairingID].Confirmed == nil {
				t.Fatal("new pairing not confirmed")
			}
			old, rec := st.Pairings[wt.OldPairingID], st.Agents[claudeLP]
			if tc.wantReplaced {
				if old.Revoked == nil || old.RevokeReason != "replaced" || rec.Pairing != wt.PairingID {
					t.Fatalf("same kind not replaced: old %+v agent %+v", old, rec)
				}
				return
			}
			if old.Revoked != nil || rec.Pairing != wt.OldPairingID || rec.Closed != nil {
				t.Fatalf("other kind touched: old %+v agent %+v", old, rec)
			}
			if c := f.callJSON("POST", wire.PathStanding, wt.OldOwnerKey, struct{}{}, nil); c != 200 {
				t.Fatalf("old key after the confirm: %d", c)
			}
		})
	}
}

func TestReportJoin(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(false)
	f.addOwner("fcanela")
	normal := f.pair("fcanela", "", true)
	test, _, _ := f.pairWith("fcanela", "", "pair test", true)
	na := f.mustAgent(normal.key, wire.HarnessClaude, wt.ClaudeSession, "")
	ta := f.mustAgent(test.key, wire.HarnessClaude, claudeSession2, "")

	join := func(key, uid string) (int, wire.ReportJoin, wire.APIError) {
		t.Helper()
		var out wire.ReportJoin
		var e wire.APIError
		code, body := f.callRaw("POST", wire.PathReportJoin, key, wire.ReportJoinRequest{UserID: uid}, &out)
		if code != 200 {
			json.Unmarshal(body, &e)
		}
		return code, out, e
	}
	normalLP, testStandingLP := f.listenerLP(normal.id), f.listenerLP(test.id)
	other := f.pair("fcanela", "", true) // a second normal pairing of the same owner
	ok := []struct {
		key, uid, room string
	}{
		{normal.key, na.UserID, reportRoom},
		{normal.key, mx(normalLP), reportRoom},
		{test.key, ta.UserID, testReportID},
		{test.key, mx(testStandingLP), testReportID},
	}
	for _, c := range ok {
		code, out, e := join(c.key, c.uid)
		if code != 200 || out.RoomID != c.room || !f.hs.member(c.room, c.uid) {
			t.Errorf("%s: %d %+v %+v", c.uid, code, out, e)
		}
	}
	if f.hs.member(reportRoom, ta.UserID) || f.hs.member(reportRoom, mx(testStandingLP)) {
		t.Fatal("a test account reached the report room")
	}

	refused := []struct {
		name, key, uid string
	}{
		{"another pairing's agent", normal.key, ta.UserID},
		{"the test standing account from the normal pairing", normal.key, mx(testStandingLP)},
		{"the normal standing account from the test pairing", test.key, mx(normalLP)},
		{"another normal pairing's listener account", other.key, mx(normalLP)},
		{"the old per-owner standing account", normal.key, wt.LegacyStanding},
		{"an unknown agent", normal.key, wt.OtherAgent},
		{"a person", normal.key, mx("fcanela")},
		{"the bot", normal.key, mx("registrar")},
		{"another server", normal.key, "@" + normalLP + ":elsewhere.org"},
	}
	for _, c := range refused {
		if code, _, e := join(c.key, c.uid); code != 403 || e.Errcode != wire.ErrNotYours {
			t.Errorf("%s: %d %+v", c.name, code, e)
		}
	}
	f.mustClose(normal.key, na.UserID)
	if code, _, e := join(normal.key, na.UserID); code != 403 || e.Errcode != wire.ErrNotYours {
		t.Errorf("closed agent: %d %+v", code, e)
	}
	if f.hs.member(reportRoom, na.UserID) {
		t.Error("closing the agent did not leave the report room")
	}
}

// An invite-only report room: the registrar invites the account, then joins it.
func TestReportJoinInviteOnly(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(true)
	f.addOwner("fcanela")
	p := f.pair("fcanela", "", true)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	var out wire.ReportJoin
	if c := f.callJSON("POST", wire.PathReportJoin, p.key, wire.ReportJoinRequest{UserID: a.UserID}, &out); c != 200 || out.RoomID != reportRoom {
		t.Fatalf("join %d %s", c, f.last)
	}
	if !f.hs.member(reportRoom, a.UserID) || f.hs.callCount("Invite "+a.UserID) != 1 {
		t.Fatalf("not invited then joined: %v", f.hs.callLog())
	}
	// A second join needs no invite: the account is a member.
	f.callJSON("POST", wire.PathReportJoin, p.key, wire.ReportJoinRequest{UserID: a.UserID}, &out)
	if f.hs.callCount("Invite "+a.UserID) != 1 {
		t.Fatal("invited a member again")
	}
}

// Without @registrar in an invite-only report room, the invite fails and the route says so.
func TestReportJoinBotNotInTheRoom(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(false)
	f.hs.mu.Lock()
	f.hs.rooms[reportRoom].inviteOnly = true
	delete(f.hs.rooms[reportRoom].members, mx("registrar"))
	f.hs.mu.Unlock()
	f.addOwner("fcanela")
	p := f.pair("fcanela", "", true)
	code, body := f.callRaw("POST", wire.PathReportJoin, p.key, wire.ReportJoinRequest{UserID: f.listenerUID(p.id)}, nil)
	if code != 502 || !strings.Contains(string(body), wire.ErrHomeserver) || !strings.Contains(string(body), matrix.ErrForbidden) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestReportJoinNoRoomForTheKind(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(false)
	f.s.Cfg.TestReportRoom = ""
	f.s.smu.Lock()
	delete(f.s.side, sideTestReport)
	f.s.smu.Unlock()
	f.addOwner("fcanela")
	test, _, _ := f.pairWith("fcanela", "", "pair test", true)
	code, body := f.callRaw("POST", wire.PathReportJoin, test.key, wire.ReportJoinRequest{UserID: f.listenerUID(test.id)}, nil)
	if code != 404 || !strings.Contains(string(body), wire.ErrNoReportRoom) {
		t.Fatalf("test pairing with no test report room: %d %s", code, body)
	}
	// A test pairing never falls back to the normal report room.
	if f.hs.member(reportRoom, f.listenerUID(test.id)) {
		t.Fatal("joined the normal report room")
	}
}

// A report room in the config that does not exist yet answers 502, tries again at each request,
// and works once the room exists.
func TestReportJoinRoomNotThereYet(t *testing.T) {
	f := newFixture(t)
	f.s.Cfg.ReportRoom = reportAlias
	f.s.smu.Lock()
	f.s.initSide()
	f.s.smu.Unlock()
	f.addOwner("fcanela")
	p := f.pair("fcanela", "", true)
	req := wire.ReportJoinRequest{UserID: f.listenerUID(p.id)}
	code, body := f.callRaw("POST", wire.PathReportJoin, p.key, req, nil)
	if code != 502 || !strings.Contains(string(body), matrix.ErrNotFound) {
		t.Fatalf("%d %s", code, body)
	}
	f.hs.addRoom(reportAlias, reportRoom, false)
	var out wire.ReportJoin
	if c := f.callJSON("POST", wire.PathReportJoin, p.key, req, &out); c != 200 || out.RoomID != reportRoom {
		t.Fatalf("after the room exists: %d %s", c, f.last)
	}
}

// An invite of @registrar to an invite-only side room, from an admin with no recorded DM, joins it
// as the side room and is never taken for that admin's DM (spec 3.3 step 2).
func TestInviteToSideRoomIsNotADM(t *testing.T) {
	f := newFixture(t)
	f.s.Cfg.ReportRoom = reportAlias
	f.s.smu.Lock()
	f.s.initSide()
	f.s.smu.Unlock()
	f.hs.addRoom(reportAlias, reportRoom, true)
	f.hs.join(reportRoom, testAdmin)
	f.s.joinSideRooms(t.Context(), true)
	if id, _ := f.s.sideID(nil, sideReport); id != "" {
		t.Fatal("joined an invite-only room with no invite")
	}
	f.s.Store.Update(func(st *State) error { delete(st.DMs, testAdmin); return nil })
	f.hs.mu.Lock()
	f.hs.rooms[reportRoom].invited = map[string]bool{mx("registrar"): true}
	f.hs.mu.Unlock()
	bot := mx("registrar")
	f.txn(f.event(testAdmin, reportRoom, "m.room.member", &bot, map[string]string{"membership": "invite"}))
	if id, _ := f.s.sideID(nil, sideReport); id != reportRoom {
		t.Fatal("the invite did not join the side room")
	}
	if dm := f.dm(testAdmin); dm == reportRoom {
		t.Fatal("the report room was recorded as a DM")
	}
}

// Rollback (docs/deploy.md): a state.json written with no test pairing has no new key, so an older
// image still reads it; the first test pairing adds "test" to that pairing only.
func TestStateShapeForRollback(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(false)
	f.addOwner("fcanela")
	p := f.pair("fcanela", "", true)
	f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	var report wire.ReportJoin
	f.callJSON("POST", wire.PathReportJoin, p.key, wire.ReportJoinRequest{UserID: f.listenerUID(p.id)}, &report)
	raw := string(mustRead(t, f.path))
	for _, k := range []string{`"test"`, reportRoom, testRoomID, testReportID} {
		if strings.Contains(raw, k) {
			t.Fatalf("state.json holds %s with no test pairing:\n%s", k, raw)
		}
	}
	test, _, _ := f.pairWith("fcanela", "", "pair test", true)
	var st struct {
		Pairings map[string]map[string]any `json:"pairings"`
	}
	if err := json.Unmarshal(mustRead(t, f.path), &st); err != nil {
		t.Fatal(err)
	}
	if st.Pairings[test.id]["test"] != true {
		t.Fatalf("test pairing %v", st.Pairings[test.id])
	}
	if _, ok := st.Pairings[p.id]["test"]; ok {
		t.Fatal("the normal pairing got a test key")
	}
}

// Owner removal closes the test standing account too, and it leaves the test room.
func TestOwnerRemoveClosesTheTestStanding(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(false)
	f.addOwner("fcanela")
	p, _, _ := f.pairWith("fcanela", "", "pair test", true)
	testStandingLP := f.listenerLP(p.id)
	f.removeOwner("fcanela")
	if a := f.state().Agents[testStandingLP]; a == nil || a.Closed == nil || !a.CleanupDone {
		t.Fatalf("test standing %+v", a)
	}
	if f.hs.member(testRoomID, mx(testStandingLP)) {
		t.Fatal("test standing account still in the test room")
	}
}

// POST /report/join holds the account's lock across the join: a close that starts during the join
// waits for it, and its cleanup then leaves the report room, so a closed agent never stays there.
func TestReportJoinHoldsTheLockAgainstAClose(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(false)
	f.addOwner("fcanela")
	p := f.pair("fcanela", "", true)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	var lk string
	f.s.Store.View(func(st *State) { lk = lockKey(st.Agents[claudeLP], claudeLP) })
	var wg sync.WaitGroup
	f.hook("JoinAs", a.UserID, func() {
		done := make(chan struct{})
		wg.Go(func() {
			defer close(done)
			if _, err := f.s.closeAgents(context.Background(), []string{claudeLP}, "session_ended", "", nil); err != nil {
				t.Error(err)
			}
		})
		f.waitBlocked(lk, 2, done)
	})
	var rj wire.ReportJoin
	f.callJSON("POST", wire.PathReportJoin, p.key, wire.ReportJoinRequest{UserID: a.UserID}, &rj)
	wg.Wait()
	if rec := f.state().Agents[claudeLP]; rec.Closed == nil {
		t.Fatalf("record %+v", rec)
	}
	if f.hs.member(reportRoom, a.UserID) {
		t.Fatal("a closed agent stayed in the report room")
	}
}

// POST /report/join checks the account again once it holds the lock: a close that finished while
// the request waited for the lock makes it 403 KWAK_NOT_YOURS, and the agent never joins.
func TestReportJoinChecksAgainUnderTheLock(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(false)
	f.addOwner("fcanela")
	p := f.pair("fcanela", "", true)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	var lk string
	f.s.Store.View(func(st *State) { lk = lockKey(st.Agents[claudeLP], claudeLP) })
	unlock := f.s.lock(lk) // a close in progress holds it
	type result struct {
		code int
		body []byte
	}
	res := make(chan result, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, b := f.callRaw("POST", wire.PathReportJoin, p.key, wire.ReportJoinRequest{UserID: a.UserID}, nil)
		res <- result{c, b}
	}()
	if got := f.waitBlocked(lk, 2, done); got != "blocked" {
		unlock()
		t.Fatalf("the join did not wait for the lock (%s)", got)
	}
	now := f.clock.Now()
	f.s.Store.Update(func(st *State) error {
		ag := st.Agents[claudeLP]
		ag.Closed, ag.CloseReason, ag.CleanupDone = &now, "session_ended", true
		return nil
	})
	unlock()
	r := <-res
	if r.code != 403 || !strings.Contains(string(r.body), wire.ErrNotYours) {
		t.Fatalf("join after the close: %d %s", r.code, r.body)
	}
	if f.hs.member(reportRoom, a.UserID) {
		t.Fatal("a closed agent joined the report room")
	}
}

// An invite of @registrar to the agents room, from an admin with no recorded DM, is not a DM: the
// bot stays in the agents room and records nothing (spec 3.3 step 2).
func TestInviteToTheAgentsRoomIsNotADM(t *testing.T) {
	f := newFixture(t)
	f.hs.join(testRoom, testAdmin)
	f.s.Store.Update(func(st *State) error { delete(st.DMs, testAdmin); return nil })
	bot := mx("registrar")
	f.txn(f.event(testAdmin, testRoom, "m.room.member", &bot, map[string]string{"membership": "invite"}))
	if !f.hs.member(testRoom, bot) || f.hs.callCount("LeaveAs "+bot) != 0 {
		t.Fatalf("the bot left the agents room: %v", f.hs.callLog())
	}
	var dm string
	f.s.Store.View(func(st *State) { dm = st.DMs[testAdmin] })
	if dm == testRoom {
		t.Fatal("the agents room was recorded as a DM")
	}
}

// joinOrInvite invites only when the room refuses the join with 403 (spec 3.11). Any other error
// (rate limit, server error, a network error) is returned as it is, with no invite.
func TestJoinOrInviteOnlyOn403(t *testing.T) {
	f := newFixture(t)
	f.sideRooms(true)
	uid := wt.StandingUser
	for _, e := range []error{mErr(429, "M_LIMIT_EXCEEDED"), mErr(500, "M_UNKNOWN"), context.DeadlineExceeded} {
		f.hs.mu.Lock()
		f.hs.failFor["JoinAs"] = map[string]error{uid: e}
		f.hs.mu.Unlock()
		if err := f.s.joinOrInvite(t.Context(), uid, reportRoom); !errors.Is(err, e) {
			t.Errorf("%v: got %v", e, err)
		}
		if n := f.hs.callCount("Invite " + uid); n != 0 {
			t.Fatalf("%v: %d invites", e, n)
		}
	}
	// A 403 invites, then joins.
	f.hs.mu.Lock()
	delete(f.hs.failFor, "JoinAs")
	f.hs.mu.Unlock()
	if err := f.s.joinOrInvite(t.Context(), uid, reportRoom); err != nil || f.hs.callCount("Invite "+uid) != 1 {
		t.Fatalf("403: %v, %v", err, f.hs.callLog())
	}
}
