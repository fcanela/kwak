//go:build integration && linux

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/cli"
	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/listener"
	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/proc/osproc"
	"github.com/fcanela/kwak/internal/registrar"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/secrets/secretstest"
	"github.com/fcanela/kwak/internal/wire"
)

// TestE18_E26 runs scenarios E18 to E26 of spec 10.4 on one world. The registrar's idle close is
// an hour here, so agents stay open between steps; E21 shortens it on its own.
func TestE18_E26(t *testing.T) {
	w := Start(t, WithRegistrarConfig(func(c *registrar.Config) { c.IdleAfter = registrar.Duration(time.Hour) }))
	if w == nil {
		return
	}
	a := w.Alice.NewHarness(t, wire.HarnessClaude)
	a.Begin(t)
	b := w.Bob.NewHarness(t, wire.HarnessCodex)
	b.Begin(t)
	aID, bID := w.Alice.WaitAgent(t, a), w.Bob.WaitAgent(t, b)
	w.WaitMember(t, aID, true)
	w.WaitMember(t, bID, true)
	steps := []struct {
		name string
		f    func(t *testing.T)
	}{
		{"E18_handover_race", func(t *testing.T) { e18(t, w, b) }},
		{"E19_single_flight", func(t *testing.T) { e19(t, w, a, bID, aID) }},
		{"E20_repair_live", func(t *testing.T) { e20(t, w, a, bID, aID) }},
		{"E23_mcp_restart", func(t *testing.T) { e23(t, w, a, aID) }},
		{"E24_codex_shell", func(t *testing.T) { e24(t, w, a) }},
		{"E26_misbinding", func(t *testing.T) { e26(t, w) }},
		{"E21_idle_reopen", func(t *testing.T) { e21(t, w, a, b, aID) }},
		{"E25_pending_crash", func(t *testing.T) { e25(t, w) }},
		{"E22_listener_kill", func(t *testing.T) { e22(t, w, a, b, aID) }},
	}
	for _, s := range steps {
		t.Run(s.name, s.f)
	}
}

// E18: two sessions start at once in the cwd of an ended session with held messages. The items
// move once, to the earlier one; the ended session's resume shows the hand-over note.
func e18(t *testing.T, w *World, b *FakeHarness) {
	o := w.Alice
	e0 := o.NewHarness(t, wire.HarnessClaude)
	e0.Begin(t)
	e0ID := o.WaitAgent(t, e0)
	if out := e0.Hook(t, wire.EventSessionEnd, nil); len(out) != 0 {
		t.Fatalf("SessionEnd printed %q", out)
	}
	Eventually(t, "the end of "+e0.SessionID, func() bool {
		s, ok := o.Session(t, e0)
		return !ok || !s.Ended.IsZero()
	})
	m1 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E18: held one", "to": []string{e0ID}}))
	m2 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E18: held two", "to": []string{e0ID}}))
	o.WaitInbox(t, e0, 2)

	y := o.NewHarnessWith(t, wire.HarnessClaude, UUIDv4(), e0.Cwd)
	z := o.NewHarnessWith(t, wire.HarnessClaude, UUIDv4(), e0.Cwd)
	var ry, rz fakeResp
	t.Run("at_once", func(t *testing.T) {
		t.Run("y", func(t *testing.T) { t.Parallel(); ry = y.HookResult(t, wire.EventSessionStart, nil) })
		t.Run("z", func(t *testing.T) { t.Parallel(); rz = z.HookResult(t, wire.EventSessionStart, nil) })
	})
	if t.Failed() {
		return
	}
	win, lose, wout, lout := y, z, ry.Stdout, rz.Stdout
	if len(wout) == 0 {
		win, lose, wout, lout = z, y, rz.Stdout, ry.Stdout
	}
	if len(lout) != 0 {
		t.Fatalf("both sessions got the held items:\n%s\n%s", wout, lout)
	}
	note := "addressed to your earlier agent " + Short(e0ID) + " (that session ended)"
	i1, i2 := w.Item(t, m1, render.Mention, ""), w.Item(t, m2, render.Mention, "")
	i1.Note, i2.Note = note, note
	SameJSON(t, "the winner's SessionStart", wout, ContextJSON("SessionStart", Rendered("this session", wire.HarnessClaude, i1, i2)))
	sw, _ := o.Session(t, win)
	sl, _ := o.Session(t, lose)
	if sw.Started.After(sl.Started) {
		t.Fatalf("the items went to the later session: winner started %s, loser %s", sw.Started, sl.Started)
	}
	if out := lose.Hook(t, wire.EventPrompt, nil); len(out) != 0 {
		t.Fatalf("the other session's prompt printed %q", out)
	}
	x := o.NewHarnessWith(t, wire.HarnessClaude, UUIDv4(), e0.Cwd)
	if out := x.Hook(t, wire.EventSessionStart, nil); len(out) != 0 {
		t.Fatalf("a third session in that cwd got %q; the items must move once", out)
	}

	before := o.Clock.Now()
	out := e0.Hook(t, wire.EventSessionStart, map[string]any{"source": "resume"})
	after := o.Clock.Now()
	body := "2 messages for you were handed to session " + win.SessionID + " in this directory on " + before.UTC().Format("2006-01-02") +
		" (threads " + m1 + ", " + m2 + "). Read them with the kwak thread tool."
	match := false
	for _, at := range []time.Time{before, after} {
		want := ContextJSON("SessionStart", Rendered(e0ID, wire.HarnessClaude, render.Item{SentAt: at, Reason: render.Notice, Body: body}))
		match = match || bytes.Equal(out, want)
	}
	if !match {
		t.Fatalf("the resumed session's SessionStart: %s\nwant the note %q", out, body)
	}
	if out := e0.Hook(t, wire.EventPrompt, nil); len(out) != 0 {
		t.Fatalf("the resumed session's prompt printed %q", out)
	}
	for _, h := range []*FakeHarness{e0, y, z, x} {
		h.Hook(t, wire.EventSessionEnd, nil)
	}
}

