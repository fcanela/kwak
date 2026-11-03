package listener

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/proc/proctest"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/secrets/secretstest"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

// ---- clock ----

type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waits   []waiter
	instant bool
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 || c.instant {
		ch <- c.now
		return ch
	}
	c.waits = append(c.waits, waiter{c.now.Add(d), ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	keep := c.waits[:0]
	for _, w := range c.waits {
		if w.at.After(c.now) {
			keep = append(keep, w)
			continue
		}
		w.ch <- c.now
	}
	c.waits = keep
}

func (c *fakeClock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waits)
}

// ---- registrar ----

type fakeReg struct {
	mu        sync.Mutex
	key       string
	keys      map[string]bool
	agentN    int
	tokens    []string
	reqs      []wire.AgentRequest
	closes    [][]string
	alives    [][]string
	touches   []bool
	closed    map[string]bool // Alive reports these closed
	agentErr  func(wire.AgentRequest) error
	standErr  error
	standings int
	gate      chan struct{}
	closeGate chan struct{} // Close waits on it, after counting the call in closing
	closing   int
	onAlive   func() // runs inside Alive, without the lock
	me        wire.Me
}

func newReg() *fakeReg {
	return &fakeReg{keys: map[string]bool{wiretest.OwnerKey: true}, closed: map[string]bool{},
		me: wire.Me{Owner: wiretest.Owner, PairingID: wiretest.PairingID, Machine: wiretest.Machine,
			KeyExpiresAt: at("2027-01-31T10:05:00Z"), Settings: wire.Settings{Resume: "off", QuestionTimeout: "15m"}}}
}

func (f *fakeReg) SetKey(k string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key = k
}

func (f *fakeReg) authLocked() error {
	if !f.keys[f.key] {
		return &wire.APIError{Status: 401, Errcode: wire.ErrBadKey, Message: "bad key"}
	}
	return nil
}

func (f *fakeReg) Standing(context.Context) (wire.Standing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.standings++
	if err := f.authLocked(); err != nil {
		return wire.Standing{}, err
	}
	if f.standErr != nil {
		return wire.Standing{}, f.standErr
	}
	return wire.Standing{UserID: wiretest.StandingUser, AccessToken: wiretest.StandingToken, DeviceID: "KWAK-" + wiretest.PairingID}, nil
}

func (f *fakeReg) Agent(ctx context.Context, r wire.AgentRequest) (wire.Agent, error) {
	f.mu.Lock()
	gate := f.gate
	f.reqs = append(f.reqs, r)
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return wire.Agent{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.authLocked(); err != nil {
		return wire.Agent{}, err
	}
	if f.agentErr != nil {
		if err := f.agentErr(r); err != nil {
			return wire.Agent{}, err
		}
	}
	hex, err := wire.SessionHex(r.Session)
	if err != nil {
		return wire.Agent{}, &wire.APIError{Status: 400, Errcode: wire.ErrBadSession}
	}
	lp := wire.Candidates(wiretest.Owner, r.Harness, hex)[0]
	if r.Helper != "" {
		lp, _ = wire.HelperLocalpart(lp, r.Helper)
	}
	f.agentN++
	tok := fmt.Sprintf("tok-agent-%04d", f.agentN)
	f.tokens = append(f.tokens, tok)
	return wire.Agent{UserID: "@" + lp + ":" + wiretest.ServerName, AccessToken: tok, DeviceID: "KWAK", RoomID: wiretest.RoomID}, nil
}

func (f *fakeReg) Close(_ context.Context, ids []string, reason string) error {
	f.mu.Lock()
	f.closing++
	gate := f.closeGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.authLocked(); err != nil {
		return err
	}
	f.closes = append(f.closes, ids)
	return nil
}

func (f *fakeReg) Alive(_ context.Context, ids []string, touch bool) ([]string, []string, error) {
	f.mu.Lock()
	hook := f.onAlive
	f.onAlive = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.authLocked(); err != nil {
		return nil, nil, err
	}
	if len(ids) > wire.MaxIDs {
		return nil, nil, &wire.APIError{Status: 400, Errcode: wire.ErrBadRequest}
	}
	f.alives = append(f.alives, ids)
	f.touches = append(f.touches, touch)
	var closed []string
	for _, id := range ids {
		if f.closed[id] {
			closed = append(closed, id)
		}
	}
	return nil, closed, nil
}

func (f *fakeReg) Me(context.Context) (wire.Me, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.authLocked(); err != nil {
		return wire.Me{}, err
	}
	return f.me, nil
}

func (f *fakeReg) Unpair(context.Context) error { return nil }

func (f *fakeReg) standingCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.standings
}

