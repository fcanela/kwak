package registrar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

const (
	testServer = wiretest.ServerName
	testRoom   = wiretest.RoomID
	testAlias  = "#agents:" + testServer
	testAdmin  = "@admin:" + testServer
	testHS     = "hs-token-for-tests"
	testIP     = "192.0.2.1"
)

func mx(lp string) string { return "@" + lp + ":" + testServer }

// fakeHS is the Homeserver double: users, devices, tokens, rooms and the bot's notices.
type fakeHS struct {
	mu      sync.Mutex
	users   map[string]*fakeUser
	rooms   map[string]*fakeRoom
	aliases map[string]string
	events  map[string]matrix.Event
	notices []fakeNotice
	calls   []string
	dmBody  []map[string]any
	fail    map[string]error            // method -> error, every call
	failFor map[string]map[string]error // method -> user -> error
	before  map[string]func(user string)
	next    int
}

type fakeUser struct {
	tokens  map[string][]string // device -> live tokens
	display string
	avatar  string
}

type fakeRoom struct {
	members   map[string]bool
	encrypted bool
}

type fakeNotice struct{ room, text string }

func newFakeHS() *fakeHS {
	f := &fakeHS{
		users: map[string]*fakeUser{}, rooms: map[string]*fakeRoom{}, aliases: map[string]string{},
		events: map[string]matrix.Event{}, fail: map[string]error{}, failFor: map[string]map[string]error{},
		before: map[string]func(string){},
	}
	f.rooms[testRoom] = &fakeRoom{members: map[string]bool{}}
	f.aliases[testAlias] = testRoom
	return f
}

func mErr(status int, code string) error { return &matrix.Error{Status: status, Code: code} }

func (f *fakeHS) enter(method, user string) error {
	f.mu.Lock()
	f.calls = append(f.calls, method+" "+user)
	hook := f.before[method]
	err := f.fail[method]
	if m := f.failFor[method]; m != nil && m[user] != nil {
		err = m[user]
	}
	f.mu.Unlock()
	if hook != nil {
		hook(user)
	}
	return err
}

func (f *fakeHS) token() string {
	f.next++
	return fmt.Sprintf("tok-%04d", f.next)
}

func (f *fakeHS) addHuman(uid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[uid] = &fakeUser{tokens: map[string][]string{}, display: uid}
}