// E19: after a listener restart found A's agent closed (a read-only ping, no re-mint), a tool call
// and two SessionStart ensures for one session at once. The registrar holds the
// first login, so all three are in flight together.
func e19(t *testing.T, w *World, a *FakeHarness, bID, aID string) {
	o := w.Alice
	lp := e2xLP(aID)
	release := make(chan struct{})
	first := make(chan struct{})
	var logins atomic.Int32
	gate := &e2xDoer{next: w.Env.HTTP, around: func(ctx context.Context, c e2xCall, fwd func() (*http.Response, error)) (*http.Response, error) {
		if c.Method == "POST" && strings.HasSuffix(c.Path, "/login") && c.User == lp {
			if logins.Add(1) == 1 {
				close(first)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
		}
		return fwd()
	}}
	w.Reg.Stop(t)
	w.Reg.e2xStartWith(t, gate)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	o.StopListener(t)
	rc := cli.ListenerRegistrar(o.ConfigPath(), w.HTTP)
	rc.SetKey(o.Key())
	if err := rc.Close(t.Context(), []string{aID}, wire.ReasonSessionEnded); err != nil {
		t.Fatalf("close A at the registrar: %v", err)
	}
	w.WaitMember(t, aID, false)
	t0 := time.Now()
	o.StartListener(t)
	e2xWarm(t, a)
	if logins.Load() != 0 {
		t.Fatal("the restart re-minted A with no use")
	}
	id := a.e2xSendTool(t, wire.ToolSend, map[string]any{"text": "E19: sent while the mint is held", "to": []string{bID}})
	select {
	case <-first:
	case <-time.After(Wait):
		t.Fatal("the tool call never minted A")
	}
	a.HookResult(t, wire.EventSessionStart, map[string]any{"source": "resume"})
	a.HookResult(t, wire.EventSessionStart, map[string]any{"source": "resume"})
	time.Sleep(time.Second)
	close(release)
	res := a.e2xRecvTool(t, id)
	if res.IsError || !strings.HasPrefix(res.Text, "Sent as "+Short(aID)+" in thread ") {
		t.Fatalf("send during the held mint: %+v", res)
	}
	time.Sleep(2 * time.Second)
	if n := logins.Load(); n != 1 {
		t.Fatalf("A logged in %d times; the three asks must share one mint", n)
	}
	if got := w.Alice.WaitAgent(t, a); got != aID {
		t.Fatalf("A's agent is %s, want %s", got, aID)
	}
	hex, _ := wire.SessionHex(a.SessionID)
	var recs []string
	w.Reg.View(func(st *registrar.State) {
		for l, r := range st.Agents {
			if r.Owner == "alice" && r.Session == hex && r.Helper == "" {
				recs = append(recs, l)
			}
		}
	})
	if len(recs) != 1 || recs[0] != lp {
		t.Fatalf("agent records for A's session: %v, want [%s]", recs, lp)
	}
	if r := w.Reg.Agent(lp); r == nil || !r.Open() {
		t.Fatalf("A's record after the mint: %+v", r)
	}
	if devs := w.Devices(t, aID); len(devs) != 1 {
		t.Fatalf("A has devices %v, want one", devs)
	}
	tok := e2xTokenOf(t, w, t0, aID)
	if tok == "" {
		t.Fatal("found no live token on A's send")
	}
	_, tok2 := e2xSendToken(t, w, a, aID, bID, "E19: second send")
	if tok2 != tok {
		t.Fatal("A's second send used another token, so a mint ran after the tool's")
	}
	w.WaitMember(t, aID, true)
}

// E20: alice re-pairs while A is live. A keeps its name and token, the old key gets 401, the old
// pairing is revoked as replaced, and the listener never shows the unpaired state.
func e20(t *testing.T, w *World, a *FakeHarness, bID, aID string) {
	o := w.Alice
	oldPairing, oldKey := o.Config(t).PairingID, o.Key()
	_, tok := e2xSendToken(t, w, a, aID, bID, "E20: before the re-pair")

	var mu sync.Mutex
	var seen []string
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var s wire.StatusResponse
			ctx, cancel := context.WithTimeout(w.Ctx, 3*time.Second)
			err := cli.SocketCaller(o.Socket)(ctx, wire.Head{V: wire.ProtocolVersion, Op: wire.OpStatus}, &s)
			cancel()
			mu.Lock()
			if err != nil {
				seen = append(seen, "error: "+err.Error())
			} else {
				seen = append(seen, s.State+" "+s.PairingID+" "+s.Detail)
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	p := o.Pair(t)
	time.Sleep(3 * time.Second)
	close(stop)
	<-done
	mu.Lock()
	for _, s := range seen {
		if !strings.HasPrefix(s, wire.StateSyncing+" ") && !strings.HasPrefix(s, wire.StateConnecting+" ") {
			t.Errorf("listener state during the re-pair: %q", s)
		}
	}
	t.Logf("%d status reads during the re-pair", len(seen))
	mu.Unlock()
	if p.PairingID == oldPairing || p.Key == oldKey {
		t.Fatalf("the re-pair kept pairing %s", p.PairingID)
	}
	if s := o.Status(t); s.State != wire.StateSyncing || s.PairingID != p.PairingID {
		t.Fatalf("status after the re-pair: %+v", s)
	}
	if code, ec := e2xMe(t, w, oldKey); code != 401 || ec != wire.ErrBadKey {
		t.Fatalf("GET /me with the old key: %d %s", code, ec)
	}
	if code, _ := e2xMe(t, w, p.Key); code != 200 {
		t.Fatalf("GET /me with the new key: %d", code)
	}
	var old registrar.Pairing
	w.Reg.View(func(st *registrar.State) {
		if x := st.Pairings[oldPairing]; x != nil {
			old = *x
		}
	})
	if old.Revoked == nil || old.RevokeReason != "replaced" {
		t.Fatalf("the old pairing: revoked %v, reason %q", old.Revoked, old.RevokeReason)
	}
	r := w.Reg.Agent(e2xLP(aID))
	if r == nil || r.Pairing != p.PairingID || !r.Open() {
		t.Fatalf("A's record after the re-pair: %+v", r)
	}
	if got := w.Alice.WaitAgent(t, a); got != aID {
		t.Fatalf("A's agent after the re-pair: %s", got)
	}
	if code, uid := e2xWhoami(t, w, tok); code != 200 || uid != aID {
		t.Errorf("A's token from before the re-pair: whoami %d %q; spec 4 step 6 and E20 say it survives", code, uid)
	}
	_, tok2 := e2xSendToken(t, w, a, aID, bID, "E20: after the re-pair")
	if tok2 != tok {
		t.Errorf("A sends with a new token after the re-pair; spec 4 step 6 and E20 say it keeps its token")
	}
}

// E23: A's MCP server is killed and restarted while the fake harness lives. The session stays
// live, the agent stays open, and the tools work again.
func e23(t *testing.T, w *World, a *FakeHarness, aID string) {
	o := w.Alice
	a.StopMCP(t, true)
	EventuallyMsg(t, "A without a tools connection", func() (bool, string) {
		s, ok := o.Session(t, a)
		return ok && !s.Tools && s.Ended.IsZero(), fmt.Sprintf("%v %+v", ok, s)
	})
	a.StartMCP(t)
	// kwak mcp dials the listener at its first tool call.
	if st := a.MustCall(t, wire.ToolStatus, map[string]any{}); !strings.Contains(st, "Agent: "+Short(aID)+"\n") {
		t.Fatalf("status tool after the MCP restart: %q", st)
	}
	if s, ok := o.Session(t, a); !ok || !s.Tools || !s.Ended.IsZero() {
		t.Fatalf("A after the MCP restart: %v %+v", ok, s)
	}
	o.Clock.Advance(11 * time.Minute)
	time.Sleep(12 * time.Second)
	s, ok := o.Session(t, a)
	if !ok || !s.Ended.IsZero() || s.Agent != aID {
		t.Fatalf("A after the close grace would have passed: %v %+v", ok, s)
	}
	if r := w.Reg.Agent(e2xLP(aID)); r == nil || !r.Open() {
		t.Fatalf("A's agent was closed: %+v", r)
	}
	if !slices.Contains(w.Members(t), aID) {
		t.Fatal("A left the room")
	}
	if res := a.MustCall(t, wire.ToolSend, map[string]any{"text": "E23: tools work again"}); !strings.HasPrefix(res, "Sent as "+Short(aID)) {
		t.Fatalf("send after the MCP restart: %q", res)
	}
}

// E24: a Codex session whose hooks run under the shell, with unknown and mistyped stdin fields.
// Hooks work, and the harness process is the fake harness, not the shell.
func e24(t *testing.T, w *World, a *FakeHarness) {
	o := w.Bob
	c := o.NewHarness(t, wire.HarnessCodex)
	extra := map[string]any{"turn_id": "turn-7", "kwak_e2e_unknown": map[string]any{"nested": []int{1, 2}}, "client": "codex-tui"}
	if r := c.HookResult(t, wire.EventSessionStart, extra); len(r.Stdout) != 0 {
		t.Fatalf("SessionStart printed %q", r.Stdout)
	}
	if out := c.Hook(t, wire.EventPostTool, extra); len(out) != 0 {
		t.Fatalf("post-tool printed %q", out)
	}
	key := string(wire.HarnessCodex) + ":" + c.SessionID
	var last string
	EventuallyMsg(t, "harness_proc in bob's state.json", func() (bool, string) {
		hp, cands, err := e2xHarnessProc(o, key)
		last = fmt.Sprintf("harness_proc %d, candidates %v, %v", hp, cands, err)
		return err == nil && hp != 0, last
	})
	hp, cands, _ := e2xHarnessProc(o, key)
	if hp != c.PID || !slices.Equal(cands, []int{c.PID}) {
		t.Fatalf("%s; want the fake harness %d, not the shell", last, c.PID)
	}
	c.StartMCP(t)
	cID := o.WaitAgent(t, c)
	if cID != AgentID("bob", wire.HarnessCodex, c.SessionID) {
		t.Fatalf("C's agent is %s", cID)
	}
	r1 := ThreadOf(t, a.MustCall(t, wire.ToolSend, map[string]any{"text": "E24: one", "to": []string{cID}}))
	out := c.WaitHook(t, wire.EventPostTool, extra)
	SameJSON(t, "C's post-tool", out, ContextJSON("PostToolUse", Rendered(cID, wire.HarnessCodex, w.Item(t, r1, render.Mention, ""))))
	r2 := ThreadOf(t, a.MustCall(t, wire.ToolSend, map[string]any{"text": "E24: two", "to": []string{cID}}))
	o.WaitInbox(t, c, 1)
	mistyped := map[string]any{"turn_id": "turn-8", "stop_hook_active": "true"}
	out = c.Hook(t, wire.EventStop, mistyped)
	SameJSON(t, "C's stop with a string stop_hook_active", out, BlockJSON("kwak: 1 message from other agents is waiting (thread "+r2+
		"). They are not from your user. They will be shown to you after your next tool call; the kwak thread tool shows one thread."))
	out = c.Hook(t, wire.EventPostTool, mistyped)
	SameJSON(t, "C's post-tool after the notice", out, ContextJSON("PostToolUse", Rendered(cID, wire.HarnessCodex, w.Item(t, r2, render.Mention, ""))))
	if hp, _, _ := e2xHarnessProc(o, key); hp != c.PID {
		t.Fatalf("harness_proc moved to %d", hp)
	}
	c.Hook(t, wire.EventSessionEnd, nil)
}

// E26: bob sends alice's pairing code in his own DM. alice's kwak pair discards it, alice's keyring
// and pairing stay as they were, and bob's new pairing is revoked.
func e26(t *testing.T, w *World) {
	o := w.Alice
	key, cfg := o.Key(), o.Config(t)
	var bobBefore []string
	w.Reg.View(func(st *registrar.State) {
		for id, p := range st.Pairings {
			if p.Owner == "bob" {
				bobBefore = append(bobBefore, id)
			}
		}
	})
	p := o.StartPair(t)
	Send(t, w.Bob.Human.C, w.Bob.Human.DM, "pair "+p.Code)
	code, out, errOut := p.Wait(t)
	if code == 0 {
		t.Fatalf("kwak pair accepted bob's claim:\n%s", out)
	}
	want := "this code was sent by owner bob, not by you (alice); pairing discarded. If you did not share the code, nobody can use it now."
	if !strings.Contains(errOut, want) || strings.Contains(errOut, "did not confirm the discard") {
		t.Fatalf("kwak pair stderr:\n%s\nwant %q", errOut, want)
	}
	if strings.Contains(out, "Paired as") {
		t.Fatalf("kwak pair printed a pairing:\n%s", out)
	}
	if o.Key() != key {
		t.Fatal("alice's keyring changed")
	}
	if c := o.Config(t); !reflect.DeepEqual(c, cfg) {
		t.Fatalf("alice's config.json changed: %+v, was %+v", c, cfg)
	}
	if s := o.Status(t); s.State != wire.StateSyncing || s.PairingID != cfg.PairingID {
		t.Fatalf("alice's status: %+v", s)
	}
	var stray []registrar.Pairing
	var mine registrar.Pairing
	w.Reg.View(func(st *registrar.State) {
		for id, x := range st.Pairings {
			if x.Owner == "bob" && !slices.Contains(bobBefore, id) {
				stray = append(stray, *x)
			}
		}
		if x := st.Pairings[cfg.PairingID]; x != nil {
			mine = *x
		}
	})
	if len(stray) != 1 || stray[0].Machine != o.Machine || stray[0].Revoked == nil {
		t.Fatalf("bob's pairing from alice's code: %+v", stray)
	}
	if mine.Revoked != nil || mine.Owner != "alice" {
		t.Fatalf("alice's pairing: %+v", mine)
	}
	if s := w.Bob.Status(t); s.State != wire.StateSyncing || s.PairingID != w.Bob.Config(t).PairingID {
		t.Fatalf("bob's status: %+v", s)
	}
}

// E21: A is idle-closed by the registrar's job while its session lives; a message for A waits; A
// resumes, gets the same name back, and SessionStart prints the message.
func e21(t *testing.T, w *World, a, b *FakeHarness, aID string) {
	lp := e2xLP(aID)
	w.Reg.Stop(t)
	w.Reg.Cfg.IdleAfter = registrar.Duration(3 * time.Second)
	w.Reg.Start(t)
	Eventually(t, "the idle close of "+lp, func() bool {
		r := w.Reg.Agent(lp)
		return r != nil && r.Closed != nil && r.CloseReason == "idle"
	})
	w.WaitMember(t, aID, false)
	w.Reg.Stop(t)
	w.Reg.Cfg.IdleAfter = registrar.Duration(time.Hour)
	w.Reg.Start(t)

	// The "not in the room" line depends on bob's 60 s member cache (spec 5.9), so it is not checked.
	root := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E21: a question while you are closed", "to": []string{aID}}))
	w.Alice.WaitInbox(t, a, 1)
	out := a.Hook(t, wire.EventSessionStart, map[string]any{"source": "resume"})
	SameJSON(t, "A's SessionStart on resume", out, ContextJSON("SessionStart", Rendered(aID, wire.HarnessClaude, w.Item(t, root, render.Mention, ""))))
	w.WaitMember(t, aID, true)
	Eventually(t, "A's record reopened", func() bool {
		r := w.Reg.Agent(lp)
		return r != nil && r.Open()
	})
	if got := w.Alice.WaitAgent(t, a); got != aID {
		t.Fatalf("A came back as %s", got)
	}
	if st := a.MustCall(t, wire.ToolStatus, map[string]any{}); !strings.Contains(st, "Agent: "+Short(aID)+"\n") {
		t.Fatalf("status tool after the reopen: %q", st)
	}
}

// E25: the registrar stops after writing pending_since and after tuwunel registered the account,
// before /register returns. The restarted registrar completes the agent with a login, same name.
func e25(t *testing.T, w *World) {
	o := w.Alice
	h := o.NewHarness(t, wire.HarnessClaude)
	uid := AgentID("alice", wire.HarnessClaude, h.SessionID)
	lp := e2xLP(uid)
	crashed, release := make(chan struct{}), make(chan struct{})
	var down atomic.Bool
	crash := &e2xDoer{next: w.Env.HTTP, around: func(ctx context.Context, c e2xCall, fwd func() (*http.Response, error)) (*http.Response, error) {
		if down.Load() {
			return nil, errors.New("e2e: this registrar is gone")
		}
		if c.Method == "POST" && strings.HasSuffix(c.Path, "/register") && c.User == lp {
			resp, err := fwd()
			if resp != nil {
				resp.Body.Close()
			}
			down.Store(true)
			close(crashed)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, fmt.Errorf("e2e: the registrar stopped mid-register (tuwunel said %v)", err)
		}
		return fwd()
	}}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	w.Reg.Stop(t)
	w.Reg.e2xStartWith(t, crash)
	if r := h.HookResult(t, wire.EventSessionStart, nil); len(r.Stdout) != 0 {
		t.Fatalf("SessionStart printed %q", r.Stdout)
	}
	// The session asks for its agent at its first send (decision 48). That send fails while the
	// registrar is down; the listener keeps asking.
	h.StartMCP(t)
	if r := h.Call(t, wire.ToolSend, map[string]any{"text": "E25: the first send asks for the agent"}); !r.IsError {
		t.Fatalf("the first send went through while the registrar was down: %+v", r)
	}
	select {
	case <-crashed:
	case <-time.After(Wait):
		t.Fatal("the listener never asked for the agent")
	}
	w.Reg.Stop(t)
	var disk struct {
		Agents map[string]struct {
			PendingSince *time.Time `json:"pending_since"`
			Closed       *time.Time `json:"closed"`
		} `json:"agents"`
	}
	raw, err := os.ReadFile(w.Reg.Cfg.State)
	if err != nil || json.Unmarshal(raw, &disk) != nil {
		t.Fatalf("read the registrar state: %v", err)
	}
	if r, ok := disk.Agents[lp]; !ok || r.PendingSince == nil || r.Closed != nil {
		t.Fatalf("state.json after the stop has %s as %+v, want pending", lp, r)
	}
	if code, _ := w.HS(t, "GET", "/_matrix/client/v3/profile/"+url.PathEscape(uid), w.Admin.Login.AccessToken, ""); code != 200 {
		t.Fatalf("tuwunel has no account %s (%d), so the crash came too early", uid, code)
	}

	rec := &e2xDoer{next: w.Env.HTTP}
	w.Reg.e2xStartWith(t, rec)
	close(release)
	var got string
	Eventually(t, "the completed agent", func() bool {
		st, _ := o.Session(t, h)
		got = st.Agent
		return got != ""
	})
	if got != uid {
		t.Fatalf("the completed agent is %s, want %s", got, uid)
	}
	r := w.Reg.Agent(lp)
	if r == nil || !r.Open() || r.Pairing != o.Config(t).PairingID {
		t.Fatalf("the completed record: %+v", r)
	}
	var steps []string
	for _, c := range rec.calls() {
		if c.User == lp && (strings.HasSuffix(c.Path, "/register") || strings.HasSuffix(c.Path, "/login")) {
			steps = append(steps, fmt.Sprintf("%s %d %s", c.Path[strings.LastIndex(c.Path, "/")+1:], c.Status, c.Errcode))
		}
	}
	if !slices.Equal(steps, []string{"register 400 M_USER_IN_USE", "login 200 "}) {
		t.Fatalf("the restarted registrar did %q, want a register that finds the account, then a login", steps)
	}
	w.WaitMember(t, uid, true)
	if dn := w.DisplayName(t, uid); dn != lp {
		t.Fatalf("display name %q", dn)
	}
	if st := h.MustCall(t, wire.ToolStatus, map[string]any{}); !strings.Contains(st, "Agent: "+Short(uid)+"\n") {
		t.Fatalf("status tool: %q", st)
	}
	h.Hook(t, wire.EventSessionEnd, nil)
}

// E22: alice's listener runs as a child process and is SIGKILLed right after a drain reply, then
// right after a sync batch. After each restart no item shows twice and none is lost.
func e22(t *testing.T, w *World, a, b *FakeHarness, aID string) {
	o := w.Alice
	for len(a.Hook(t, wire.EventPrompt, nil)) > 0 {
	}
	o.StopListener(t)
	var ch *e2xChild
	defer func() {
		if ch != nil {
			ch.kill()
		}
		o.StartListener(t)
		o.WaitSyncing(t)
	}()
	cert := e2xEdgeCert(t, w)
	ch = e2xStartChild(t, w, o, cert)
	o.WaitSyncing(t)
	o.WaitInbox(t, a, 0)

	m1 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E22: before the first kill", "to": []string{aID}}))
	o.WaitInbox(t, a, 1)
	out := a.Hook(t, wire.EventPrompt, nil)
	ch.kill()
	SameJSON(t, "A's prompt before the kill", out, ContextJSON("UserPromptSubmit", Rendered(aID, wire.HarnessClaude, w.Item(t, m1, render.Mention, ""))))
	m2 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E22: while the listener is dead", "to": []string{aID}}))
	ch = e2xStartChild(t, w, o, cert)
	o.WaitSyncing(t)
	out = a.WaitHook(t, wire.EventPrompt, nil)
	SameJSON(t, "A's prompt after the first restart", out, ContextJSON("UserPromptSubmit", Rendered(aID, wire.HarnessClaude, w.Item(t, m2, render.Mention, ""))))
	if out := a.Hook(t, wire.EventPrompt, nil); len(out) != 0 {
		t.Fatalf("A's next prompt printed %q", out)
	}

	m3 := ThreadOf(t, b.MustCall(t, wire.ToolSend, map[string]any{"text": "E22: before the second kill", "to": []string{aID}}))
	o.WaitInbox(t, a, 1)
	ch.kill()
	ch = e2xStartChild(t, w, o, cert)
	o.WaitSyncing(t)
	out = a.WaitHook(t, wire.EventPrompt, nil)
	SameJSON(t, "A's prompt after the second restart", out, ContextJSON("UserPromptSubmit", Rendered(aID, wire.HarnessClaude, w.Item(t, m3, render.Mention, ""))))
	time.Sleep(2 * time.Second)
	if out := a.Hook(t, wire.EventPrompt, nil); len(out) != 0 {
		t.Fatalf("A's prompt after the second restart printed again %q", out)
	}
	ch.stop(t)
}

// ---- helpers for E18 to E26 ----

func e2xLP(uid string) string { return strings.TrimPrefix(Short(uid), "@") }

// e2xStartWith is Registrar.Start with doer as the registrar's HTTP client to tuwunel.
func (r *Registrar) e2xStartWith(t *testing.T, doer matrix.Doer) {
	t.Helper()
	store, err := registrar.OpenStore(r.Cfg.State)
	if err != nil {
		t.Fatalf("open the registrar store: %v", err)
	}
	r.Store = store
	r.Srv = &registrar.Server{
		Cfg: r.Cfg, Store: store,
		HS:    registrar.NewHomeserver(r.Cfg.Homeserver, r.w.Env.Registration.ASToken, doer),
		Clock: r.Clock, Rand: rand.Reader, Log: r.w.Log("registrar"),
	}
	api, as := r.Srv.API(), r.Srv.Appservice()
	r.api.Store(&api)
	r.as.Store(&as)
	if err := r.Srv.Start(r.w.Ctx); err != nil {
		t.Fatalf("registrar start: %v", err)
	}
	ctx, cancel := context.WithCancel(r.w.Ctx)
	r.cancel, r.done = cancel, make(chan struct{})
	go func() { r.Srv.RunJobs(ctx); close(r.done) }()
}

// e2xCall is one registrar request to tuwunel, as e2xDoer saw it.
type e2xCall struct {
	Method, Path, User string // User: the localpart the call is for, if any
	Status             int
	Errcode            string
}

// e2xDoer sits between the registrar and tuwunel: it records calls, and around may hold or
// replace one.
type e2xDoer struct {
	next   matrix.Doer
	around func(ctx context.Context, c e2xCall, fwd func() (*http.Response, error)) (*http.Response, error)
	mu     sync.Mutex
	log    []e2xCall
}

func (d *e2xDoer) Do(req *http.Request) (*http.Response, error) {
	c := e2xCall{Method: req.Method, Path: req.URL.Path, User: e2xUser(req)}
	fwd := func() (*http.Response, error) { return d.next.Do(req) }
	var resp *http.Response
	var err error
	if d.around != nil {
		resp, err = d.around(req.Context(), c, fwd)
	} else {
		resp, err = fwd()
	}
	if resp != nil {
		c.Status = resp.StatusCode
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var e struct {
				Errcode string `json:"errcode"`
			}
			json.Unmarshal(b, &e)
			c.Errcode = e.Errcode
			resp.Body = io.NopCloser(bytes.NewReader(b))
		}
	}
	d.mu.Lock()
	d.log = append(d.log, c)
	d.mu.Unlock()
	return resp, err
}

func (d *e2xDoer) calls() []e2xCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.log)
}