func (f *fakeReg) agentReqs() []wire.AgentRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wire.AgentRequest(nil), f.reqs...)
}

func (f *fakeReg) agentCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// ---- room ----

type sent struct {
	token, txn string
	content    json.RawMessage
}

type syncResult struct {
	resp *matrix.SyncResponse
	err  error
}

type fakeRoom struct {
	mu       sync.Mutex
	revoked  map[string]bool
	events   map[string]matrix.Event
	replies  map[string][]matrix.Event // root -> replies, oldest first
	members  []string
	sent     []sent
	ids      []string // event ids Send hands out, in order
	sendErr  []error  // errors Send returns first, in order
	eventErr error
	syncs    chan syncResult
	since    []string
	pages    map[string]*matrix.MessagesResponse
	pageErr  map[string]error
	froms    []string
	fetches  int
}

func newRoom() *fakeRoom {
	return &fakeRoom{revoked: map[string]bool{}, events: map[string]matrix.Event{}, replies: map[string][]matrix.Event{},
		syncs: make(chan syncResult, 64), pages: map[string]*matrix.MessagesResponse{}, pageErr: map[string]error{}}
}

func (r *fakeRoom) client(tok string) Matrix { return &fakeMatrix{r, tok} }

type fakeMatrix struct {
	r   *fakeRoom
	tok string
}

func (m *fakeMatrix) check() error {
	if m.r.revoked[m.tok] {
		return &matrix.Error{Status: 401, Code: matrix.ErrUnknownToken}
	}
	return nil
}

func (m *fakeMatrix) Sync(ctx context.Context, since, filter string, timeout time.Duration) (*matrix.SyncResponse, error) {
	m.r.mu.Lock()
	m.r.since = append(m.r.since, since)
	err := m.check()
	m.r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case s := <-m.r.syncs:
		return s.resp, s.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *fakeMatrix) Send(_ context.Context, roomID, txn string, content any) (string, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	if err := m.check(); err != nil {
		return "", err
	}
	if len(m.r.sendErr) > 0 {
		err := m.r.sendErr[0]
		m.r.sendErr = m.r.sendErr[1:]
		if err != nil {
			return "", err
		}
	}
	b, _ := json.Marshal(content)
	m.r.sent = append(m.r.sent, sent{m.tok, txn, b})
	id := fmt.Sprintf("$sent%d", len(m.r.sent))
	if len(m.r.ids) > 0 {
		id, m.r.ids = m.r.ids[0], m.r.ids[1:]
	}
	return id, nil
}

func (m *fakeMatrix) Messages(_ context.Context, roomID, from string, limit int, filter string) (*matrix.MessagesResponse, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	m.r.froms = append(m.r.froms, from)
	if err := m.r.pageErr[from]; err != nil {
		return nil, err
	}
	p := m.r.pages[from]
	if p == nil {
		return &matrix.MessagesResponse{}, nil
	}
	return p, nil
}

func (m *fakeMatrix) Event(_ context.Context, roomID, id string) (*matrix.Event, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	m.r.fetches++
	if err := m.check(); err != nil {
		return nil, err
	}
	if m.r.eventErr != nil {
		return nil, m.r.eventErr
	}
	ev, ok := m.r.events[id]
	if !ok {
		return nil, &matrix.Error{Status: 404, Code: matrix.ErrNotFound}
	}
	return &ev, nil
}

func (m *fakeMatrix) ThreadReplies(_ context.Context, roomID, root string, limit int) ([]matrix.Event, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	if err := m.check(); err != nil {
		return nil, err
	}
	rs := m.r.replies[root]
	out := make([]matrix.Event, 0, len(rs))
	for i := len(rs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, rs[i])
	}
	return out, nil
}

func (m *fakeMatrix) JoinedMembers(context.Context, string) ([]string, error) {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	return append([]string(nil), m.r.members...), nil
}

