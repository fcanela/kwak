package registrar

import (
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/wire"
	wt "github.com/fcanela/kwak/internal/wire/wiretest"
)

// seedPairing writes a pairing of owner whose key is key, as if it had been claimed.
func (f *fixture) seedPairing(id, owner, key string, confirmed bool) {
	f.t.Helper()
	f.seedKind(id, owner, key, confirmed, false)
}

// seedKind is seedPairing for either kind: test set makes a test pairing (decision 40). The
// contract pairing wt.PairingID gets the listener account of the fixtures (wt.StandingUser, or
// wt.TestStanding for a test pairing); any other pairing a fresh word pair. A confirmed pairing's
// listener account is made at once, as its first POST /standing would (decision 42).
func (f *fixture) seedKind(id, owner, key string, confirmed, test bool) {
	f.t.Helper()
	now := f.clock.Now()
	err := f.s.Store.Update(func(st *State) error {
		p := &Pairing{Owner: owner, Machine: wt.Machine, OS: "linux", IP: testIP, KeySHA256: sha(key),
			Created: now, Expires: now.Add(f.s.Cfg.KeyLifetime.D()), IPs: map[string]time.Time{testIP: now}, LastIP: testIP,
			Test: test}
		if confirmed {
			p.Confirmed = &now
		}
		switch {
		case id == wt.PairingID && owner == wt.Owner && test:
			p.Listener = strings.TrimSuffix(strings.TrimPrefix(wt.TestStanding, "@"), ":"+testServer)
		case id == wt.PairingID && owner == wt.Owner:
			p.Listener = strings.TrimSuffix(strings.TrimPrefix(wt.StandingUser, "@"), ":"+testServer)
		default:
			lp, err := f.s.freshListener(st, owner, test)
			if err != nil {
				return err
			}
			p.Listener = lp
		}
		st.Pairings[id] = p
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if confirmed {
		if _, e := f.s.ensureListener(f.t.Context(), owner, id); e != nil {
			f.t.Fatal(e)
		}
	}
}

// seed makes owner fcanela with pairing PairingID and key OwnerKey.
func (f *fixture) seed() {
	f.addOwner(wt.Owner)
	f.seedPairing(wt.PairingID, wt.Owner, wt.OwnerKey, true)
}

func (f *fixture) mustAgent(key string, h wire.Harness, session, helper string) wire.Agent {
	f.t.Helper()
	code, a, e := f.agent(key, h, session, helper)
	if code != 200 {
		f.t.Fatalf("agent %s %s: %d %+v", session, helper, code, e)
	}
	return a
}

func (f *fixture) mustClose(key string, uids ...string) {
	f.t.Helper()
	var out wire.CloseResponse
	if c := f.callJSON("POST", wire.PathAgentsClose, key, wire.CloseRequest{UserIDs: uids, Reason: "session_ended"}, &out); c != 200 {
		f.t.Fatalf("close %v: %d %s", uids, c, f.last)
	}
}

func (f *fixture) removeOwner(name string) {
	f.t.Helper()
	out := f.say(testAdmin, f.dm(testAdmin), "owner remove "+name)
	if len(out) != 1 || !strings.HasPrefix(out[0], "Removed owner") {
		f.t.Fatalf("owner remove: %q", out)
	}
}

// startPair runs POST /pair and returns the response.
func (f *fixture) startPair() wire.PairResponse {
	f.t.Helper()
	var pr wire.PairResponse
	if c := f.callJSON("POST", wire.PathPair, "", wire.PairRequest{Machine: wt.Machine, OS: "linux", Version: wire.Version}, &pr); c != 201 {
		f.t.Fatalf("pair: %d %s", c, f.last)
	}
	return pr
}

// tableSetup prepares the state each HTTP transcript row needs. It returns the claim to send in
// place of the fixture's, if any.
var tableSetup = map[string]func(f *fixture) string{
	"pair: first pairing": func(f *fixture) string { return "" },
	"pair: re-pair with a valid old key": func(f *fixture) string {
		f.addOwner(wt.Owner)
		f.seedPairing(wt.OldPairingID, wt.Owner, wt.OldOwnerKey, true)
		return ""
	},
	"pair: a bad, expired or revoked old key is not an error": func(f *fixture) string { return "" },
	"pair: unknown field": func(f *fixture) string { return "" },
	"claim: not sent yet": func(f *fixture) string { return f.startPair().Claim },
	"claim: paired": func(f *fixture) string {
		f.addOwner(wt.Owner)
		pr := f.startPair()
		f.say(mx(wt.Owner), f.dm(mx(wt.Owner)), "pair "+pr.Code)
		return pr.Claim
	},
	"claim: expired or already collected": func(f *fixture) string {
		f.addOwner(wt.Owner)
		pr := f.startPair()
		f.say(mx(wt.Owner), f.dm(mx(wt.Owner)), "pair "+pr.Code)
		if c := f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, nil); c != 200 {
			f.t.Fatalf("first claim: %d", c)
		}
		return pr.Claim
	},
	"claim: unknown claim":                      func(f *fixture) string { return "" },
	"standing: token":                           seedOnly,
	"standing: unknown, expired or revoked key": seedOnly,
	"standing: no key":                          seedOnly,
	"standing: owner removed":                   seedRemoved,
	"agents: new Claude session":                seedOnly,
	"agents: new Codex session":                 seedOnly,
	"agents: upper-case session id of an open agent re-mints its token": func(f *fixture) string {
		f.seed()
		f.mustAgent(wt.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
		return ""
	},
	"agents: closed agent reopens under its name": func(f *fixture) string {
		f.seed()
		a := f.mustAgent(wt.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
		f.mustClose(wt.OwnerKey, a.UserID)
		return ""
	},
	"agents: new helper of an open parent": func(f *fixture) string {
		f.seed()
		f.mustAgent(wt.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
		return ""
	},
	"agents: session id is not a UUID": seedOnly,
	"agents: unknown harness":          seedOnly,
	"agents: bad helper name":          seedOnly,
	"agents: bad request_id":           seedOnly,
	"agents: unknown field":            seedOnly,
	"agents: the session's agent belongs to another pairing": func(f *fixture) string {
		f.seed()
		f.seedPairing(wt.OldPairingID, wt.Owner, wt.OldOwnerKey, true)
		f.mustAgent(wt.OldOwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
		return ""
	},
	"agents: helper whose parent belongs to another pairing": func(f *fixture) string {
		f.seed()
		f.seedPairing(wt.OldPairingID, wt.Owner, wt.OldOwnerKey, true)
		f.mustAgent(wt.OldOwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
		return ""
	},
	"agents: helper whose parent is closed": func(f *fixture) string {
		f.seed()
		a := f.mustAgent(wt.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
		f.mustClose(wt.OwnerKey, a.UserID)
		return ""
	},
	"agents: helper whose parent does not exist": seedOnly,
	"agents: all five names taken": func(f *fixture) string {
		f.seed()
		hex, _ := wire.SessionHex(wt.ClaudeSession)
		for _, lp := range wire.Candidates(wt.Owner, wire.HarnessClaude, hex) {
			f.hs.addHuman(mx(lp))
		}
		return ""
	},
	"agents: display name or join failed at tuwunel": func(f *fixture) string {
		f.seed()
		f.hs.fail["SetDisplayName"] = mErr(500, "M_UNKNOWN")
		return ""
	},
	"agents: bad key":       seedOnly,
	"agents: owner removed": seedRemoved,
	"close: session agent and its helper": func(f *fixture) string {
		f.seed()
		f.mustAgent(wt.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
		f.mustAgent(wt.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "tests")
		return ""
	},
	"close: another pairing's agent": func(f *fixture) string {
		f.seed()
		f.mustAgent(wt.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
		f.addOwner("bob")
		f.seedPairing("b0b0b0b0", "bob", wt.BadKey, true)
		f.mustAgent(wt.BadKey, wire.HarnessClaude, "0718a1b2-c3d4-e5f6-8a9b-0c1d2e3f4a5b", "")
		return ""
	},
	"close: the standing account": seedOnly,
	"close: more than 500 ids":    seedOnly,
	"close: bad key":              seedOnly,
	"alive: one open, one unknown, one closed": func(f *fixture) string {
		f.seed()
		f.mustAgent(wt.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
		h := f.mustAgent(wt.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "tests")
		f.mustClose(wt.OwnerKey, h.UserID)
		return ""
	},
	"alive: all open": func(f *fixture) string {
		f.seed()
		f.mustAgent(wt.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
		return ""
	},
	"alive: more than 500 ids":     seedOnly,
	"alive: bad key":               seedOnly,
	"unpair: revokes this pairing": seedOnly,
	"unpair: bad key":              seedOnly,
	"me: owner settings":           seedOnly,
	"me: bad key":                  seedOnly,
	"me: owner removed":            seedRemoved,
	"report: join the session's agent": func(f *fixture) string {
		f.seed()
		f.sideRooms(false)
		f.mustAgent(wt.OwnerKey, wire.HarnessClaude, wt.ClaudeSession, "")
		return ""
	},
	"report: join the standing account": seedSide,
	"report: another pairing's agent":   seedSide,
	"report: the standing account of the other kind": func(f *fixture) string {
		f.seed()
		f.sideRooms(false)
		f.seedKind(wt.OldPairingID, wt.Owner, wt.OldOwnerKey, true, true)
		return ""
	},
	"report: no report room":   seedOnly,
	"report: unknown field":    seedSide,
	"report: bad key":          seedSide,
	"responder: token":         seedOnly,
	"responder: unknown field": seedOnly,
	"responder: bad key":       seedOnly,
	"responder: owner removed": seedRemoved,
}

func seedSide(f *fixture) string { f.seed(); f.sideRooms(false); return "" }

func seedOnly(f *fixture) string { f.seed(); return "" }

func seedRemoved(f *fixture) string {
	f.seed()
	f.removeOwner(wt.Owner)
	return ""
}

// TestHTTPTranscriptTable runs every registrar row of the wiretest wt.HTTP table (plan P1).
func TestHTTPTranscriptTable(t *testing.T) {
	for _, e := range wt.HTTP {
		if e.Edge {
			continue
		}
		setup, ok := tableSetup[e.Name]
		if !ok {
			t.Errorf("row %q has no setup", e.Name)
			continue
		}
		t.Run(e.Name, func(t *testing.T) {
			f := newFixture(t)
			claim := setup(f)
			auth := e.Auth
			if claim != "" && auth == "Bearer "+wt.Claim {
				auth = "Bearer " + claim
			}
			status, body := f.call(e.Method, e.Path, auth, e.Request)
			if err := e.Check(status, body); err != nil {
				t.Fatal(err)
			}
			if e.Errcode != "" && !strings.Contains(string(body), `"errcode":"`+e.Errcode+`"`) {
				t.Fatalf("errcode: %s", body)
			}
			if e.Status == 502 && !strings.Contains(string(body), "M_UNKNOWN") {
				t.Fatalf("502 body names no tuwunel errcode: %s", body)
			}
		})
	}
}