// e2xUser reads the localpart a request acts for: the register username, the login user, or
// ?user_id=.
func e2xUser(req *http.Request) string {
	u := req.URL.Query().Get("user_id")
	if req.Body != nil && req.Body != http.NoBody {
		b, _ := io.ReadAll(req.Body)
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(b))
		var v struct {
			Username   string `json:"username"`
			Identifier struct {
				User string `json:"user"`
			} `json:"identifier"`
		}
		if json.Unmarshal(b, &v) == nil {
			if v.Username != "" {
				u = v.Username
			}
			if v.Identifier.User != "" {
				u = v.Identifier.User
			}
		}
	}
	return strings.TrimSuffix(strings.TrimPrefix(u, "@"), ":"+Server)
}

// e2xWarm calls the status tool until the MCP server has reconnected to a restarted listener.
func e2xWarm(t *testing.T, f *FakeHarness) {
	t.Helper()
	var last Tool
	EventuallyMsg(t, "the MCP server back on the listener", func() (bool, string) {
		last = f.Call(t, wire.ToolStatus, map[string]any{})
		return !last.IsError, last.Text
	})
}

// e2xSendTool sends a tools/call without waiting for the answer; e2xRecvTool reads it. The listener
// works on the call while the test runs a hook.
func (f *FakeHarness) e2xSendTool(t *testing.T, tool string, args any) int {
	t.Helper()
	f.mu.Lock()
	id := f.nextID
	f.nextID++
	f.mu.Unlock()
	p := map[string]any{"name": tool, "arguments": args}
	if f.Meta != nil {
		p["_meta"] = f.Meta
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": p})
	if r := f.do(t, fakeReq{Op: "mcp_send", Line: b}); r.Err != "" {
		t.Fatalf("mcp_send: %s", r.Err)
	}
	return id
}

func (f *FakeHarness) e2xRecvTool(t *testing.T, id int) Tool {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		r := f.do(t, fakeReq{Op: "mcp_recv", Timeout: time.Until(deadline)})
		if r.Err != "" {
			t.Fatalf("mcp_recv: %s", r.Err)
		}
		f.w.cap.add(string(f.Harness)+" mcp", r.Line)
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Content []struct{ Type, Text string } `json:"content"`
				IsError bool                          `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(r.Line, &resp); err != nil {
			t.Fatalf("mcp line %q: %v", r.Line, err)
		}
		if string(resp.ID) != fmt.Sprint(id) {
			continue
		}
		if len(resp.Result.Content) != 1 {
			t.Fatalf("tools/call answer %s", r.Line)
		}
		return Tool{Text: resp.Result.Content[0].Text, IsError: resp.Result.IsError}
	}
	t.Fatalf("no answer to tools/call %d", id)
	return Tool{}
}

// e2xWhoami asks tuwunel who token belongs to.
func e2xWhoami(t *testing.T, w *World, token string) (int, string) {
	t.Helper()
	code, b := w.HS(t, "GET", "/_matrix/client/v3/account/whoami", token, "")
	var v struct {
		UserID string `json:"user_id"`
	}
	json.Unmarshal(b, &v)
	return code, v.UserID
}

// e2xTokenOf returns the live token of uid among the message sends the edge passed since t0.
func e2xTokenOf(t *testing.T, w *World, t0 time.Time, uid string) string {
	t.Helper()
	tried := map[string]bool{}
	for _, r := range w.Requests(t0) {
		if r.Method != "PUT" || !strings.Contains(r.Path, "/rooms/"+w.Room+"/send/") || r.Token == "" || tried[r.Token] {
			continue
		}
		tried[r.Token] = true
		if code, who := e2xWhoami(t, w, r.Token); code == 200 && who == uid {
			return r.Token
		}
	}
	return ""
}

// e2xSendToken sends from h to to and returns the thread and the token h's agent used.
func e2xSendToken(t *testing.T, w *World, h *FakeHarness, me, to, text string) (string, string) {
	t.Helper()
	t0 := time.Now()
	res := h.MustCall(t, wire.ToolSend, map[string]any{"text": text, "to": []string{to}})
	tok := e2xTokenOf(t, w, t0, me)
	if tok == "" {
		t.Fatalf("no live token on %s's send", me)
	}
	return ThreadOf(t, res), tok
}

// e2xMe calls GET /_kwak/v1/me through the edge with key and returns the status and errcode.
func e2xMe(t *testing.T, w *World, key string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "GET", w.EdgeURL+wire.APIPrefix+wire.PathMe, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := w.HTTP.Do(req)
	if err != nil {
		t.Fatalf("GET /me: %v", err)
	}
	defer resp.Body.Close()
	var e struct {
		Errcode string `json:"errcode"`
	}
	json.NewDecoder(resp.Body).Decode(&e)
	return resp.StatusCode, e.Errcode
}

// e2xHarnessProc reads a session's harness_proc pid and candidate pids from the owner's state.json.
func e2xHarnessProc(o *Owner, key string) (int, []int, error) {
	raw, err := os.ReadFile(filepath.Join(o.StateDir, "state.json"))
	if err != nil {
		return 0, nil, err
	}
	var st struct {
		Sessions map[string]struct {
			Candidates []struct {
				PID int `json:"pid"`
			} `json:"candidates"`
			HarnessProc *struct {
				PID int `json:"pid"`
			} `json:"harness_proc"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return 0, nil, err
	}
	s, ok := st.Sessions[key]
	if !ok {
		return 0, nil, errors.New("no session " + key)
	}
	var cands []int
	for _, c := range s.Candidates {
		cands = append(cands, c.PID)
	}
	if s.HarnessProc == nil {
		return 0, cands, nil
	}
	return s.HarnessProc.PID, cands, nil
}