func (r *fakeRoom) sentContents() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, s := range r.sent {
		var m map[string]any
		json.Unmarshal(s.content, &m)
		out = append(out, m)
	}
	return out
}

// ---- runner ----

type run struct {
	dir  string
	argv []string
	done chan error
}

type fakeRunner struct {
	mu   sync.Mutex
	runs []*run
}

func (f *fakeRunner) Start(ctx context.Context, dir string, argv []string, log io.Writer) (func() error, error) {
	r := &run{dir: dir, argv: argv, done: make(chan error, 1)}
	f.mu.Lock()
	f.runs = append(f.runs, r)
	f.mu.Unlock()
	return func() error {
		select {
		case err := <-r.done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

// ---- environment ----

const (
	sid1 = wiretest.ClaudeSession
	sid2 = "9b1d3c5e-2f4a-4b6c-8d0e-1a2b3c4d5e6f"
	sid3 = "c0ffee00-1111-4222-8333-444455556666"
	cx1  = wiretest.CodexSession
	cx2  = "01a198dd-ad17-79f0-9999-000000000002"
)

var (
	mallory = "@agent.mallory.8261713321e78fea:" + wiretest.ServerName
	bob     = wiretest.OtherAgent
	human   = "@alice:" + wiretest.ServerName
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

type env struct {
	t        *testing.T
	l        *Listener
	clk      *fakeClock
	reg      *fakeReg
	room     *fakeRoom
	procs    *proctest.Reader
	sec      *secretstest.Memory
	runner   *fakeRunner
	dir      string
	confPath string
	pid      atomic.Int64
	pidOK    atomic.Bool
	dirs     map[string]bool
}

type opt func(*env, *Config)

func unpairedOpt(e *env, _ *Config) { os.Remove(e.confPath) }

func budgetOpt(h wire.Harness, n int) opt {
	return func(_ *env, c *Config) { c.Budget = map[wire.Harness]int{h: n} }
}

func machineConf() MachineConfig {
	return MachineConfig{Version: 1, Server: "https://" + wiretest.ServerName, ServerName: wiretest.ServerName,
		Owner: wiretest.Owner, Machine: wiretest.Machine, PairingID: wiretest.PairingID, RoomID: wiretest.RoomID,
		StandingUser: wiretest.StandingUser, ResumeMax: 2,
		ResumeExe: map[wire.Harness]string{wire.HarnessClaude: "/opt/bin/claude"}}
}

func newEnv(t *testing.T, opts ...opt) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, clk: newClock(at("2026-11-02T10:49:00Z")), reg: newReg(), room: newRoom(), procs: proctest.New(),
		sec: secretstest.Mem(), runner: &fakeRunner{}, dir: filepath.Join(root, "state"),
		confPath: filepath.Join(root, "config.json"), dirs: map[string]bool{}}
	e.pidOK.Store(true)
	writeConf(t, e.confPath, machineConf())
	e.sec.Set(secrets.OwnerKeyName(wiretest.ServerName), wiretest.OwnerKey)
	cfg := Config{ConfigPath: e.confPath, CheckDir: func(p string) bool {
		return e.dirs[p] || realDir(p)
	}}
	for _, o := range opts {
		o(e, &cfg)
	}
	e.dirs[wiretest.ClaudeCwd], e.dirs[wiretest.CodexCwd], e.dirs[wiretest.WindowsCwd] = true, true, true
	e.l = e.build(cfg)
	return e
}

func (e *env) build(cfg Config) *Listener {
	l, err := New(cfg, e.dir, Deps{
		Secrets: e.sec, Registrar: e.reg, Matrix: e.room.client, Procs: e.procs, Runner: e.runner, Clock: e.clk,
		PeerPID: func(net.Conn) (int, bool, error) { return int(e.pid.Load()), e.pidOK.Load(), nil },
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(l.stop)
	return l
}

func writeConf(t *testing.T, path string, c MachineConfig) {
	b, _ := json.Marshal(c)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// connect gets the standing token and the owner's settings, as the supervisor does.
func (e *env) connect() {
	e.t.Helper()
	if err := e.l.connect(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// Process trees. A Claude harness at pid p has its shell at p-1 and runs hooks and its MCP server
// as direct children. A Codex harness runs each hook under a fresh shell.
func (e *env) claudeTree(p int) (harness proc.ID) {
	harness = proc.ID{PID: p, Start: uint64(p)}
	e.procs.Line(harness, proc.ID{PID: p - 1, Start: uint64(p - 1)})
	return harness
}

func (e *env) child(parent proc.ID, pid int) proc.ID {
	id := proc.ID{PID: pid, Start: uint64(pid)}
	e.procs.Add(id, parent)
	return id
}

func (e *env) chainOf(pid int) []proc.ID {
	ch, err := proc.Chain(e.procs, pid, chainDepth)
	if err != nil {
		e.t.Fatalf("chain of %d: %v", pid, err)
	}
	return ch
}

func hookReq(h wire.Harness, event, sid, cwd string) wire.HookRequest {
	return wire.HookRequest{V: 1, Kwak: wire.Version, Op: wire.OpHook, Harness: h, Event: event, SessionID: sid, Cwd: cwd}
}

// hook runs a hook from process pid.
func (e *env) hook(pid int, req wire.HookRequest) wire.HookResponse {
	e.t.Helper()
	return e.l.hookOp(req, e.chainOf(pid), false)
}

func (e *env) start(pid int, h wire.Harness, sid, source, cwd string) wire.HookResponse {
	r := hookReq(h, wire.EventSessionStart, sid, cwd)
	r.Source = source
	return e.hook(pid, r)
}

// waitAgent waits until the session's agent is minted.
func (e *env) waitAgent(key string) string {
	e.t.Helper()
	for i := 0; i < 2000; i++ {
		e.l.mu.Lock()
		s := e.l.st.Sessions[key]
		uid := ""
		if s != nil && e.l.tokens[s.Agent] != "" && !s.AgentClosed {
			uid = s.Agent
		}
		inFlight := len(e.l.flights)
		e.l.mu.Unlock()
		if uid != "" && inFlight == 0 {
			return uid
		}
		time.Sleep(time.Millisecond)
	}
	e.t.Fatalf("no agent for %s", key)
	return ""
}

func (e *env) session(key string) *session {
	e.l.mu.Lock()
	defer e.l.mu.Unlock()
	return e.l.st.Sessions[key]
}

// mcp opens an MCP connection from process pid, as serveConn does after mcp_hello.
func (e *env) mcp(pid int, h wire.Harness, envSession string) *client {
	e.t.Helper()
	e.pid.Store(int64(pid))
	c := &client{pid: pid, pidOK: true}
	e.l.hello(c, wire.MCPHello{V: 1, Kwak: wire.Version, Op: wire.OpMCPHello, Harness: h, EnvSession: envSession})
	return c
}

func (e *env) tool(c *client, name, args string, meta ...string) wire.ToolResponse {
	e.t.Helper()
	r := wire.ToolRequest{V: 1, Op: wire.OpTool, ID: 1, Name: name, Args: json.RawMessage(args)}
	if len(meta) > 0 {
		r.MetaSession = meta[0]
	}
	if len(meta) > 1 {
		r.MetaThread = meta[1]
	}
	return e.l.toolOp(c, r)
}

// event builds a room message event.
func event(id, sender string, ts time.Time, content map[string]any) matrix.Event {
	b, _ := json.Marshal(content)
	return matrix.Event{EventID: id, RoomID: wiretest.RoomID, Sender: sender, Type: "m.room.message", TS: ts.UnixMilli(), Content: b}
}

func text(body string, ment ...string) map[string]any {
	m := map[string]any{"msgtype": "m.text", "body": body, "m.mentions": map[string]any{}}
	if len(ment) > 0 {
		m["m.mentions"] = map[string]any{"user_ids": ment}
	}
	return m
}

func inThread(c map[string]any, root string) map[string]any {
	c["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": root, "is_falling_back": true}
	return c
}

// deliver routes events as the sync loop does.
func (e *env) deliver(evs ...matrix.Event) {
	e.l.routeEvents(context.Background(), e.room.client(wiretest.StandingToken), evs)
}

func (e *env) items(key string) []render.Item {
	e.l.mu.Lock()
	defer e.l.mu.Unlock()
	return append([]render.Item(nil), e.l.st.Sessions[key].Inbox.Items...)
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 3000; i++ {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func newEnvLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
