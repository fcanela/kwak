package registrar

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
	wt "github.com/fcanela/kwak/internal/wire/wiretest"
)

// hook sets a before-hook that runs once, for calls on user only. Later calls do not wait for it.
func (f *fixture) hook(method, user string, fn func()) {
	var fired atomic.Bool
	f.hs.mu.Lock()
	defer f.hs.mu.Unlock()
	f.hs.before[method] = func(u string) {
		if u == user && fired.CompareAndSwap(false, true) {
			fn()
		}
	}
}

// waitBlocked waits until done closes or n holders queue on the lock key, and reports which.
func (f *fixture) waitBlocked(key string, n int, done <-chan struct{}) string {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return "done"
		default:
		}
		f.s.lmu.Lock()
		l := f.s.locks[key]
		blocked := l != nil && l.refs >= n
		f.s.lmu.Unlock()
		if blocked {
			return "blocked"
		}
		time.Sleep(time.Millisecond)
	}
	f.t.Fatal("neither done nor blocked on", key)
	return ""
}

func (f *fixture) waitState(cond func(*State) bool) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ok := false
		f.s.Store.View(func(st *State) { ok = cond(st) })
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	f.t.Fatal("state never changed")
}

func (f *fixture) deviceTokens(uid, dev string) []string {
	f.hs.mu.Lock()
	defer f.hs.mu.Unlock()
	if u := f.hs.users[uid]; u != nil {
		return append([]string(nil), u.tokens[dev]...)
	}
	return nil
}

func claudeLock(t *testing.T, helper string) string {
	hex, err := wire.SessionHex(wt.ClaudeSession)
	if err != nil {
		t.Fatal(err)
	}
	return "agent|" + sessionKey{"fcanela", wire.HarnessClaude, hex, helper}.String()
}

func revoked(id string) func(*State) bool {
	return func(st *State) bool { return st.Pairings[id].Revoked != nil }
}

func TestRevokeDuringNewIPNoticeRefusesAgent(t *testing.T) {
	f, p := pairedFixture(t)
	f.hook("Notice", f.dm(mx("fcanela")), func() {
		if err := f.s.revoke(context.Background(), p.id, "revoked"); err != nil {
			t.Error(err)
		}
	})
	f.ip = "198.51.100.7"
	if code, _, e := f.agent(p.key, wire.HarnessClaude, wt.ClaudeSession, ""); code != 401 || e.Errcode != wire.ErrBadKey {
		t.Fatalf("POST /agents after a revoke: %d %+v", code, e)
	}
	if rec := f.state().Agents[claudeLP]; rec != nil && rec.Open() {
		t.Fatalf("open record on a revoked pairing %+v", rec)
	}
	if live := f.hs.live(mx(claudeLP)); live != nil {
		t.Fatalf("live %v on a revoked pairing (I9)", live)
	}
}

func TestRevokeDuringReopenWithdrawsTheToken(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	f.mustClose(p.key, a.UserID)
	f.hook("Login", a.UserID, func() {
		if err := f.s.revoke(context.Background(), p.id, "revoked"); err != nil {
			t.Error(err)
		}
	})
	if code, _, e := f.agent(p.key, wire.HarnessClaude, wt.ClaudeSession, ""); code != 401 || e.Errcode != wire.ErrBadKey {
		t.Fatalf("reopen across a revoke: %d %+v", code, e)
	}
	rec := f.state().Agents[claudeLP]
	if rec.Closed == nil || rec.CloseReason != "revoked" || !rec.CleanupDone {
		t.Fatalf("record %+v", rec)
	}
	if live := f.hs.live(a.UserID); live != nil || f.hs.member(testRoom, a.UserID) {
		t.Fatalf("live %v, member %v after a revoke (I9)", live, f.hs.member(testRoom, a.UserID))
	}
}

func TestRevokeDuringRemintWithdrawsTheToken(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	var wg sync.WaitGroup
	f.hook("Login", a.UserID, func() {
		wg.Go(func() { f.s.revoke(context.Background(), p.id, "revoked") })
		f.waitState(revoked(p.id))
	})
	if code, _, e := f.agent(p.key, wire.HarnessClaude, wt.ClaudeSession, ""); code != 401 {
		t.Fatalf("re-mint across a revoke: %d %+v", code, e)
	}
	wg.Wait()
	if rec := f.state().Agents[claudeLP]; rec.Closed == nil || f.hs.live(a.UserID) != nil {
		t.Fatalf("record %+v, live %v", rec, f.hs.live(a.UserID))
	}
}

