package registrar

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/wire"
	wt "github.com/fcanela/kwak/internal/wire/wiretest"
)

func TestPairingFlow(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	pr := f.startPair()
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`).MatchString(pr.Code) {
		t.Errorf("code %q", pr.Code)
	}
	if len(pr.Claim) != 43 || pr.Bot != mx("registrar") || pr.Replaces || !pr.ExpiresAt.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("pair response %+v", pr)
	}
	var cr wire.ClaimResponse
	if c := f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, &cr); c != 202 || cr.Status != wire.ClaimPending {
		t.Fatalf("claim before the code was sent: %d %+v", c, cr)
	}

	// Matching ignores case and dashes.
	out := f.say(mx("fcanela"), dm, "PAIR "+strings.ToLower(strings.ReplaceAll(pr.Code, "-", "")))
	if len(out) != 1 || !strings.Contains(out[0], "Paired machine 'laptop' (linux) as pairing ") ||
		!strings.Contains(out[0], "asked from "+testIP+" at 10:05 UTC") || !strings.Contains(out[0], "The key expires 2027-01-31") ||
		!strings.Contains(out[0], "send: revoke ") {
		t.Fatalf("bot reply %q", out)
	}
	if !f.hs.member(testRoom, mx("agent.fcanela.listener")) {
		t.Error("standing account not in the room")
	}
	if c := f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, &cr); c != 200 {
		t.Fatalf("claim: %d %s", c, f.last)
	}
	if cr.Owner != "fcanela" || !strings.HasPrefix(cr.OwnerKey, KeyPrefix) || len(cr.OwnerKey) != len(KeyPrefix)+43 ||
		cr.RoomID != testRoom || cr.StandingUser != mx("agent.fcanela.listener") || cr.ServerName != testServer {
		t.Fatalf("claim %+v", cr)
	}
	if c := f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, nil); c != 410 {
		t.Fatalf("second claim: %d", c)
	}
	st := f.state()
	p := st.Pairings[cr.PairingID]
	if p == nil || p.KeySHA256 != sha(cr.OwnerKey) || p.Confirmed != nil {
		t.Fatalf("pairing %+v", p)
	}
	if strings.Contains(string(mustRead(t, f.path)), cr.OwnerKey) {
		t.Fatal("plaintext owner key in state.json (I5)")
	}

	var sr wire.Standing
	if c := f.callJSON("POST", wire.PathStanding, cr.OwnerKey, struct{}{}, &sr); c != 200 {
		t.Fatalf("standing: %d", c)
	}
	if sr.DeviceID != "KWAK-"+cr.PairingID || sr.UserID != mx("agent.fcanela.listener") {
		t.Errorf("standing %+v", sr)
	}
	if live := f.hs.live(sr.UserID); len(live) != 1 || live[0] != sr.AccessToken {
		t.Errorf("standing tokens %v, want only %s", live, sr.AccessToken)
	}
	f.callJSON("POST", wire.PathStanding, cr.OwnerKey, struct{}{}, &sr)
	if live := f.hs.live(sr.UserID); len(live) != 1 || live[0] != sr.AccessToken {
		t.Errorf("after a second /standing: %v, want only %s", live, sr.AccessToken)
	}
	if f.state().Pairings[cr.PairingID].Confirmed == nil {
		t.Error("first authenticated call did not confirm the key")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPairingWrongCodeAndExpiry(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	out := f.say(mx("fcanela"), dm, "pair AAAA-BBBB-CCCC")
	if len(out) != 1 || !strings.HasPrefix(out[0], "No pairing with that code. Codes expire after 10 minutes") {
		t.Fatalf("wrong code: %q", out)
	}
	pr := f.startPair()
	f.clock.Advance(10*time.Minute + time.Second)
	out = f.say(mx("fcanela"), dm, "pair "+pr.Code)
	if len(out) != 1 || !strings.HasPrefix(out[0], "No pairing with that code") {
		t.Fatalf("expired code: %q", out)
	}
	if c := f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, nil); c != 410 {
		t.Fatalf("claim of an expired pairing: %d", c)
	}
	// A code works once.
	pr = f.startPair()
	f.say(mx("fcanela"), dm, "pair "+pr.Code)
	out = f.say(mx("fcanela"), dm, "pair "+pr.Code)
	if len(out) != 1 || !strings.HasPrefix(out[0], "No pairing with that code") {
		t.Fatalf("code reused: %q", out)
	}
}

func TestPairingUnconfirmedKeyIsRevoked(t *testing.T) {
	f := newFixture(t)
	f.addOwner("fcanela")
	old := f.pair("fcanela", "", true)
	repair := f.pair("fcanela", old.key, false)
	f.clock.Advance(11 * time.Minute)
	f.s.runJobs(t.Context())
	st := f.state()
	if p := st.Pairings[repair.id]; p.Revoked == nil || p.RevokeReason != "unconfirmed" || !p.LogoutDone {
		t.Fatalf("unconfirmed pairing %+v", p)
	}
	if p := st.Pairings[old.id]; p.Revoked != nil {
		t.Fatalf("the pairing it would replace was revoked: %+v", p)
	}
	if c := f.callJSON("POST", wire.PathStanding, repair.key, struct{}{}, nil); c != 401 {
		t.Fatalf("revoked key: %d", c)
	}
	if c := f.callJSON("POST", wire.PathStanding, old.key, struct{}{}, nil); c != 200 {
		t.Fatalf("old key: %d", c)
	}
}

func TestRepairConfirmationMovesRecordsAndRevokesOld(t *testing.T) {
	f := newFixture(t)
	f.addOwner("fcanela")
	old := f.pair("fcanela", "", true)
	open := f.mustAgent(old.key, wire.HarnessClaude, wt.ClaudeSession, "")
	closed := f.mustAgent(old.key, wire.HarnessCodex, wt.CodexSession, "")
	f.mustClose(old.key, closed.UserID)

	var pr wire.PairResponse
	f.callJSON("POST", wire.PathPair, old.key, wire.PairRequest{Machine: "laptop", OS: "linux"}, &pr)
	if !pr.Replaces {
		t.Fatal("re-pair with a valid old key does not replace")
	}
	f.say(mx("fcanela"), f.dm(mx("fcanela")), "pair "+pr.Code)
	var cr wire.ClaimResponse
	f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, &cr)
	if st := f.state(); st.Pairings[old.id].Revoked != nil || st.Agents["agent.fcanela.4f2a9c1e7b034d5a"].Pairing != old.id {
		t.Fatal("records moved before the new key was confirmed")
	}
	tokensBefore := f.hs.live(open.UserID)
	if c := f.callJSON("POST", wire.PathStanding, cr.OwnerKey, struct{}{}, nil); c != 200 {
		t.Fatalf("standing: %d", c)
	}
	st := f.state()
	if p := st.Pairings[old.id]; p.Revoked == nil || p.RevokeReason != "replaced" {
		t.Fatalf("old pairing %+v", p)
	}
	for lp, a := range st.Agents {
		if a.Kind != wire.KindStanding && a.Session != "" && a.Pairing != cr.PairingID {
			t.Errorf("%s stayed on pairing %s", lp, a.Pairing)
		}
	}
	if a := st.Agents["agent.fcanela.4f2a9c1e7b034d5a"]; a.Closed != nil {
		t.Error("a re-pair closed a live agent")
	}
	if got := f.hs.live(open.UserID); len(got) != 1 || got[0] != tokensBefore[0] || got[0] != open.AccessToken {
		t.Errorf("the live agent's token changed: %v, had %s", got, open.AccessToken)
	}
	if f.hs.live(mx("agent.fcanela.listener")) == nil {
		t.Error("standing account lost every token")
	}
	if c := f.callJSON("POST", wire.PathStanding, old.key, struct{}{}, nil); c != 401 {
		t.Errorf("old key after the re-pair: %d", c)
	}
	// The moved records belong to the new key now: the same session re-mints there.
	if code, a, _ := f.agent(cr.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, ""); code != 200 || a.UserID != open.UserID || a.New {
		t.Errorf("session after the re-pair: %d %+v", code, a)
	}
	if code, a, _ := f.agent(cr.OwnerKey, wire.HarnessCodex, wt.CodexSession, ""); code != 200 || !a.Reopened {
		t.Errorf("closed session after the re-pair: %d %+v", code, a)
	}
}

func TestReplacesClearedWhenOldPairingRevokedBeforeCode(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	old := f.pair("fcanela", "", true)
	var pr wire.PairResponse
	f.callJSON("POST", wire.PathPair, old.key, wire.PairRequest{Machine: "laptop", OS: "linux"}, &pr)
	if !pr.Replaces {
		t.Fatal("no replaces")
	}
	f.say(mx("fcanela"), dm, "revoke "+old.id)
	out := f.say(mx("fcanela"), dm, "pair "+pr.Code)
	if len(out) != 1 || !strings.Contains(out[0], "This pairing does not replace an earlier one.") {
		t.Fatalf("reply %q", out)
	}
	var cr wire.ClaimResponse
	f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, &cr)
	if p := f.state().Pairings[cr.PairingID]; p.Replaces != "" {
		t.Fatalf("replaces %q", p.Replaces)
	}
}

func TestReplacesNeedsTheSameOwner(t *testing.T) {
	f := newFixture(t)
	f.addOwner("fcanela")
	bobDM := f.addOwner("bob")
	old := f.pair("fcanela", "", true)
	var pr wire.PairResponse
	f.callJSON("POST", wire.PathPair, old.key, wire.PairRequest{Machine: "laptop", OS: "linux"}, &pr)
	out := f.say(mx("bob"), bobDM, "pair "+pr.Code)
	if len(out) != 1 || !strings.Contains(out[0], "does not replace an earlier one") {
		t.Fatalf("reply %q", out)
	}
	var cr wire.ClaimResponse
	f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, &cr)
	if cr.Owner != "bob" || f.state().Pairings[cr.PairingID].Replaces != "" {
		t.Fatalf("bob's pairing replaces fcanela's: %+v", f.state().Pairings[cr.PairingID])
	}
}

func TestPairWithBadBearerPairsWithoutReplaces(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	var pr wire.PairResponse
	if c := f.callJSON("POST", wire.PathPair, wt.BadKey, wire.PairRequest{Machine: "laptop", OS: "linux"}, &pr); c != 201 || pr.Replaces {
		t.Fatalf("%d %+v", c, pr)
	}
	out := f.say(mx("fcanela"), dm, "pair "+pr.Code)
	if len(out) != 1 || !strings.Contains(out[0], "old key sent with it was not valid") {
		t.Fatalf("reply %q", out)
	}
}

func TestNewIPPostsOneNotice(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	p := f.pair("fcanela", "", true)
	base := len(f.hs.noticesIn(dm))
	f.callJSON("GET", wire.PathMe, p.key, nil, nil)
	if n := len(f.hs.noticesIn(dm)) - base; n != 0 {
		t.Fatalf("%d notices for the pairing's own IP", n)
	}
	f.ip = "198.51.100.7"
	f.callJSON("GET", wire.PathMe, p.key, nil, nil)
	f.callJSON("GET", wire.PathMe, p.key, nil, nil)
	got := f.hs.noticesIn(dm)[base:]
	want := "Key for laptop (" + p.id + ") used from 198.51.100.7. If that is not you, send: revoke " + p.id
	if len(got) != 1 || got[0] != want {
		t.Fatalf("notices %q, want one %q", got, want)
	}
	if st := f.state(); st.Pairings[p.id].LastIP != "198.51.100.7" {
		t.Errorf("last_ip %q", st.Pairings[p.id].LastIP)
	}
}

func TestClientIPIsLastForwardedFor(t *testing.T) {
	f := newFixture(t)
	r := newReq("POST", wire.APIPrefix+wire.PathPair, `{"machine":"m","os":"linux","version":"0.1.0"}`)
	r.Header.Add("X-Forwarded-For", "203.0.113.9, 198.51.100.1")
	if ip := clientIP(r); ip != "198.51.100.1" {
		t.Errorf("ip %q", ip)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if ip := clientIP(r); ip != testIP {
		t.Errorf("unparsable XFF gave %q, want the peer", ip)
	}
	_ = f
}

func TestPendingPairingsAreBounded(t *testing.T) {
	f := newFixture(t)
	first := f.startPair()
	for range maxPending {
		f.startPair()
	}
	f.s.pmu.Lock()
	n := len(f.s.pending)
	_, kept := f.s.pending[normCode(first.Code)]
	f.s.pmu.Unlock()
	if n != maxPending || kept {
		t.Fatalf("%d pending, oldest kept %v", n, kept)
	}
}

func TestSanitizeLabel(t *testing.T) {
	cases := map[string]string{
		"laptop":                        "laptop",
		"evil‮machine":                  "evilmachine",
		"two\nlines\tand\x00nul":        "two lines andnul",
		" sep ":                         "sep",
		strings.Repeat("x", 100):        strings.Repeat("x", 64),
		"tag\U000E0041\U000E0042hidden": "taghidden",
		"\x1b[31mred\x1b[0m":            "red",
		"heart\uFE0F\uFDD0":             "heart",
		"bad\uFFFDbyte":                 "bad\uFFFDbyte",
		"a\tb":                          "a b",
	}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func newReq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = testIP + ":40000"
	return r
}
