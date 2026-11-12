package listener

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

func syncResp(next string, limited bool, prev string, evs ...matrix.Event) syncResult {
	return syncResult{resp: &matrix.SyncResponse{NextBatch: next, Rooms: matrix.SyncRooms{Join: map[string]matrix.JoinedRoom{
		wiretest.RoomID: {Timeline: matrix.Timeline{Events: evs, Limited: limited, PrevBatch: prev}},
	}}}}
}

func (e *env) step() error {
	return e.l.syncStep(context.Background(), e.room.client(wiretest.StandingToken))
}

func TestFirstSyncDeliversNoHistory(t *testing.T) {
	e := newEnv(t)
	e.connect()
	uid := e.claudeSession(100, sid1, cwd)
	e.room.syncs <- syncResp("s1", false, "", event("$old", bob, e.clk.Now(), text("old", uid)))
	if err := e.step(); err != nil {
		t.Fatal(err)
	}
	if len(e.items(k1)) != 0 || e.l.st.NextBatch != "s1" {
		t.Fatal("first sync delivered history")
	}
	e.room.syncs <- syncResp("s2", false, "", event("$new", bob, e.clk.Now(), text("new", uid)))
	e.step()
	if len(e.items(k1)) != 1 || e.l.st.NextBatch != "s2" || e.l.st.LastEventID != "$new" {
		t.Fatalf("second sync: %d items, %+v", len(e.items(k1)), e.l.st.NextBatch)
	}
	if st := e.l.status(&client{pidOK: true}); st.State != wire.StateSyncing || st.LastSync.IsZero() {
		t.Fatalf("%+v", st)
	}
}

// gap builds 25 pages of /messages, two mentions each, with the last routed event in the last one.
func (e *env) gap(uid string) (pages int, newest []matrix.Event) {
	base := at("2026-11-02T08:00:00Z")
	e.l.mu.Lock()
	e.l.st.NextBatch, e.l.st.LastEventID, e.l.st.LastEventTS = "s1", "$last", base.UnixMilli()
	e.l.mu.Unlock()
	pages = 25
	n := 0
	ev := func() matrix.Event {
		n++
		return event(fmt.Sprintf("$g%03d", n), bob, base.Add(time.Duration(1000-n)*time.Second), text(fmt.Sprintf("g%03d", n), uid))
	}
	newest = []matrix.Event{ev(), ev()}
	newest[0], newest[1] = newest[1], newest[0]
	for p := 0; p < pages; p++ {
		chunk := []matrix.Event{ev(), ev()}
		end := fmt.Sprintf("p%d", p+1)
		if p == pages-1 {
			chunk = append(chunk, event("$last", bob, base, text("seen", uid)), ev())
			end = "p-never"
		}
		e.room.pages[fmt.Sprintf("p%d", p)] = &matrix.MessagesResponse{Chunk: chunk, End: end}
	}
	return pages, newest
}

func bodiesInOrder(e *env) []string {
	var out []string
	for _, it := range e.items(k1) {
		out = append(out, it.Body)
	}
	return out
}

func TestGapFillOverManyPages(t *testing.T) {
	e := newEnv(t)
	e.connect()
	uid := e.claudeSession(100, sid1, cwd)
	pages, newest := e.gap(uid)
	e.room.syncs <- syncResp("s2", true, "p0", newest...)
	if err := e.step(); err != nil {
		t.Fatal(err)
	}
	got := bodiesInOrder(e)
	if len(got) != 2*pages+2 {
		t.Fatalf("%d items, want %d", len(got), 2*pages+2)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] < got[i] {
			t.Fatalf("not oldest first at %d: %v", i, got[i-1:i+1])
		}
	}
	if e.l.st.NextBatch != "s2" || e.l.st.Backfill != nil || e.l.st.LastEventID != newest[1].EventID {
		t.Fatalf("after the fill: %q %+v %q", e.l.st.NextBatch, e.l.st.Backfill, e.l.st.LastEventID)
	}
	if _, err := os.Stat(filepath.Join(e.dir, backfillFile)); !os.IsNotExist(err) {
		t.Fatal("backfill.jsonl left behind")
	}
}

