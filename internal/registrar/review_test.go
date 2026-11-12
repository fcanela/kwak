package registrar

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/wire"
	wt "github.com/fcanela/kwak/internal/wire/wiretest"
)

func TestReplayedOldCommandOutsideDoneEventsRefused(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	old := f.event(mx("fcanela"), dm, "m.room.message", nil, map[string]string{"msgtype": "m.text", "body": "set question-timeout 2h"})
	f.txn(old)
	f.clock.Advance(time.Minute)
	if out := f.say(mx("fcanela"), dm, "set question-timeout 15m"); len(out) != 1 {
		t.Fatalf("set question-timeout 15m: %q", out)
	}
	f.s.Store.Update(func(st *State) error {
		st.DoneEvents = nil
		return nil
	})
	before := len(f.hs.noticesIn(dm))
	f.txn(old)
	if out := f.hs.noticesIn(dm)[before:]; len(out) != 0 {
		t.Fatalf("a replayed command got a reply: %q", out)
	}
	if got := f.state().Owners["fcanela"].Settings.QuestionTimeout; got != "15m" {
		t.Fatalf("a replayed command ran: question timeout %s", got)
	}
}

func TestCommandReReadFailureReplies(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	f.hs.mu.Lock()
	f.hs.fail["FetchEvent"] = errors.New("deadline exceeded")
	f.hs.mu.Unlock()
	out := f.say(mx("fcanela"), dm, "set question-timeout 2h")
	if len(out) != 1 || !strings.HasPrefix(out[0], "That failed: ") || !strings.HasSuffix(out[0], "Try again.") {
		t.Fatalf("re-read failure: %q", out)
	}
	if f.state().Owners["fcanela"].Settings.QuestionTimeout == "2h" {
		t.Fatal("the command ran without the re-check")
	}
	f.hs.mu.Lock()
	delete(f.hs.fail, "FetchEvent")
	f.hs.fail["RoomInfo"] = errors.New("deadline exceeded")
	f.hs.mu.Unlock()
	if out := f.say(mx("fcanela"), dm, "set question-timeout 2h"); len(out) != 1 || !strings.HasPrefix(out[0], "That failed: ") {
		t.Fatalf("room info failure: %q", out)
	}
}

func TestIdleJobSkipsAgentUsedDuringTheRun(t *testing.T) {
	f, p := pairedFixture(t)
	f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	b := f.mustAgent(p.key, wire.HarnessCodex, wt.CodexSession, "")
	f.clock.Advance(15 * 24 * time.Hour)
	first := mx(claudeLP)
	if codexLP < claudeLP {
		t.Fatal("fixture order changed")
	}
	f.hs.mu.Lock()
	f.hs.before["LeaveAs"] = func(user string) {
		if user != first {
			return
		}
		f.hs.mu.Lock()
		delete(f.hs.before, "LeaveAs")
		f.hs.mu.Unlock()
		f.callJSON("POST", wire.PathAgentsAlive, p.key, wire.AliveRequest{UserIDs: []string{b.UserID}, Touch: true}, nil)
	}
	f.hs.mu.Unlock()
	f.s.runJobs(t.Context())
	st := f.state()
	if st.Agents[claudeLP].Closed == nil {
		t.Fatal("the idle agent stayed open")
	}
	if r := st.Agents[codexLP]; r.Closed != nil {
		t.Fatalf("an agent used during the run was closed as idle: %+v", r)
	}
}

func TestFailedStandingLeavesRepairUnconfirmed(t *testing.T) {
	f := newFixture(t)
	f.addOwner("fcanela")
	old := f.pair("fcanela", "", true)
	f.mustAgent(old.key, wire.HarnessClaude, wt.ClaudeSession, "")
	var pr wire.PairResponse
	f.callJSON("POST", wire.PathPair, old.key, wire.PairRequest{Machine: "laptop", OS: "linux"}, &pr)
	f.say(mx("fcanela"), f.dm(mx("fcanela")), "pair "+pr.Code)
	var cr wire.ClaimResponse
	f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, &cr)
	f.hs.mu.Lock()
	f.hs.fail["Login"] = mErr(502, "M_UNKNOWN")
	f.hs.mu.Unlock()
	if c := f.callJSON("POST", wire.PathStanding, cr.OwnerKey, struct{}{}, nil); c != 502 {
		t.Fatalf("standing with a failing homeserver: %d", c)
	}
	st := f.state()
	if st.Pairings[old.id].Revoked != nil || st.Pairings[cr.PairingID].Confirmed != nil ||
		st.Agents[claudeLP].Pairing != old.id {
		t.Fatal("a failed POST /standing confirmed the re-pair")
	}
	if c := f.callJSON("GET", wire.PathMe, old.key, nil, nil); c != 200 {
		t.Fatalf("old key after the failed standing: %d", c)
	}
	f.hs.mu.Lock()
	delete(f.hs.fail, "Login")
	f.hs.mu.Unlock()
	if c := f.callJSON("POST", wire.PathStanding, cr.OwnerKey, struct{}{}, nil); c != 200 {
		t.Fatalf("standing: %d", c)
	}
	st = f.state()
	if st.Pairings[old.id].RevokeReason != "replaced" || st.Agents[claudeLP].Pairing != cr.PairingID {
		t.Fatal("the working POST /standing did not confirm the re-pair")
	}
}

func TestAgentsListsEveryAgentInNoticesUnderTheLimit(t *testing.T) {
	f, p := pairedFixture(t)
	var want []string
	for i := 0; i < 450; i++ {
		sid := fmt.Sprintf("%08x-1111-4222-8333-%012x", i+1, i+1)
		a := f.mustAgent(p.key, wire.HarnessClaude, sid, "")
		want = append(want, strings.TrimSuffix(strings.TrimPrefix(a.UserID, "@"), ":"+testServer))
	}
	out := f.say(mx("fcanela"), f.dm(mx("fcanela")), "agents")
	if len(out) < 2 {
		t.Fatalf("%d notices for 450 agents", len(out))
	}
	all := strings.Join(out, "\n")
	for _, n := range out {
		if len(n) > maxNotice {
			t.Fatalf("a notice of %d bytes", len(n))
		}
	}
	for _, lp := range want {
		if !strings.Contains(all, lp+",") {
			t.Fatalf("%s is missing from the list", lp)
		}
	}
	if strings.Contains(all, "more") {
		t.Fatal("the list is still capped")
	}
}
