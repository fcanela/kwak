//go:build integration

package registrar_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/registrar"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/test/integration/ittest"
)

// TestRegistrarAgainstTuwunel runs the real Server against tuwunel (plan P1): pairing through a
// real DM, then one agent's create, close and reopen.
func TestRegistrarAgainstTuwunel(t *testing.T) {
	env := ittest.Start(t)
	if env == nil {
		return
	}
	ctx := t.Context()
	server := env.ServerName
	bot := "@" + env.Registration.SenderLocalpart + ":" + server
	admin := env.Client(env.Admin.AccessToken)
	alice := env.Client(env.Alice.AccessToken)
	room, err := admin.CreateRoom(ctx, registrar.RoomBody(bot))
	if err != nil {
		t.Fatalf("create the agents room: %v", err)
	}

	cfg := registrar.Config{
		ServerName: server, Homeserver: env.Homeserver, Room: "#agents:" + server,
		Admins: []string{env.Admin.UserID}, State: filepath.Join(t.TempDir(), "state.json"),
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

	dmOf := func(human string) string {
		var dm string
		store.View(func(st *registrar.State) { dm = st.DMs[human] })
		return dm
	}
	adminDM := waitFor(t, "the admin DM", func() (string, bool) { d := dmOf(env.Admin.UserID); return d, d != "" })
	if enc, err := env.AppserviceClient().RoomEncrypted(ctx, adminDM); err != nil || enc {
		t.Fatalf("admin DM encrypted %v, %v", enc, err)
	}
	if _, err := admin.Join(ctx, adminDM); err != nil {
		t.Fatalf("admin joins the DM: %v", err)
	}
	send(t, admin, adminDM, "owner add "+env.Alice.UserID)
	waitNotice(t, admin, adminDM, "Added "+env.Alice.UserID+" as owner alice.")
	aliceDM := waitFor(t, "alice's DM", func() (string, bool) { d := dmOf(env.Alice.UserID); return d, d != "" })
	if _, err := alice.Join(ctx, aliceDM); err != nil {
		t.Fatalf("alice joins her DM: %v", err)
	}
	waitNotice(t, alice, aliceDM, "You are now a kwak owner as `alice`.")

	// Pairing: POST /pair, alice sends the code in her DM, the claim collects the key.
	var pr wire.PairResponse
	if c := callAPI(t, api.URL, "POST", wire.PathPair, "", wire.PairRequest{Machine: "it-box", OS: "linux", Version: wire.Version}, &pr); c != 201 {
		t.Fatalf("pair: %d", c)
	}
	if pr.Bot != bot {
		t.Fatalf("bot %q", pr.Bot)
	}
	send(t, alice, aliceDM, "pair "+pr.Code)
	var cr wire.ClaimResponse
	if c := callAPI(t, api.URL, "POST", wire.PathPairClaim, pr.Claim, struct{}{}, &cr); c != 200 || cr.Status != wire.ClaimPaired {
		t.Fatalf("claim: %d %+v", c, cr)
	}
	// The pairing's own listener account (decision 42): agent.alice.<adjective>-<animal>.listener.
	m := regexp.MustCompile(`^@agent\.alice\.([a-z]+-[a-z]+)\.listener:`).FindStringSubmatch(cr.StandingUser)
	if cr.Owner != "alice" || cr.RoomID != room || m == nil {
		t.Fatalf("claim %+v", cr)
	}
	waitNotice(t, alice, aliceDM, "Paired machine 'it-box' (linux) as "+m[1]+" (pairing "+cr.PairingID+"), asked from 203.0.113.5")

	var sr wire.Standing
	if c := callAPI(t, api.URL, "POST", wire.PathStanding, cr.OwnerKey, struct{}{}, &sr); c != 200 || sr.UserID != cr.StandingUser {
		t.Fatalf("standing: %d %+v", c, sr)
	}
	members, err := env.Client(sr.AccessToken).JoinedMembers(ctx, room)
	if err != nil || !slices.Contains(members, sr.UserID) {
		t.Fatalf("standing token: members %v, %v", members, err)
	}

	// One agent: create, close, reopen.
	session := "4f2a9c1e-7b03-4d5a-8c0d-" + fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	req := wire.AgentRequest{RequestID: strings.Repeat("a1", 16), Harness: wire.HarnessClaude, Session: session}
	var ag wire.Agent
	if c := callAPI(t, api.URL, "POST", wire.PathAgents, cr.OwnerKey, req, &ag); c != 200 || !ag.New {
		t.Fatalf("agent: %d %+v", c, ag)
	}
	if ag.UserID != "@agent.alice.4f2a9c1e7b034d5a:"+server || ag.RoomID != room {
		t.Fatalf("agent %+v", ag)
	}
	agent := env.Client(ag.AccessToken)
	if _, err := agent.Send(ctx, room, "it-1", map[string]string{"msgtype": "m.text", "body": "hello from the agent"}); err != nil {
		t.Fatalf("agent sends: %v", err)
	}
	if name := displayName(t, env, ag.AccessToken, ag.UserID); name != "agent.alice.4f2a9c1e7b034d5a" {
		t.Fatalf("display name %q", name)
	}

	var closed wire.CloseResponse
	if c := callAPI(t, api.URL, "POST", wire.PathAgentsClose, cr.OwnerKey, wire.CloseRequest{UserIDs: []string{ag.UserID}, Reason: wire.ReasonSessionEnded}, &closed); c != 200 {
		t.Fatalf("close: %d", c)
	}
	if _, err := agent.Send(ctx, room, "it-2", map[string]string{"msgtype": "m.text", "body": "after close"}); !matrix.IsCode(err, matrix.ErrUnknownToken) {
		t.Fatalf("old token after close: %v", err)
	}
	if members, _ := admin.JoinedMembers(ctx, room); slices.Contains(members, ag.UserID) {
		t.Fatal("closed agent still in the room")
	}
	var alive wire.AliveResponse
	callAPI(t, api.URL, "POST", wire.PathAgentsAlive, cr.OwnerKey, wire.AliveRequest{UserIDs: []string{ag.UserID}}, &alive)
	if len(alive.Closed) != 1 {
		t.Fatalf("alive %+v", alive)
	}

	req.RequestID = strings.Repeat("b2", 16)
	var re wire.Agent
	if c := callAPI(t, api.URL, "POST", wire.PathAgents, cr.OwnerKey, req, &re); c != 200 || !re.Reopened || re.New {
		t.Fatalf("reopen: %d %+v", c, re)
	}
	if re.UserID != ag.UserID {
		t.Fatalf("reopened as %s, was %s", re.UserID, ag.UserID)
	}
	if _, err := env.Client(re.AccessToken).Send(ctx, room, "it-3", map[string]string{"msgtype": "m.text", "body": "back"}); err != nil {
		t.Fatalf("reopened agent sends: %v", err)
	}
	if members, _ := admin.JoinedMembers(ctx, room); !slices.Contains(members, ag.UserID) {
		t.Fatal("reopened agent not in the room")
	}
	if _, err := agent.Send(ctx, room, "it-4", map[string]string{"msgtype": "m.text", "body": "old"}); !matrix.IsCode(err, matrix.ErrUnknownToken) {
		t.Fatalf("the first token came back: %v", err)
	}

	// The agent's own room events reach the registrar and move its idle clock.
	waitFor(t, "last_active from the agent's message", func() (string, bool) {
		var ok bool
		store.View(func(st *registrar.State) {
			a := st.Agents["agent.alice.4f2a9c1e7b034d5a"]
			ok = a != nil && a.Closed == nil && !a.LastActive.IsZero()
		})
		return "", ok
	})

	// The cut-over of decision 43: an open per-owner standing account of a build before decision
	// 42, with a listener syncing on it, is closed by the next job pass. Its sync token dies with
	// M_UNKNOWN_TOKEN (what makes a listener call POST /standing again), it leaves the room, and
	// POST /standing answers the pairing's own account.
	const legacyLP = "agent.alice.listener"
	legacyUID := "@" + legacyLP + ":" + server
	ll, err := env.AppserviceClient().RegisterAppservice(ctx, legacyLP, "KWAK-"+cr.PairingID)
	if err != nil {
		t.Fatalf("register the old account: %v", err)
	}
	if _, err := env.AppserviceClient().As(legacyUID).Join(ctx, room); err != nil {
		t.Fatalf("the old account joins: %v", err)
	}
	legacy := env.Client(ll.AccessToken)
	if _, err := legacy.Sync(ctx, "", "", 0); err != nil {
		t.Fatalf("the old account syncs: %v", err)
	}
	if err := store.Update(func(st *registrar.State) error {
		now := time.Now().UTC()
		st.Agents[legacyLP] = &registrar.Agent{Owner: "alice", Kind: wire.KindStanding, Created: now, LastActive: now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job pass closes the old account", func() (string, bool) {
		var done bool
		store.View(func(st *registrar.State) {
			a := st.Agents[legacyLP]
			done = a.Closed != nil && a.CleanupDone && a.CloseReason == "retired"
		})
		return "", done
	})
	if _, err := legacy.Sync(ctx, "", "", 0); !matrix.IsCode(err, matrix.ErrUnknownToken) {
		t.Fatalf("the old account's sync after the close: %v", err)
	}
	if members, _ := admin.JoinedMembers(ctx, room); slices.Contains(members, legacyUID) || !slices.Contains(members, sr.UserID) {
		t.Fatalf("members after the cut-over: %v", members)
	}
	var again wire.Standing
	if c := callAPI(t, api.URL, "POST", wire.PathStanding, cr.OwnerKey, struct{}{}, &again); c != 200 || again.UserID != sr.UserID {
		t.Fatalf("standing after the cut-over: %d %+v", c, again)
	}
}

func waitFor(t *testing.T, what string, f func() (string, bool)) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if v, ok := f(); ok {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

var txnN int

func send(t *testing.T, c *matrix.Client, room, body string) {
	t.Helper()
	txnN++
	if _, err := c.Send(t.Context(), room, fmt.Sprintf("it-cmd-%d-%d", time.Now().UnixNano(), txnN), map[string]string{"msgtype": "m.text", "body": body}); err != nil {
		t.Fatalf("send %q: %v", body, err)
	}
}

// waitNotice waits for a bot notice in room that starts with prefix.
func waitNotice(t *testing.T, c *matrix.Client, room, prefix string) {
	t.Helper()
	var seen []string
	waitFor(t, "notice "+prefix, func() (string, bool) {
		page, err := c.Messages(t.Context(), room, "", 50, "")
		if err != nil {
			return "", false
		}
		seen = seen[:0]
		for _, ev := range page.Chunk {
			var m struct{ MsgType, Body string }
			json.Unmarshal(ev.Content, &m)
			seen = append(seen, m.Body)
			if strings.HasPrefix(m.Body, prefix) {
				return "", true
			}
		}
		return "", false
	})
}

func callAPI(t *testing.T, base, method, path, key string, body, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, base+wire.APIPrefix+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Logf("%s %s: %d %s", method, path, resp.StatusCode, data)
		return resp.StatusCode
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, data)
		}
	}
	return resp.StatusCode
}

func displayName(t *testing.T, env *ittest.Env, token, uid string) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "GET", env.Homeserver+"/_matrix/client/v3/profile/"+uid+"/displayname", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Forwarded-For", ittest.ForwardedFor)
	resp, err := env.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var p struct {
		Displayname string `json:"displayname"`
	}
	json.NewDecoder(resp.Body).Decode(&p)
	return p.Displayname
}