func TestGapFillContinuesAfterRestart(t *testing.T) {
	e := newEnv(t)
	e.connect()
	uid := e.claudeSession(100, sid1, cwd)
	pages, newest := e.gap(uid)
	e.room.pageErr["p12"] = errors.New("network down")
	e.room.syncs <- syncResp("s2", true, "p0", newest...)
	if err := e.step(); err == nil {
		t.Fatal("no error mid-fill")
	}
	if e.l.st.Backfill == nil || e.l.st.Backfill.From != "p12" || e.l.st.NextBatch != "s1" {
		t.Fatalf("progress not saved: %+v %q", e.l.st.Backfill, e.l.st.NextBatch)
	}
	if len(e.items(k1)) != 0 {
		t.Fatal("routed before the fill finished")
	}
	e.l.stop()
	e.l = e.build(Config{ConfigPath: e.confPath})
	e.connect()
	delete(e.room.pageErr, "p12")
	e.room.froms = nil
	e.room.syncs <- syncResp("s3", false, "")
	if err := e.step(); err != nil {
		t.Fatal(err)
	}
	if e.room.froms[0] != "p12" || len(e.room.froms) != pages-12 {
		t.Fatalf("restart re-fetched pages: %v", e.room.froms)
	}
	if n := len(e.items(k1)); n != 2*pages+2 {
		t.Fatalf("%d items", n)
	}
	if e.l.st.NextBatch != "s3" || e.l.st.Backfill != nil {
		t.Fatal("not finished")
	}
}

func TestGapFillMissedHistory(t *testing.T) {
	e := newEnv(t)
	e.connect()
	uid := e.claudeSession(100, sid1, cwd)
	e.l.mu.Lock()
	e.l.st.NextBatch, e.l.st.LastEventID, e.l.st.LastEventTS = "s1", "$gone", at("2026-11-01T00:00:00Z").UnixMilli()
	e.l.mu.Unlock()
	e.room.pages["p0"] = &matrix.MessagesResponse{Chunk: []matrix.Event{event("$a", bob, e.clk.Now(), text("a", uid))}}
	e.room.syncs <- syncResp("s2", true, "p0", event("$b", bob, e.clk.Now(), text("b", uid)))
	e.step()
	r := e.prompt(100, sid1, cwd)
	if !strings.Contains(r.Text, "| some messages may have been missed") || !strings.Contains(r.Text, "| a") || !strings.Contains(r.Text, "| b") {
		t.Fatalf("%q", r.Text)
	}
}

func TestLimitedSyncWithoutGap(t *testing.T) {
	e := newEnv(t)
	e.connect()
	uid := e.claudeSession(100, sid1, cwd)
	e.l.mu.Lock()
	e.l.st.NextBatch, e.l.st.LastEventID = "s1", "$seen"
	e.l.mu.Unlock()
	e.room.syncs <- syncResp("s2", true, "p0", event("$seen", bob, e.clk.Now(), text("x", uid)), event("$n", bob, e.clk.Now(), text("y", uid)))
	e.step()
	if got := bodiesInOrder(e); len(got) != 1 || got[0] != "y" || len(e.room.froms) != 0 {
		t.Fatalf("%v, pages %v", got, e.room.froms)
	}
}

func TestSyncLoopReturnsOnUnknownToken(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.room.revoked[wiretest.StandingToken] = true
	if err := e.l.syncLoop(context.Background(), wiretest.StandingToken); !errors.Is(err, errUnknownToken) {
		t.Fatal(err)
	}
}

func TestSyncWaitsRetryAfter(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.room.syncs <- syncResult{err: &matrix.Error{Status: 429, Code: matrix.ErrLimit, RetryAfter: 7 * time.Second}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- e.l.syncLoop(ctx, wiretest.StandingToken) }()
	eventually(t, "the wait", func() bool { return e.clk.waiting() == 1 })
	e.clk.Advance(6 * time.Second)
	time.Sleep(5 * time.Millisecond)
	if len(e.room.since) != 1 {
		t.Fatal("retried before Retry-After")
	}
	e.clk.Advance(time.Second)
	eventually(t, "the retry", func() bool {
		e.room.mu.Lock()
		defer e.room.mu.Unlock()
		return len(e.room.since) == 2
	})
	cancel()
	<-done
}

func TestAlivePingOnlyActiveInChunks(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.fixtureProcs()
	for i := 0; i < 1300; i++ {
		sid := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		s := e.live("claude", sid, cwd, claudeH, []proc.ID{claudeH}, fmt.Sprintf("@agent.fcanela.%016d:kwak.test", i))
		s.Active = i < 1200
	}
	e.reg.mu.Lock()
	e.reg.closed["@agent.fcanela.0000000000000007:kwak.test"] = true
	e.reg.mu.Unlock()
	e.l.mu.Lock()
	e.l.standing = wiretest.StandingToken
	e.l.mu.Unlock()
	e.l.hourly()
	e.reg.mu.Lock()
	sizes := []int{}
	total := 0
	for _, a := range e.reg.alives {
		sizes = append(sizes, len(a))
		total += len(a)
	}
	e.reg.mu.Unlock()
	if total != 1200 || len(sizes) != 3 || sizes[0] != 500 {
		t.Fatalf("chunks %v", sizes)
	}
	eventually(t, "reopen of the closed agent", func() bool { return e.reg.agentCalls() == 1 })
	e.reg.mu.Lock()
	e.reg.alives = nil
	e.reg.mu.Unlock()
	e.clk.Advance(time.Hour)
	e.l.hourly()
	if len(e.reg.alives) != 0 {
		t.Fatal("pinged again within a day")
	}
	e.clk.Advance(24 * time.Hour)
	e.l.hourly()
	if len(e.reg.alives) != 0 {
		t.Fatal("pinged sessions inactive since the last ping")
	}
}