// ---- E22's child listener (spec 10.4): the test binary with KWAK_IT_LISTENER=1 ----

const e2xEnvListener = "KWAK_IT_LISTENER"

func init() {
	if os.Getenv(e2xEnvListener) == "1" {
		os.Exit(e2xListenerMain())
	}
}

// e2xChildConf is the first stdin line of the child listener. The owner key travels only here.
type e2xChildConf struct {
	Config, State, Socket string
	KeyName, Key          string
	Cert                  []byte // the edge's certificate, DER
}

// e2xListenerMain runs the real listener as kwak listen does, until SIGTERM or stdin closes.
func e2xListenerMain() int {
	in := bufio.NewReader(os.Stdin)
	line, err := in.ReadBytes('\n')
	var c e2xChildConf
	if err != nil || json.Unmarshal(line, &c) != nil {
		fmt.Fprintln(os.Stderr, "listener child: bad config line")
		return 2
	}
	keys := secretstest.Mem()
	keys.Set(c.KeyName, c.Key)
	cert, err := x509.ParseCertificate(c.Cert)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listener child:", err)
		return 2
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	httpc := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).With("c", "listener-child")
	release, err := ipc.Lock(filepath.Join(c.State, "listen.lock"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "listener child: listen.lock:", err)
		return 1
	}
	defer release()
	if err := ipc.RemoveStale(c.Socket); err != nil {
		fmt.Fprintln(os.Stderr, "listener child:", err)
		return 1
	}
	ln, err := ipc.Listen(c.Socket, ipc.Policy{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "listener child:", err)
		return 1
	}
	cfgPath := c.Config
	d := listener.Deps{
		Secrets:   keys,
		Registrar: cli.ListenerRegistrar(cfgPath, httpc),
		Matrix: func(token string) listener.Matrix {
			base := ""
			if mc, err := listener.ReadMachineConfig(cfgPath); err == nil {
				base = mc.Server
			}
			return &matrix.Client{Base: base, Token: token, HTTP: httpc}
		},
		Procs: osproc.New(), PeerPID: ipc.PeerPID, Runner: &RecRunner{}, Clock: &Clock{}, Rand: rand.Reader, Log: log,
	}
	l, err := listener.New(listener.Config{ConfigPath: cfgPath}, c.State, d)
	if err != nil {
		ln.Close()
		fmt.Fprintln(os.Stderr, "listener child:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		io.Copy(io.Discard, in)
		stop()
	}()
	fmt.Println("ready")
	err = l.Run(ctx, ln)
	ln.Close()
	os.Remove(c.Socket)
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "listener child:", err)
		return 1
	}
	return 0
}