func TestParentCloseDuringHelperReopen(t *testing.T) {
	f, p := pairedFixture(t)
	f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	h := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "tests")
	f.mustClose(p.key, h.UserID)
	f.hook("Login", h.UserID, func() {
		if _, err := f.s.closeAgents(context.Background(), []string{claudeLP}, "session_ended", "", nil); err != nil {
			t.Error(err)
		}
	})
	if code, _, e := f.agent(p.key, wire.HarnessClaude, wt.ClaudeSession, "tests"); code != 403 || e.Errcode != wire.ErrParentClosed {
		t.Fatalf("helper reopen across its parent's close: %d %+v", code, e)
	}
	if rec := f.state().Agents[claudeLP+".tests"]; rec.Closed == nil || !rec.CleanupDone {
		t.Fatalf("helper record %+v", rec)
	}
	if f.hs.live(h.UserID) != nil || f.hs.member(testRoom, h.UserID) {
		t.Fatal("helper of a closed parent keeps a token or the membership")
	}
}

func TestRevokeDuringNewIPNoticeRefusesStanding(t *testing.T) {
	f, p := pairedFixture(t)
	f.hook("Notice", f.dm(mx("fcanela")), func() {
		if err := f.s.revoke(context.Background(), p.id, "revoked"); err != nil {
			t.Error(err)
		}
	})
	f.ip = "198.51.100.7"
	if c := f.callJSON("POST", wire.PathStanding, p.key, struct{}{}, nil); c != 401 {
		t.Fatalf("POST /standing after a revoke: %d", c)
	}
	if toks := f.deviceTokens(mx("agent.fcanela.listener"), "KWAK-"+p.id); len(toks) != 0 {
		t.Fatalf("standing device tokens %v on a revoked pairing (I9)", toks)
	}
}

func TestRevokeWaitsForStandingLogin(t *testing.T) {
	f, p := pairedFixture(t)
	uid, dev := mx("agent.fcanela.listener"), "KWAK-"+p.id
	var wg sync.WaitGroup
	f.hook("LogoutDevice", uid+"/"+dev, func() {
		done := make(chan struct{})
		wg.Go(func() {
			defer close(done)
			f.s.revoke(context.Background(), p.id, "revoked")
		})
		f.waitState(revoked(p.id))
		f.waitBlocked("standing|fcanela", 2, done)
	})
	f.callJSON("POST", wire.PathStanding, p.key, struct{}{}, nil)
	wg.Wait()
	if toks := f.deviceTokens(uid, dev); len(toks) != 0 {
		t.Fatalf("standing device tokens %v survive the revoke (I9)", toks)
	}
}

// reopenDuring makes a reopen of the claude agent race the cleanup that LeaveAs starts.
func reopenDuring(t *testing.T, f *fixture, p paired, uid string) (*sync.WaitGroup, *wire.Agent, *int) {
	var wg sync.WaitGroup
	var got wire.Agent
	var code int
	f.hook("LeaveAs", uid, func() {
		done := make(chan struct{})
		wg.Go(func() {
			defer close(done)
			code, got, _ = f.agent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
		})
		f.waitBlocked(claudeLock(t, ""), 2, done)
	})
	return &wg, &got, &code
}

func checkReopened(t *testing.T, f *fixture, uid string, code int, got wire.Agent) {
	t.Helper()
	if code != 200 || !got.Reopened {
		t.Fatalf("reopen %d %+v", code, got)
	}
	if rec := f.state().Agents[claudeLP]; rec.Closed != nil {
		t.Fatalf("record %+v", rec)
	}
	if live := f.hs.live(uid); len(live) != 1 || live[0] != got.AccessToken || !f.hs.member(testRoom, uid) {
		t.Fatalf("after reopen: live %v, member %v", live, f.hs.member(testRoom, uid))
	}
}

func TestReopenWaitsForCloseCleanup(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	wg, got, code := reopenDuring(t, f, p, a.UserID)
	f.mustClose(p.key, a.UserID)
	wg.Wait()
	checkReopened(t, f, a.UserID, *code, *got)
}

func TestReopenWaitsForCleanupRetry(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	f.hs.fail["LogoutAll"] = mErr(502, "")
	f.mustClose(p.key, a.UserID)
	f.hs.mu.Lock()
	delete(f.hs.fail, "LogoutAll")
	f.hs.mu.Unlock()
	wg, got, code := reopenDuring(t, f, p, a.UserID)
	f.s.retryCleanups(t.Context())
	wg.Wait()
	checkReopened(t, f, a.UserID, *code, *got)
}

