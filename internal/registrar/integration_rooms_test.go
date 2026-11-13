//go:build integration

package registrar_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/registrar"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/test/integration/ittest"
)

// TestSideRoomsAgainstTuwunel runs decisions 39 and 40 against tuwunel: the admin makes the
// agents room, an invite-only report room, a test room and an invite-only test report room, and
// invites @registrar to the invite-only ones. A test pairing's standing account and agents join
// only the test room; POST /report/join invites and joins an agent to the test report room, the
// agent posts and leaves with its own token, and closing it later still counts as a clean leave
// of a room it already left (the doneErr answer of a real tuwunel, inferred before this test).
func TestSideRoomsAgainstTuwunel(t *testing.T) {
	env := ittest.Start(t)
	if env == nil {
		return
	}
	ctx := t.Context()
	server := env.ServerName
	bot := "@" + env.Registration.SenderLocalpart + ":" + server
	admin := env.Client(env.Admin.AccessToken)
	alice := env.Client(env.Alice.AccessToken)
	rooms := map[string]string{}
	for _, r := range []struct {
		alias      string
		inviteOnly bool
	}{{"agents", false}, {"agents-report", true}, {"agents-test", false}, {"agents-report-test", true}} {
		id, err := admin.CreateRoom(ctx, registrar.RoomBodyFor(bot, r.alias, r.inviteOnly))
		if err != nil {
			t.Fatalf("create %s: %v", r.alias, err)
		}
		rooms[r.alias] = id
	}

	cfg := registrar.Config{
		ServerName: server, Homeserver: env.Homeserver, Room: "#agents:" + server,
		ReportRoom: "#agents-report:" + server, TestRoom: "#agents-test:" + server,
		TestReportRoom: "#agents-report-test:" + server,
		Admins:         []string{env.Admin.UserID}, State: filepath.Join(t.TempDir(), "state.json"),
		HSToken: env.Registration.HSToken, BotLocalpart: env.Registration.SenderLocalpart,
		JobsEvery: registrar.Duration(time.Second), ClaimWait: registrar.Duration(20 * time.Second),
	}
	cfg.Defaults()
	if err := cfg.Check(); err != nil {
		t.Fatal(err)
	}
	store, err := registrar.OpenStore(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	srv := &registrar.Server{
		Cfg: cfg, Store: store, HS: registrar.NewHomeserver(env.Homeserver, env.Registration.ASToken, env.HTTP),
		Clock: registrar.SystemClock{}, Rand: rand.Reader, Log: slog.New(slog.NewTextHandler(t.Output(), nil)),
	}
	env.ServeAppservice(t, srv.Appservice())
	api := httptest.NewServer(srv.API())
	t.Cleanup(api.Close)
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	jctx, stop := context.WithCancel(ctx)
	jobsDone := make(chan struct{})
	go func() { srv.RunJobs(jctx); close(jobsDone) }()
	t.Cleanup(func() { stop(); <-jobsDone })

	members := func(room string) []string {
		t.Helper()
		m, err := admin.JoinedMembers(ctx, room)
		if err != nil {
			t.Fatalf("members of %s: %v", room, err)
		}
		return m
	}
	for alias, id := range rooms {
		if !slices.Contains(members(id), bot) {
			t.Fatalf("@registrar did not join %s", alias)
		}
	}

	dmOf := func(human string) string {
		var dm string
		store.View(func(st *registrar.State) { dm = st.DMs[human] })
		return dm
	}
	adminDM := waitFor(t, "the admin DM", func() (string, bool) { d := dmOf(env.Admin.UserID); return d, d != "" })
	if _, err := admin.Join(ctx, adminDM); err != nil {
		t.Fatalf("admin joins the DM: %v", err)
	}
	send(t, admin, adminDM, "owner add "+env.Alice.UserID)
	aliceDM := waitFor(t, "alice's DM", func() (string, bool) { d := dmOf(env.Alice.UserID); return d, d != "" })
	if _, err := alice.Join(ctx, aliceDM); err != nil {
		t.Fatalf("alice joins her DM: %v", err)
	}
	waitNotice(t, alice, aliceDM, "You are now a kwak owner as `alice`.")

	// A test pairing.
	var pr wire.PairResponse
	if c := callAPI(t, api.URL, "POST", wire.PathPair, "", wire.PairRequest{Machine: "it-test", OS: "linux", Version: wire.Version}, &pr); c != 201 {
		t.Fatalf("pair: %d", c)
	}
	send(t, alice, aliceDM, "pair test "+pr.Code)
	var cr wire.ClaimResponse
	if c := callAPI(t, api.URL, "POST", wire.PathPairClaim, pr.Claim, struct{}{}, &cr); c != 200 || cr.Status != wire.ClaimPaired {
		t.Fatalf("claim: %d %+v", c, cr)
	}
	// The test pairing's own listener account (decision 42): agent.alice.<pair>-test.listener.
	testStanding := cr.StandingUser
	m := regexp.MustCompile(`^@agent\.alice\.([a-z]+-[a-z]+)-test\.listener:`).FindStringSubmatch(testStanding)
	if !cr.Test || cr.RoomID != rooms["agents-test"] || m == nil {
		t.Fatalf("claim %+v", cr)
	}
	waitNotice(t, alice, aliceDM, "Paired machine 'it-test' (linux) as "+m[1]+"-test (test pairing "+cr.PairingID+"), ")
	var sr wire.Standing
	if c := callAPI(t, api.URL, "POST", wire.PathStanding, cr.OwnerKey, struct{}{}, &sr); c != 200 || sr.UserID != testStanding {
		t.Fatalf("standing: %d %+v", c, sr)
	}
	if !slices.Contains(members(rooms["agents-test"]), testStanding) || slices.Contains(members(rooms["agents"]), testStanding) {
		t.Fatal("the test standing account must be in the test room only")
	}

	// The test pairing's responder account (decision 46) lives in the test room only.
	var rs wire.Agent
	if c := callAPI(t, api.URL, "POST", wire.PathResponder, cr.OwnerKey, struct{}{}, &rs); c != 200 ||
		rs.UserID != "@agent.alice."+m[1]+"-test.responder:"+server || rs.RoomID != rooms["agents-test"] {
		t.Fatalf("responder: %d %+v", c, rs)
	}
	if !slices.Contains(members(rooms["agents-test"]), rs.UserID) || slices.Contains(members(rooms["agents"]), rs.UserID) {
		t.Fatal("the test responder must be in the test room only")
	}

	// A test agent lives in the test room only.
	session := "5e3b0d2f-8c14-4e6b-9d1e-" + fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	req := wire.AgentRequest{RequestID: strings.Repeat("c3", 16), Harness: wire.HarnessClaude, Session: session}
	var ag wire.Agent
	if c := callAPI(t, api.URL, "POST", wire.PathAgents, cr.OwnerKey, req, &ag); c != 200 || ag.RoomID != rooms["agents-test"] {
		t.Fatalf("agent: %d %+v", c, ag)
	}
	if slices.Contains(members(rooms["agents"]), ag.UserID) {
		t.Fatal("a test agent joined the agents room")
	}
	// Its own join of the agents room is refused (the guest gate of fact F4): only the registrar
	// puts agents in rooms (decision 39).
	agent := env.Client(ag.AccessToken)
	if _, err := agent.Join(ctx, rooms["agents"]); err == nil {
		t.Fatal("a test agent joined the agents room on its own")
	}

	// Report access: invite-only test report room, invited then joined by the registrar.
	var rj wire.ReportJoin
	if c := callAPI(t, api.URL, "POST", wire.PathReportJoin, cr.OwnerKey, wire.ReportJoinRequest{UserID: ag.UserID}, &rj); c != 200 ||
		rj.RoomID != rooms["agents-report-test"] {
		t.Fatalf("report join: %d %+v", c, rj)
	}
	if _, err := agent.Send(ctx, rj.RoomID, "it-report-1", map[string]string{"msgtype": "m.notice", "body": "kwak report: a check held a message"}); err != nil {
		t.Fatalf("agent posts the report: %v", err)
	}
	if err := agent.Leave(ctx, rj.RoomID); err != nil {
		t.Fatalf("agent leaves the report room: %v", err)
	}
	if slices.Contains(members(rooms["agents-report"]), ag.UserID) {
		t.Fatal("a test agent reached the normal report room")
	}
	// The pairing's listener account reports like an agent (decision 42): it joins, posts with
	// its own token and leaves, and it can do so again: the invite-only room invites it again.
	standing := env.Client(sr.AccessToken)
	for i := range 2 {
		var sj wire.ReportJoin
		if c := callAPI(t, api.URL, "POST", wire.PathReportJoin, cr.OwnerKey, wire.ReportJoinRequest{UserID: testStanding}, &sj); c != 200 ||
			sj.RoomID != rooms["agents-report-test"] {
			t.Fatalf("standing report join %d: %d %+v", i, c, sj)
		}
		if _, err := standing.Send(ctx, sj.RoomID, fmt.Sprintf("it-standing-report-%d", i), map[string]string{"msgtype": "m.notice", "body": "kwak report: the hourly limit"}); err != nil {
			t.Fatalf("standing account posts report %d: %v", i, err)
		}
		if err := standing.Leave(ctx, sj.RoomID); err != nil {
			t.Fatalf("standing account leaves the report room %d: %v", i, err)
		}
	}
	if slices.Contains(members(rooms["agents-report-test"]), testStanding) {
		t.Fatal("the test listener account stayed in the test report room")
	}

	// The per-owner standing accounts of the builds before decision 42 are no pairing's.
	for _, old := range []string{"@agent.alice.listener:" + server, "@agent.alice.listener-test:" + server} {
		if c := callAPI(t, api.URL, "POST", wire.PathReportJoin, cr.OwnerKey, wire.ReportJoinRequest{UserID: old}, nil); c != 403 {
			t.Fatalf("old standing account %s: %d", old, c)
		}
	}

	// Closing it leaves the test room, and the leave of the report room it already left counts as
	// done, so the cleanup finishes.
	if c := callAPI(t, api.URL, "POST", wire.PathAgentsClose, cr.OwnerKey,
		wire.CloseRequest{UserIDs: []string{ag.UserID}, Reason: wire.ReasonSessionEnded}, nil); c != 200 {
		t.Fatalf("close: %d", c)
	}
	waitFor(t, "cleanup of the closed test agent", func() (string, bool) {
		var done bool
		store.View(func(st *registrar.State) {
			a := st.Agents[strings.TrimSuffix(strings.TrimPrefix(ag.UserID, "@"), ":"+server)]
			done = a != nil && a.Closed != nil && a.CleanupDone
		})
		return "", done
	})
	if slices.Contains(members(rooms["agents-test"]), ag.UserID) {
		t.Fatal("closed agent still in the test room")
	}
}