func (f *fakeHS) Register(ctx context.Context, lp, dev string) (matrix.Login, error) {
	uid := mx(lp)
	if err := f.enter("Register", uid); err != nil {
		return matrix.Login{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.users[uid] != nil {
		return matrix.Login{}, mErr(400, matrix.ErrUserInUse)
	}
	t := f.token()
	f.users[uid] = &fakeUser{tokens: map[string][]string{dev: {t}}, display: lp + " 💕"}
	return matrix.Login{UserID: uid, AccessToken: t, DeviceID: dev}, nil
}

func (f *fakeHS) Login(ctx context.Context, uid, dev string) (matrix.Login, error) {
	if err := f.enter("Login", uid); err != nil {
		return matrix.Login{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.users[uid]
	if u == nil {
		return matrix.Login{}, mErr(403, matrix.ErrForbidden)
	}
	t := f.token()
	u.tokens[dev] = append(u.tokens[dev], t)
	return matrix.Login{UserID: uid, AccessToken: t, DeviceID: dev}, nil
}

func (f *fakeHS) LogoutDevice(ctx context.Context, uid, dev string) error {
	if err := f.enter("LogoutDevice", uid+"/"+dev); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.users[uid]; u != nil {
		delete(u.tokens, dev)
	}
	return nil
}

func (f *fakeHS) LogoutAll(ctx context.Context, uid string) error {
	if err := f.enter("LogoutAll", uid); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.users[uid]; u != nil {
		clear(u.tokens)
	}
	return nil
}

func (f *fakeHS) who(uid string) string {
	if uid == "" {
		return mx("registrar")
	}
	return uid
}

func (f *fakeHS) JoinAs(ctx context.Context, uid, room string) error {
	if err := f.enter("JoinAs", f.who(uid)); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rooms[room]
	if r == nil {
		return mErr(404, matrix.ErrNotFound)
	}
	r.members[f.who(uid)] = true
	return nil
}

func (f *fakeHS) LeaveAs(ctx context.Context, uid, room string) error {
	if err := f.enter("LeaveAs", f.who(uid)); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rooms[room]
	if r == nil || !r.members[f.who(uid)] {
		return mErr(403, matrix.ErrForbidden)
	}
	delete(r.members, f.who(uid))
	return nil
}

func (f *fakeHS) SetDisplayName(ctx context.Context, uid, name string) error {
	if err := f.enter("SetDisplayName", uid); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.users[uid]; u != nil {
		u.display = name
	}
	return nil
}

func (f *fakeHS) SetAvatarURL(ctx context.Context, uid, url string) error {
	if err := f.enter("SetAvatarURL", uid); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.users[uid]; u != nil {
		u.avatar = url
	}
	return nil
}

func (f *fakeHS) UserExists(ctx context.Context, uid string) (bool, error) {
	if err := f.enter("UserExists", uid); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[uid] != nil, nil
}

func (f *fakeHS) CreateDM(ctx context.Context, human string) (string, error) {
	if err := f.enter("CreateDM", human); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("!dm%d", f.next)
	f.rooms[id] = &fakeRoom{members: map[string]bool{mx("registrar"): true}}
	f.dmBody = append(f.dmBody, DMBody(human))
	return id, nil
}

func (f *fakeHS) Invite(ctx context.Context, room, uid string) error {
	return f.enter("Invite", uid)
}

func (f *fakeHS) RoomInfo(ctx context.Context, room string) (int, bool, error) {
	if err := f.enter("RoomInfo", room); err != nil {
		return 0, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rooms[room]
	if r == nil {
		return 0, false, mErr(404, matrix.ErrNotFound)
	}
	return len(r.members), r.encrypted, nil
}

func (f *fakeHS) FetchEvent(ctx context.Context, room, id string) (*matrix.Event, error) {
	if err := f.enter("FetchEvent", id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ev, ok := f.events[id]
	if !ok || ev.RoomID != room {
		return nil, mErr(404, matrix.ErrNotFound)
	}
	return &ev, nil
}

func (f *fakeHS) Notice(ctx context.Context, room, txn, text string) error {
	if err := f.enter("Notice", room); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notices = append(f.notices, fakeNotice{room, text})
	return nil
}

func (f *fakeHS) ResolveAlias(ctx context.Context, alias string) (string, error) {
	if err := f.enter("ResolveAlias", alias); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.aliases[alias]; ok {
		return id, nil
	}
	return "", mErr(404, matrix.ErrNotFound)
}

// live returns every live token of uid.
func (f *fakeHS) live(uid string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	if u := f.users[uid]; u != nil {
		for _, ts := range u.tokens {
			out = append(out, ts...)
		}
	}
	slices.Sort(out)
	return out
}

func (f *fakeHS) member(room, uid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rooms[room]
	return r != nil && r.members[uid]
}

func (f *fakeHS) join(room, uid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms[room].members[uid] = true
}

func (f *fakeHS) noticesIn(room string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, n := range f.notices {
		if n.room == room {
			out = append(out, n.text)
		}
	}
	return out
}

func (f *fakeHS) callCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeHS) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeClock is a settable Clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fixedRand is a seeded, goroutine-safe random stream.
type fixedRand struct {
	mu sync.Mutex
	r  *rand.ChaCha8
}

func newFixedRand() *fixedRand {
	return &fixedRand{r: rand.NewChaCha8([32]byte{'k', 'w', 'a', 'k'})}
}

func (r *fixedRand) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.Read(p)
}

// fixture is one registrar over a fakeHS, a fixed clock and a fixed Rand.
type fixture struct {
	t     *testing.T
	s     *Server
	hs    *fakeHS
	clock *fakeClock
	path  string
	api   http.Handler
	as    http.Handler
	evN   int
	ip    string
	lmu   sync.Mutex
	last  []byte
}

var t0 = time.Date(2026, 11, 2, 10, 5, 0, 0, time.UTC)

func testConfig(dir string) Config {
	c := Config{
		ServerName: testServer, Homeserver: "http://tuwunel:8008", Room: testAlias,
		Admins: []string{testAdmin}, State: filepath.Join(dir, "state.json"),
		HSToken: testHS, ClaimWait: Duration(50 * time.Millisecond),
	}
	c.Defaults()
	return c
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig(dir)
	st, err := OpenStore(cfg.State)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, hs: newFakeHS(), clock: &fakeClock{t: t0}, path: cfg.State, ip: testIP}
	f.hs.addHuman(testAdmin)
	f.s = &Server{Cfg: cfg, Store: st, HS: f.hs, Clock: f.clock, Rand: newFixedRand(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	f.api, f.as = f.s.API(), f.s.Appservice()
	if err := f.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.hs.join(f.dm(testAdmin), testAdmin)
	return f
}

// reopen builds a new Server over a fresh OpenStore of the same file, as after a restart.
func (f *fixture) reopen() {
	f.t.Helper()
	st, err := OpenStore(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.s = &Server{Cfg: f.s.Cfg, Store: st, HS: f.hs, Clock: f.clock, Rand: newFixedRand(), Log: f.s.Log}
	f.api, f.as = f.s.API(), f.s.Appservice()
}

func (f *fixture) dm(human string) string {
	var room string
	f.s.Store.View(func(st *State) { room = st.DMs[human] })
	return room
}

func (f *fixture) state() *State {
	var cp *State
	f.s.Store.View(func(st *State) {
		b, _ := json.Marshal(st)
		cp = &State{}
		json.Unmarshal(b, cp)
	})
	return cp
}

// call sends one API request from f.ip.
func (f *fixture) call(method, path, auth, body string) (int, []byte) {
	f.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = f.ip + ":40000"
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	f.api.ServeHTTP(w, r)
	f.lmu.Lock()
	f.last = w.Body.Bytes()
	f.lmu.Unlock()
	return w.Code, w.Body.Bytes()
}

func (f *fixture) callJSON(method, path, key string, body any, out any) int {
	f.t.Helper()
	code, _ := f.callRaw(method, path, key, body, out)
	return code
}

func (f *fixture) callRaw(method, path, key string, body any, out any) (int, []byte) {
	f.t.Helper()
	b := ""
	if body != nil {
		raw, _ := json.Marshal(body)
		b = string(raw)
	}
	auth := ""
	if key != "" {
		auth = "Bearer " + key
	}
	code, resp := f.call(method, wire.APIPrefix+path, auth, b)
	if out != nil && code < 300 {
		if err := json.Unmarshal(resp, out); err != nil {
			f.t.Errorf("%s %s: %v: %s", method, path, err, resp)
		}
	}
	return code, resp
}

// txn delivers events to the appservice with the hs_token.
func (f *fixture) txn(events ...matrix.Event) int {
	f.t.Helper()
	f.evN++
	b, _ := json.Marshal(map[string]any{"events": events})
	r := httptest.NewRequest("PUT", fmt.Sprintf("/_matrix/app/v1/transactions/t%d", f.evN), bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+testHS)
	w := httptest.NewRecorder()
	f.as.ServeHTTP(w, r)
	return w.Code
}

// event makes an event that tuwunel also holds, so FetchEvent agrees.
func (f *fixture) event(sender, room, typ string, stateKey *string, content any) matrix.Event {
	f.evN++
	c, _ := json.Marshal(content)
	ev := matrix.Event{EventID: fmt.Sprintf("$ev%d", f.evN), RoomID: room, Sender: sender, Type: typ,
		StateKey: stateKey, TS: f.clock.Now().UnixMilli(), Content: c}
	f.hs.mu.Lock()
	f.hs.events[ev.EventID] = ev
	f.hs.mu.Unlock()
	return ev
}

// say sends a bot command as sender in room and returns the bot's new notices there.
func (f *fixture) say(sender, room, body string) []string {
	f.t.Helper()
	before := len(f.hs.noticesIn(room))
	f.txn(f.event(sender, room, "m.room.message", nil, map[string]string{"msgtype": "m.text", "body": body}))
	return f.hs.noticesIn(room)[before:]
}

// addOwner runs `owner add` for @name and joins the human to the new DM.
func (f *fixture) addOwner(name string) string {
	f.t.Helper()
	human := mx(name)
	f.hs.addHuman(human)
	out := f.say(testAdmin, f.dm(testAdmin), "owner add "+human)
	if len(out) != 1 || !strings.Contains(out[0], "Added") {
		f.t.Fatalf("owner add: %q", out)
	}
	dm := f.dm(human)
	if dm == "" {
		f.t.Fatal("no DM recorded for", human)
	}
	f.hs.join(dm, human)
	return dm
}

type paired struct {
	key, id, code string
}

// pair runs POST /pair, `pair <code>` in the owner's DM and the claim. confirm also calls /standing.
func (f *fixture) pair(owner, oldKey string, confirm bool) paired {
	f.t.Helper()
	var pr wire.PairResponse
	if c := f.callJSON("POST", wire.PathPair, oldKey, wire.PairRequest{Machine: "laptop", OS: "linux", Version: wire.Version}, &pr); c != 201 {
		f.t.Fatalf("pair: %d", c)
	}
	out := f.say(mx(owner), f.dm(mx(owner)), "pair "+pr.Code)
	if len(out) != 1 || !strings.HasPrefix(out[0], "Paired machine") {
		f.t.Fatalf("pair reply: %q", out)
	}
	var cr wire.ClaimResponse
	if c := f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, &cr); c != 200 {
		f.t.Fatalf("claim: %d", c)
	}
	if confirm {
		var sr wire.Standing
		if c := f.callJSON("POST", wire.PathStanding, cr.OwnerKey, struct{}{}, &sr); c != 200 {
			f.t.Fatalf("standing: %d", c)
		}
	}
	return paired{key: cr.OwnerKey, id: cr.PairingID, code: pr.Code}
}

func (f *fixture) agent(key string, h wire.Harness, session, helper string) (int, wire.Agent, wire.APIError) {
	f.t.Helper()
	rid, _ := f.s.randomHex(16)
	return f.agentReq(key, wire.AgentRequest{RequestID: rid, Harness: h, Session: session, Helper: helper})
}

func (f *fixture) agentReq(key string, req wire.AgentRequest) (int, wire.Agent, wire.APIError) {
	f.t.Helper()
	var a wire.Agent
	var e wire.APIError
	code, body := f.callRaw("POST", wire.PathAgents, key, req, &a)
	if code != 200 {
		json.Unmarshal(body, &e)
	}
	return code, a, e
}