func TestUnconfirmedKeyPastTTLIsRefused(t *testing.T) {
	f, old := pairedFixture(t)
	a := f.mustAgent(old.key, wire.HarnessClaude, wt.ClaudeSession, "")
	p := f.pair("fcanela", old.key, false)
	f.clock.Advance(f.s.Cfg.UnconfirmedTTL.D() + time.Minute)
	if c := f.callJSON("POST", wire.PathStanding, p.key, struct{}{}, nil); c != 401 {
		t.Fatalf("an unconfirmed key past unconfirmed_ttl: %d", c)
	}
	st := f.state()
	if st.Pairings[p.id].Confirmed != nil || st.Pairings[old.id].Revoked != nil || st.Agents[claudeLP].Pairing != old.id {
		t.Fatalf("the late key moved or revoked the old pairing: %+v %+v", st.Pairings[old.id], st.Agents[claudeLP])
	}
	if c := f.callJSON("GET", wire.PathMe, old.key, nil, nil); c != 200 {
		t.Fatalf("the replaced key must stay valid: %d", c)
	}
	if b := f.mustAgent(old.key, wire.HarnessClaude, wt.ClaudeSession, ""); b.UserID != a.UserID {
		t.Fatalf("agent %+v", b)
	}
}

func TestHomeserverErrorsHoldNoURL(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	base := dead.URL
	dead.Close()
	garbled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer garbled.Close()
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"no"}`))
	}))
	defer forbidden.Close()

	uid := mx(claudeLP)
	ctx := t.Context()
	calls := map[string]func(Homeserver) error{
		"Register":       func(h Homeserver) error { _, err := h.Register(ctx, claudeLP, "KWAK"); return err },
		"Login":          func(h Homeserver) error { _, err := h.Login(ctx, uid, "KWAK"); return err },
		"LogoutDevice":   func(h Homeserver) error { return h.LogoutDevice(ctx, uid, "KWAK-3f9a1c2e") },
		"LogoutAll":      func(h Homeserver) error { return h.LogoutAll(ctx, uid) },
		"JoinAs":         func(h Homeserver) error { return h.JoinAs(ctx, uid, testRoom) },
		"LeaveAs":        func(h Homeserver) error { return h.LeaveAs(ctx, uid, testRoom) },
		"SetDisplayName": func(h Homeserver) error { return h.SetDisplayName(ctx, uid, claudeLP) },
		"SetAvatarURL":   func(h Homeserver) error { return h.SetAvatarURL(ctx, uid, "") },
		"UserExists":     func(h Homeserver) error { _, err := h.UserExists(ctx, uid); return err },
		"CreateDM":       func(h Homeserver) error { _, err := h.CreateDM(ctx, testAdmin); return err },
		"Invite":         func(h Homeserver) error { return h.Invite(ctx, testRoom, testAdmin) },
		"RoomInfo":       func(h Homeserver) error { _, _, err := h.RoomInfo(ctx, testRoom); return err },
		"FetchEvent":     func(h Homeserver) error { _, err := h.FetchEvent(ctx, testRoom, "$e"); return err },
		"Notice":         func(h Homeserver) error { return h.Notice(ctx, testRoom, "kwak-1", "hi") },
		"ResolveAlias":   func(h Homeserver) error { _, err := h.ResolveAlias(ctx, testAlias); return err },
	}
	for name, call := range calls {
		for _, b := range []string{base, garbled.URL} {
			err := call(NewHomeserver(b, "as-token", matrix.NewHTTPClient()))
			if err == nil {
				if b == base {
					t.Errorf("%s on a dead port: no error", name)
				}
				continue
			}
			for _, bad := range []string{"user_id", "?", "/_matrix", "http://", "as-token"} {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("%s: error %q holds %q (I4)", name, err, bad)
				}
			}
		}
		err := call(NewHomeserver(forbidden.URL, "as-token", matrix.NewHTTPClient()))
		if name != "UserExists" && name != "RoomInfo" && !matrix.IsCode(err, matrix.ErrForbidden) {
			t.Errorf("%s: a matrix error must survive the scrub, got %v", name, err)
		}
	}

	var logs syncBuffer
	cfg := testConfig(t.TempDir())
	st, err := OpenStore(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Cfg: cfg, Store: st, HS: NewHomeserver(base, "as-token", matrix.NewHTTPClient()),
		Clock: &fakeClock{t: t0}, Rand: newFixedRand(), Log: slog.New(slog.NewTextHandler(&logs, nil))}
	s.init()
	st.Update(func(st *State) error { st.RoomID = testRoom; return nil })
	s.cleanup(ctx, claudeLP)
	s.notice(ctx, testRoom, "hi")
	out := logs.String()
	if !strings.Contains(out, "cleanup failed") {
		t.Fatalf("log captured nothing: %s", out)
	}
	for _, bad := range []string{"user_id", "/_matrix", "http://"} {
		if strings.Contains(out, bad) {
			t.Errorf("log holds %q (I4): %s", bad, out)
		}
	}
}