// The cut-over of decision 43 relies on this: when the registrar closes the account a listener
// syncs on (the old per-owner account, logged out with every device), the sync gets 401
// M_UNKNOWN_TOKEN (probe F6), and the supervisor calls POST /standing again by itself, which hands
// it the pairing's own account. The sync goes on from its next_batch, with the new token.
func TestSupervisorReconnectsAfterALoggedOutToken(t *testing.T) {
	const legacyTok = "tok-legacy-0002"
	e := newEnv(t)
	e.reg.standUser, e.reg.standTok = wiretest.LegacyStanding, legacyTok
	e.supervise()
	syncs := func() []string {
		e.room.mu.Lock()
		defer e.room.mu.Unlock()
		return append([]string(nil), e.room.since...)
	}
	eventually(t, "first sync", func() bool { return len(syncs()) == 1 && e.reg.standingCalls() == 1 })
	e.room.syncs <- syncResp("s1", false, "")
	eventually(t, "second sync", func() bool { return len(syncs()) == 2 })

	// The registrar closes the old account and now answers the pairing's own one.
	e.reg.mu.Lock()
	e.reg.standUser, e.reg.standTok = "", ""
	e.reg.mu.Unlock()
	e.room.mu.Lock()
	e.room.revoked[legacyTok] = true
	e.room.mu.Unlock()
	e.room.syncs <- syncResult{err: &matrix.Error{Status: 401, Code: matrix.ErrUnknownToken}}

	eventually(t, "standing again", func() bool { return e.reg.standingCalls() == 2 })
	eventually(t, "sync as the new account", func() bool { return len(syncs()) == 3 })
	e.l.mu.Lock()
	user, tok, phase := e.l.standUser, e.l.standing, e.l.phase
	e.l.mu.Unlock()
	if user != wiretest.StandingUser || tok != wiretest.StandingToken || phase == wire.StateUnpaired {
		t.Fatalf("after the reconnect: account %q, token %q, phase %s", user, tok, phase)
	}
	if got := syncs(); got[2] != "s1" {
		t.Fatalf("the new account synced from %q", got[2])
	}
	e.waitLog("listener account changed", "from="+wiretest.LegacyStanding, "to="+wiretest.StandingUser)
}

// When the registrar moves the pairing to its own listener account (decisions 42 and 43: the old
// per-owner account is logged out, POST /standing answers the new one), the listener keeps its
// next_batch: a fresh first sync would drop the messages of the gap. A replay of what it already
// routed, up to last_event_id, reaches no inbox twice.
func TestListenerAccountSwitchKeepsTheSyncPosition(t *testing.T) {
	const legacyTok = "tok-legacy-0001"
	e := newEnv(t)
	e.reg.standUser, e.reg.standTok = wiretest.LegacyStanding, legacyTok
	e.connect()
	uid := e.claudeSession(100, sid1, cwd)
	old := e.room.client(legacyTok)
	e.room.syncs <- syncResp("s1", false, "")
	if err := e.l.syncStep(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	e.room.syncs <- syncResp("s2", false, "", event("$a", bob, e.clk.Now(), text("first", uid)))
	if err := e.l.syncStep(context.Background(), old); err != nil {
		t.Fatal(err)
	}

	// The old account's token is logged out; the reconnect lands on the pairing's own account.
	e.room.mu.Lock()
	e.room.revoked[legacyTok] = true
	e.room.mu.Unlock()
	e.reg.mu.Lock()
	e.reg.standUser, e.reg.standTok = "", ""
	e.reg.mu.Unlock()
	e.connect()
	e.waitLog("listener account changed", "from="+wiretest.LegacyStanding, "to="+wiretest.StandingUser)
	e.l.mu.Lock()
	next, user := e.l.st.NextBatch, e.l.standUser
	e.l.mu.Unlock()
	if next != "s2" || user != wiretest.StandingUser {
		t.Fatalf("next_batch %q, account %q", next, user)
	}

	e.room.syncs <- syncResp("s3", false, "",
		event("$a", bob, e.clk.Now(), text("first", uid)), event("$b", bob, e.clk.Now(), text("second", uid)))
	if err := e.step(); err != nil {
		t.Fatal(err)
	}
	e.room.mu.Lock()
	since := e.room.since[len(e.room.since)-1]
	e.room.mu.Unlock()
	if since != "s2" {
		t.Fatalf("the new account synced from %q", since)
	}
	if got := bodiesInOrder(e); len(got) != 2 || !strings.Contains(got[0], "first") || !strings.Contains(got[1], "second") {
		t.Fatalf("inbox %q", got)
	}
}