type e2xChild struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	exited chan struct{}
	code   int
}

// e2xStartChild starts alice's listener as a child process on o's dirs and socket.
func e2xStartChild(t *testing.T, w *World, o *Owner, cert []byte) *e2xChild {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^$")
	cmd.Env = append(withEnv(os.Environ(), envFake, ""), e2xEnvListener+"=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out := &lockedBuf{}
	cmd.Stdout, cmd.Stderr = out, w.sink
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the child listener: %v", err)
	}
	c := &e2xChild{cmd: cmd, in: in, exited: make(chan struct{})}
	go func() {
		c.code = exitCode(cmd.Wait())
		close(c.exited)
	}()
	w.top.Cleanup(c.kill)
	b, _ := json.Marshal(e2xChildConf{Config: o.ConfigPath(), State: o.StateDir, Socket: o.Socket,
		KeyName: o.KeyName(), Key: o.Key(), Cert: cert})
	if _, err := in.Write(append(b, '\n')); err != nil {
		t.Fatalf("write to the child listener: %v", err)
	}
	EventuallyMsg(t, "the child listener ready", func() (bool, string) {
		select {
		case <-c.exited:
			t.Fatalf("the child listener exited %d", c.code)
		default:
		}
		return strings.Contains(out.String(), "ready\n"), out.String()
	})
	return c
}

// kill SIGKILLs the child and waits for it. Safe to call twice.
func (c *e2xChild) kill() {
	c.cmd.Process.Signal(syscall.SIGKILL)
	<-c.exited
}

// stop closes the child's stdin, a graceful stop, and checks it exited 0.
func (c *e2xChild) stop(t *testing.T) {
	t.Helper()
	c.in.Close()
	select {
	case <-c.exited:
	case <-time.After(30 * time.Second):
		c.kill()
		t.Fatal("the child listener did not stop")
	}
	if c.code != 0 {
		t.Fatalf("the child listener exited %d", c.code)
	}
}

// e2xEdgeCert reads the edge's certificate, so the child listener can trust exactly it.
func e2xEdgeCert(t *testing.T, w *World) []byte {
	t.Helper()
	u, err := url.Parse(w.EdgeURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", u.Host, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("dial the edge: %v", err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].Raw
}
