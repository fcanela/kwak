package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/secrets/secretstest"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

// Keyring items (decision 40): the old shared name an older kwak wrote, and the item of the
// pairing the fake registrar hands out.
var (
	legacyKeyName = secrets.OwnerKeyName(wiretest.ServerName)
	keyName       = secrets.PairingKeyName(wiretest.ServerName, wiretest.PairingID)
)

// fakeRegistrar answers /_kwak/v1 like the registrar, from a script.
type fakeRegistrar struct {
	mu       sync.Mutex
	calls    []string // "path auth"
	pairs    []int    // status per /pair call; 201 when the list runs out
	pending  int      // claims answered 202 before the 200
	owner    string   // owner in the claim
	claimErr int      // 410 instead of a claim
	standing int      // status of /standing
	unpair   int      // status of /unpair
	me       int      // status of /me
	test     bool     // the claim is for a test pairing (decision 40)
	pairing  string   // pairing id in the claim; wiretest.PairingID when empty
	// replaced is set when a /pair carried OldOwnerKey and the claim is a normal pairing, and
	// confirmed once /standing took the new key: then OldOwnerKey is revoked, as the registrar
	// revokes a replaced pairing (a pairing of the other kind is a new one, decision 41, so a test claim
	// replaces nothing).
	replaced, confirmed bool
}

func (f *fakeRegistrar) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, wire.APIPrefix)
	f.calls = append(f.calls, path+" "+r.Header.Get("Authorization"))
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	write := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	apiErr := func(status int, code string) {
		write(status, wire.APIError{Errcode: code, Message: "scripted"})
	}
	switch path {
	case wire.PathPair:
		status := 201
		if len(f.pairs) > 0 {
			status, f.pairs = f.pairs[0], f.pairs[1:]
		}
		if status == 429 {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			return
		}
		f.replaced = bearer == wiretest.OldOwnerKey
		write(status, wire.PairResponse{Code: wiretest.PairCode, Claim: wiretest.Claim, Bot: wiretest.Bot,
			ExpiresAt: at("2026-11-02T10:15:00Z"), Replaces: bearer == wiretest.OldOwnerKey})
	case wire.PathPairClaim:
		if bearer != wiretest.Claim {
			apiErr(410, wire.ErrExpired)
			return
		}
		if f.claimErr != 0 {
			apiErr(f.claimErr, wire.ErrExpired)
			return
		}
		if f.pending > 0 {
			f.pending--
			write(202, wire.ClaimResponse{Status: wire.ClaimPending})
			return
		}
		owner := f.owner
		if owner == "" {
			owner = wiretest.Owner
		}
		pairing := f.pairing
		if pairing == "" {
			pairing = wiretest.PairingID
		}
		write(200, wire.ClaimResponse{Status: wire.ClaimPaired, Owner: owner, PairingID: pairing,
			OwnerKey: wiretest.OwnerKey, KeyExpiresAt: at("2027-01-31T10:05:00Z"), RoomID: wiretest.RoomID,
			StandingUser: wiretest.StandingUser, ServerName: wiretest.ServerName, Test: f.test})
	case wire.PathStanding:
		if f.standing != 0 && f.standing != 200 {
			apiErr(f.standing, wire.ErrHomeserver)
			return
		}
		f.confirmed = f.confirmed || bearer == wiretest.OwnerKey
		write(200, wire.Standing{UserID: wiretest.StandingUser, AccessToken: wiretest.StandingToken, DeviceID: "KWAK-" + wiretest.PairingID})
	case wire.PathMe:
		if f.me == 401 || bearer == wiretest.OldOwnerKey && f.replaced && f.confirmed && !f.test {
			apiErr(401, wire.ErrBadKey)
			return
		}
		write(200, wire.Me{Owner: wiretest.Owner, PairingID: wiretest.OldPairingID})
	case wire.PathUnpair:
		if f.unpair != 0 && f.unpair != 200 {
			code := wire.ErrHomeserver
			if f.unpair == 401 {
				code = wire.ErrBadKey
			}
			apiErr(f.unpair, code)
			return
		}
		write(200, wire.Empty{})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRegistrar) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	slept  []time.Duration
	failAt int // Sleep returns an error on this call number (1-based); 0 never
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	if c.failAt != 0 && len(c.slept) >= c.failAt {
		return context.Canceled
	}
	return ctx.Err()
}

type fakeService struct {
	installed []string
	err       error
}

func (s *fakeService) Install(exe string) error { s.installed = append(s.installed, exe); return s.err }
func (s *fakeService) Uninstall() error         { return nil }
func (s *fakeService) Start() error             { return nil }
func (s *fakeService) Stop() error              { return nil }
func (s *fakeService) Status() (string, error)  { return "", nil }

// fakeListener answers IPC ops from a script. A missing op means the listener is down.
type fakeListener struct {
	mu      sync.Mutex
	ops     []string
	replies map[string][]string // op -> reply lines, the last one repeats
	onOp    map[string]func()   // run when the op arrives, before the reply
}

func (l *fakeListener) call(_ context.Context, req, reply any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := req.(wire.Head)
	if h.V != wire.ProtocolVersion {
		return errors.New("bad v")
	}
	l.ops = append(l.ops, h.Op)
	if f := l.onOp[h.Op]; f != nil {
		f()
	}
	rs := l.replies[h.Op]
	if len(rs) == 0 {
		return errors.New("dial unix: connection refused")
	}
	line := rs[0]
	if len(rs) > 1 {
		l.replies[h.Op] = rs[1:]
	}
	if line == "timeout" {
		return context.DeadlineExceeded
	}
	return wire.Decode([]byte(line), reply)
}

type rig struct {
	t        *testing.T
	reg      *fakeRegistrar
	srv      *httptest.Server
	keys     *secretstest.Memory
	svc      *fakeService
	ln       *fakeListener
	clock    *fakeClock
	cfgDir   string
	stateDir string
	harness  []wire.Harness
	terminal bool
	bus      error
	cli      *CLI
}

func newRig(t *testing.T) *rig { return newRigOn(t, secretstest.Mem(), false) }

// newRigOn is a rig over keys, which another rig may share as two pairings on one machine share
// the OS keyring. dev builds the CLI of a dev build (make dev).
func newRigOn(t *testing.T, keys *secretstest.Memory, dev bool) *rig {
	r := &rig{
		t:        t,
		reg:      &fakeRegistrar{},
		keys:     keys,
		svc:      &fakeService{},
		ln:       &fakeListener{replies: map[string][]string{}},
		clock:    &fakeClock{now: at("2026-11-02T10:05:00Z")},
		cfgDir:   filepath.Join(t.TempDir(), "config"),
		stateDir: filepath.Join(t.TempDir(), "state"),
		terminal: true,
	}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(r.reg.handler))
	t.Cleanup(r.srv.Close)
	client := r.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r.cli = New(Env{
		Secrets:   r.keys,
		Service:   r.svc,
		Preflight: func() error { return r.bus },
		Call:      r.ln.call,
		PrintHarness: func(w io.Writer, h wire.Harness) error {
			r.harness = append(r.harness, h)
			_, err := io.WriteString(w, "steps for "+string(h)+"\n")
			return err
		},
		IsTerminal: func() bool { return r.terminal },
		HTTP:       client,
		ConfigDir:  func() (string, error) { return r.cfgDir, nil },
		StateDir:   func() (string, error) { return r.stateDir, nil },
		Hostname:   func() (string, error) { return "laptop\x1b[31m\n", nil },
		Executable: func() (string, error) { return "/home/u/go/bin/kwak", nil },
		Now:        r.clock.Now,
		Sleep:      r.clock.Sleep,
		Dev:        dev,
	})
	return r
}

func (r *rig) run(m Main, stdin string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := m(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func (r *rig) pair(args ...string) (int, string, string) {
	return r.run(r.cli.Pair, "", append([]string{"--server", r.srv.URL}, args...)...)
}

// paired writes the state of a machine paired by an older kwak with OldOwnerKey as pairing
// OldPairingID: no keyring_user in config.json, and the key under the old shared name.
func (r *rig) paired() {
	r.t.Helper()
	data := `{"version":1,"server":"` + r.srv.URL + `","server_name":"kwak.test","owner":"fcanela","machine":"laptop",` +
		`"pairing_id":"` + wiretest.OldPairingID + `","room_id":"!a1B2c3D4","standing_user":"` + wiretest.StandingUser + `",` +
		`"resume_max":3,"resume_exe":{"claude":"/opt/claude"},"resume":["replies"],"thread_ceiling":7,"resume_per_hour":40,"check_outgoing":"/opt/out.sh","check_incoming":"/opt/in.sh",` +
		`"future_field":{"x":1}}`
	if err := os.MkdirAll(r.cfgDir, 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.cfgDir, ConfigFile), []byte(data), 0o600); err != nil {
		r.t.Fatal(err)
	}
	if err := r.keys.Set(legacyKeyName, wiretest.OldOwnerKey); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) config() map[string]any {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.cfgDir, ConfigFile))
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		r.t.Fatal(err)
	}
	return m
}

// key is the owner key the machine uses now: the one in the item config.json names, or, with no
// config.json, any owner key item left behind ("" when there is none).
func (r *rig) key() string {
	name := ""
	if cfg, err := loadConfig(r.cfgDir, true); err == nil {
		name = cfg.KeyName()
	} else if items := r.ownerItems(); len(items) > 0 {
		name = items[0]
	}
	v, err := r.keys.Get(name)
	if err != nil {
		return ""
	}
	return v
}

// ownerItems lists the owner key items in the keyring, sorted.
func (r *rig) ownerItems() []string {
	var out []string
	for _, k := range r.keys.Items() {
		if strings.HasSuffix(k, "/owner-key") {
			out = append(out, k)
		}
	}
	return out
}

func (r *rig) noProbe() {
	r.t.Helper()
	if _, err := r.keys.Get(probeKey); !errors.Is(err, secrets.ErrNotFound) {
		r.t.Error("the keyring probe item was left behind")
	}
}

func TestPairFirst(t *testing.T) {
	r := newRig(t)
	r.reg.pending = 2
	r.ln.replies[wire.OpReload] = []string{`{"v":1,"ok":true}`}
	code, out, errs := r.pair("--owner", "fcanela")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{
		"In Element, send this to @registrar:kwak.test, in your chat with it:\n\n    pair 7H3Q-9XRM-K2PD\n\n" +
			"The code works once, for 10 minutes. kwak never asks you to paste a secret. Waiting...\n",
		"Paired as fcanela on laptop (pairing 3f9a1c2e, key valid until 2027-01-31).\nNext: kwak install.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, wiretest.OwnerKey) || strings.Contains(out, wiretest.Claim) {
		t.Error("a secret was printed")
	}
	if r.key() != wiretest.OwnerKey {
		t.Errorf("keyring holds %q", r.key())
	}
	if items := r.ownerItems(); len(items) != 1 || items[0] != keyName {
		t.Errorf("keyring items %v, want only %s", items, keyName)
	}
	r.noProbe()
	cfg := r.config()
	want := map[string]any{"version": 1.0, "server": r.srv.URL, "server_name": "kwak.test", "owner": "fcanela",
		"machine": "laptop", "pairing_id": "3f9a1c2e", "room_id": "!a1B2c3D4", "standing_user": wiretest.StandingUser,
		"resume_max": 10.0, "keyring_user": "kwak.test/3f9a1c2e/owner-key"}
	for k, v := range want {
		if cfg[k] != v {
			t.Errorf("config %s = %v, want %v", k, cfg[k], v)
		}
	}
	if len(cfg) != len(want) {
		t.Errorf("config has extra keys: %v", cfg)
	}
	fi, err := os.Stat(filepath.Join(r.cfgDir, ConfigFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v %v", fi.Mode(), err)
	}
	calls := r.reg.log()
	wantCalls := []string{"/pair ", "/pair/claim Bearer " + wiretest.Claim, "/pair/claim Bearer " + wiretest.Claim,
		"/pair/claim Bearer " + wiretest.Claim, "/standing Bearer " + wiretest.OwnerKey}
	if strings.Join(calls, "|") != strings.Join(wantCalls, "|") {
		t.Errorf("calls %q\nwant %q", calls, wantCalls)
	}
	if strings.Join(r.ln.ops, ",") != "reload" {
		t.Errorf("listener ops %v", r.ln.ops)
	}
	for _, d := range r.clock.slept {
		if d != claimRetry {
			t.Errorf("slept %v", d)
		}
	}
}

func TestPairReload(t *testing.T) {
	r := newRig(t)
	r.ln.replies[wire.OpReload] = []string{"timeout"}
	if _, out, _ := r.pair("--owner", "fcanela"); !strings.Contains(out, "The listener is still loading the new key") {
		t.Errorf("timeout not reported:\n%s", out)
	}
	r = newRig(t)
	r.ln.replies[wire.OpReload] = []string{`{"v":1,"ok":false,"err":"internal","detail":"registrar unreachable, HTTP 502"}`}
	if _, out, _ := r.pair("--owner", "fcanela"); !strings.Contains(out, "did not take the new key yet (internal registrar unreachable, HTTP 502)") {
		t.Errorf("refusal not reported:\n%s", out)
	}
	r = newRig(t)
	if code, out, _ := r.pair("--owner", "fcanela"); code != 0 || strings.Contains(out, "listener") {
		t.Errorf("listener down: exit %d:\n%s", code, out)
	}
}

func TestPairNeedsTerminal(t *testing.T) {
	r := newRig(t)
	r.terminal = false
	code, _, errs := r.pair("--owner", "fcanela")
	if code != 1 || !strings.Contains(errs, "needs a terminal") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if len(r.reg.log()) != 0 || r.keys.Sets() != 0 {
		t.Error("pair did something without a terminal")
	}
}

func TestPairNoBus(t *testing.T) {
	r := newRig(t)
	r.bus = errors.New("DBUS_SESSION_BUS_ADDRESS is not set")
	code, _, errs := r.pair("--owner", "fcanela")
	if code != 1 || !strings.Contains(errs, "no session bus: run kwak pair from your desktop session (DBUS_SESSION_BUS_ADDRESS is not set)") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if len(r.reg.log()) != 0 {
		t.Error("registrar called")
	}
}

func TestPairKeyringBroken(t *testing.T) {
	r := newRig(t)
	r.keys.Fail(secrets.ErrTimeout)
	code, _, errs := r.pair("--owner", "fcanela")
	if code != 1 || !strings.Contains(errs, "the keyring does not work") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if len(r.reg.log()) != 0 {
		t.Error("registrar called")
	}
}

func TestPairOwnerPrompt(t *testing.T) {
	r := newRig(t)
	code, out, errs := r.run(r.cli.Pair, "fcanela\n", "--server", r.srv.URL)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.HasPrefix(out, "Your kwak owner name (your Matrix user name, for example fcanela): ") {
		t.Errorf("no prompt: %s", out)
	}
	if r.config()["owner"] != "fcanela" {
		t.Error("owner not from the prompt")
	}
}

func TestPairInputRefused(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bad owner", []string{"--owner", "Fc.anela", "--server", "https://kwak.test"}, "is not an owner name"},
		{"no server", []string{"--owner", "fcanela"}, "pass --server https://<your kwak server>, or set KWAK_SERVER"},
		{"http server", []string{"--owner", "fcanela", "--server", "http://kwak.test"}, "must start with https://"},
	}
	for _, c := range cases {
		r := newRig(t)
		code, _, errs := r.run(r.cli.Pair, "", c.args...)
		if code != 1 || !strings.Contains(errs, c.want) {
			t.Errorf("%s: exit %d: %s", c.name, code, errs)
		}
		if len(r.reg.log()) != 0 {
			t.Errorf("%s: registrar called", c.name)
		}
	}
	r := newRig(t)
	if code, _, _ := r.run(r.cli.Pair, "", "extra"); code != 2 {
		t.Errorf("positional argument: exit %d", code)
	}
}

func TestPairDefaultServer(t *testing.T) {
	r := newRig(t)
	r.cli.env.Server = r.srv.URL
	if code, _, errs := r.run(r.cli.Pair, "", "--owner", "fcanela"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

// The defaults from KWAK_SERVER and KWAK_OWNER: a first pairing then needs no flag and no answer.
func TestPairDefaults(t *testing.T) {
	r := newRig(t)
	r.cli.env.Server = r.srv.URL + "/"
	r.cli.env.Owner = "fcanela"
	code, out, errs := r.run(r.cli.Pair, "")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if strings.Contains(out, "owner name") {
		t.Errorf("prompted although Env.Owner is set: %s", out)
	}
	if cfg := r.config(); cfg["owner"] != "fcanela" || cfg["server"] != r.srv.URL {
		t.Errorf("config %v", cfg)
	}
}

func TestPairFlagsBeatDefaults(t *testing.T) {
	r := newRig(t)
	r.cli.env.Server = "https://wrong.invalid"
	r.cli.env.Owner = "bob"
	if code, _, errs := r.pair("--owner", "fcanela"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if cfg := r.config(); cfg["owner"] != "fcanela" || cfg["server"] != r.srv.URL {
		t.Errorf("config %v", cfg)
	}
}

// A paired machine keeps the server and owner in its config.json. Defaults that disagree are not
// an error, unlike flags that disagree: they are only meant for a first pairing.
func TestRepairIgnoresDefaults(t *testing.T) {
	r := newRig(t)
	r.paired()
	r.cli.env.Server = "https://other.invalid"
	r.cli.env.Owner = "bob"
	if code, _, errs := r.run(r.cli.Pair, ""); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if cfg := r.config(); cfg["owner"] != "fcanela" || cfg["server"] != r.srv.URL {
		t.Errorf("config %v", cfg)
	}
}

func TestPairBadOwnerDefault(t *testing.T) {
	r := newRig(t)
	r.cli.env.Owner = "Fc.anela"
	code, _, errs := r.pair()
	if code != 1 || !strings.Contains(errs, `"Fc.anela" is not an owner name`) {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if len(r.reg.log()) != 0 {
		t.Error("registrar called")
	}
}

func TestRepair(t *testing.T) {
	r := newRig(t)
	r.paired()
	code, out, errs := r.run(r.cli.Pair, "")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if strings.Contains(out, "did not accept") {
		t.Error("valid old key reported as refused")
	}
	calls := r.reg.log()
	if calls[0] != "/pair Bearer "+wiretest.OldOwnerKey {
		t.Errorf("first call %q", calls[0])
	}
	if r.key() != wiretest.OwnerKey {
		t.Errorf("key %q", r.key())
	}
	// The machine moves from the old shared item to its pairing's own item; the old one goes.
	if items := r.ownerItems(); len(items) != 1 || items[0] != keyName {
		t.Errorf("keyring items %v, want only %s", items, keyName)
	}
	cfg := r.config()
	if cfg["keyring_user"] != keyName {
		t.Errorf("keyring_user %v", cfg["keyring_user"])
	}
	if cfg["pairing_id"] != wiretest.PairingID || cfg["resume_max"] != 3.0 || cfg["thread_ceiling"] != 7.0 || cfg["resume_per_hour"] != 40.0 || cfg["future_field"] == nil ||
		cfg["check_outgoing"] != "/opt/out.sh" || cfg["check_incoming"] != "/opt/in.sh" {
		t.Errorf("config %v", cfg)
	}
	if exe, _ := cfg["resume_exe"].(map[string]any); exe["claude"] != "/opt/claude" {
		t.Errorf("resume_exe lost: %v", cfg["resume_exe"])
	}
	if on, _ := cfg["resume"].([]any); len(on) != 1 || on[0] != "replies" {
		t.Errorf("resume lost: %v", cfg["resume"])
	}
}

func TestRepairOwnerFlagMismatch(t *testing.T) {
	r := newRig(t)
	r.paired()
	code, _, errs := r.run(r.cli.Pair, "", "--owner", "bob")
	if code != 1 || !strings.Contains(errs, "paired as fcanela") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestPairClaimByAnotherOwner(t *testing.T) {
	for _, repair := range []bool{false, true} {
		r := newRig(t)
		if repair {
			r.paired()
		}
		before := r.config()
		r.reg.owner = "bob"
		code, _, errs := r.run(r.cli.Pair, "", "--owner", "fcanela", "--server", r.srv.URL)
		if code != 1 || !strings.Contains(errs, "this code was sent by owner bob, not by you (fcanela); pairing discarded. "+
			"If you did not share the code, nobody can use it now.") {
			t.Fatalf("repair %v: exit %d: %s", repair, code, errs)
		}
		calls := r.reg.log()
		if last := calls[len(calls)-1]; last != "/unpair Bearer "+wiretest.OwnerKey {
			t.Errorf("repair %v: last call %q, want /unpair with the new key", repair, last)
		}
		wantKey := ""
		if repair {
			wantKey = wiretest.OldOwnerKey
		}
		if r.key() != wantKey {
			t.Errorf("repair %v: keyring holds %q", repair, r.key())
		}
		if sets := r.keys.Sets(); sets != map[bool]int{false: 1, true: 2}[repair] {
			t.Errorf("repair %v: %d keyring writes, want only the probe", repair, sets)
		}
		if after := r.config(); !jsonSame(before, after) {
			t.Errorf("repair %v: config changed: %v", repair, after)
		}
	}
}

func jsonSame(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestPairConfirmationFails(t *testing.T) {
	r := newRig(t)
	r.paired()
	before := r.config()
	r.reg.standing = 502
	code, _, errs := r.run(r.cli.Pair, "")
	if code != 1 || !strings.Contains(errs, "The old key and config are back.") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if r.key() != wiretest.OldOwnerKey {
		t.Errorf("key %q, want the old key back", r.key())
	}
	if !jsonSame(before, r.config()) {
		t.Errorf("config %v, want the old one", r.config())
	}

	r = newRig(t)
	r.reg.standing = 502
	code, _, errs = r.pair("--owner", "fcanela")
	if code != 1 || !strings.Contains(errs, "The new key and config were removed.") {
		t.Fatalf("first pairing: exit %d: %s", code, errs)
	}
	if r.key() != "" || r.config() != nil {
		t.Error("first pairing left a key or a config")
	}
	r.noProbe()
}

// A lost /standing answer on a re-pair: the old key is already revoked, so the new one stays.
func TestPairConfirmationLostKeepsNewKey(t *testing.T) {
	r := newRig(t)
	r.paired()
	r.reg.standing = 502
	r.reg.me = 401
	code, out, errs := r.run(r.cli.Pair, "")
	if code != 0 || !strings.Contains(out, "Paired as fcanela") {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	if r.key() != wiretest.OwnerKey || r.config()["pairing_id"] != wiretest.PairingID {
		t.Fatalf("key %q config %v, want the new ones", r.key(), r.config())
	}
}

func TestPairRateLimited(t *testing.T) {
	r := newRig(t)
	r.reg.pairs = []int{429}
	code, out, errs := r.pair("--owner", "fcanela")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "kwak pair is rate limited; waiting 30 s") || r.clock.slept[0] != 30*time.Second {
		t.Errorf("no wait: %v\n%s", r.clock.slept, out)
	}

	r = newRig(t)
	r.reg.pairs = []int{429, 429}
	if code, _, _ := r.pair("--owner", "fcanela"); code != 1 {
		t.Errorf("second 429: exit %d", code)
	}
	if n := len(r.reg.log()); n != 2 {
		t.Errorf("%d calls, want 2", n)
	}
}

func TestPairExpired(t *testing.T) {
	r := newRig(t)
	r.reg.claimErr = 410
	code, _, errs := r.pair("--owner", "fcanela")
	if code != 1 || !strings.Contains(errs, "pairing expired; run kwak pair again") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if r.key() != "" || r.config() != nil {
		t.Error("expired pairing stored something")
	}

	r = newRig(t)
	r.reg.pending = 1 << 30
	code, _, errs = r.pair("--owner", "fcanela")
	if code != 1 || !strings.Contains(errs, "pairing expired") {
		t.Fatalf("never sent: exit %d: %s", code, errs)
	}
}

func TestSetup(t *testing.T) {
	r := newRig(t)
	r.ln.replies[wire.OpStatus] = []string{
		`{"v":1,"ok":true,"kwak":"0.1.0","state":"connecting","caller_checks":"peer","resume_runs":0,"resume_active":0,"sessions":[]}`,
		`{"v":1,"ok":true,"kwak":"0.1.0","state":"syncing","caller_checks":"peer","resume_runs":0,"resume_active":0,"sessions":[]}`,
	}
	code, out, errs := r.run(r.cli.Setup, "", "--owner", "fcanela", "--server", r.srv.URL)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if r.key() != wiretest.OwnerKey {
		t.Error("not paired")
	}
	if strings.Join(r.svc.installed, ",") != "/home/u/go/bin/kwak" {
		t.Errorf("installed %v", r.svc.installed)
	}
	if len(r.harness) != 2 || r.harness[0] != wire.HarnessClaude || r.harness[1] != wire.HarnessCodex {
		t.Errorf("harness steps %v", r.harness)
	}
	if !strings.Contains(out, "The listener is connected.") || !strings.HasSuffix(out, "steps for claude\n\nsteps for codex\n") {
		t.Errorf("output:\n%s", out)
	}
	if strings.Contains(out, "Next: kwak install") {
		t.Error("setup tells the user to run install")
	}
}

func TestSetupNoSync(t *testing.T) {
	r := newRig(t)
	code, out, errs := r.run(r.cli.Setup, "", "--owner", "fcanela", "--server", r.srv.URL)
	if code != 1 || !strings.Contains(errs, "did not connect within 30 s") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "steps for codex") {
		t.Error("harness steps not printed")
	}
	var waited time.Duration
	for _, d := range r.clock.slept {
		if d == setupPoll {
			waited += d
		}
	}
	if waited < SetupWait || waited > SetupWait+setupPoll {
		t.Errorf("waited %v", waited)
	}
}

func TestSetupPairFails(t *testing.T) {
	r := newRig(t)
	r.terminal = false
	if code, _, _ := r.run(r.cli.Setup, "", "--owner", "fcanela", "--server", r.srv.URL); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if len(r.svc.installed) != 0 || len(r.harness) != 0 {
		t.Error("setup went on after a failed pairing")
	}
}

func TestSetupInstallFails(t *testing.T) {
	r := newRig(t)
	r.svc.err = errors.New("systemctl failed")
	code, _, errs := r.run(r.cli.Setup, "", "--owner", "fcanela", "--server", r.srv.URL)
	if code != 1 || !strings.Contains(errs, "systemctl failed") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func (r *rig) stateFiles() {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Join(r.stateDir, "resume"), 0o700); err != nil {
		r.t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.stateDir, "state.json"), []byte("{}"), 0o600)
}

func TestUnpair(t *testing.T) {
	r := newRig(t)
	r.paired()
	r.stateFiles()
	code, out, errs := r.run(r.cli.Unpair, "")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if calls := r.reg.log(); len(calls) != 1 || calls[0] != "/unpair Bearer "+wiretest.OldOwnerKey {
		t.Errorf("calls %v", calls)
	}
	if r.key() != "" || r.config() != nil {
		t.Error("key or config left")
	}
	if _, err := os.Stat(r.stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("state dir left")
	}
	if !strings.Contains(out, "Unpaired laptop (pairing 77c0de11).") || strings.Contains(out, "revoke") {
		t.Errorf("output %s", out)
	}
	if strings.Join(r.ln.ops, ",") != wire.OpShutdown {
		t.Errorf("listener ops %v", r.ln.ops)
	}
}

func TestUnpairRegistrarDown(t *testing.T) {
	r := newRig(t)
	r.paired()
	r.stateFiles()
	r.reg.unpair = 502
	code, _, errs := r.run(r.cli.Unpair, "")
	if code != 1 || !strings.Contains(errs, "The key is kept") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if r.key() != wiretest.OldOwnerKey || r.config() == nil {
		t.Error("deleted although the registrar was not told")
	}
	if _, err := os.Stat(filepath.Join(r.stateDir, "state.json")); err != nil {
		t.Error("state deleted")
	}

	code, out, _ := r.run(r.cli.Unpair, "", "--force")
	if code != 0 || !strings.Contains(out, "Send this to @registrar:kwak.test: revoke 77c0de11") {
		t.Fatalf("force: exit %d: %s", code, out)
	}
	if r.key() != "" || r.config() != nil {
		t.Error("force left the key or config")
	}
}

func TestUnpairKeyAlreadyRevoked(t *testing.T) {
	r := newRig(t)
	r.paired()
	r.reg.unpair = 401
	code, out, errs := r.run(r.cli.Unpair, "")
	if code != 0 || strings.Contains(out, "revoke") {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	if r.key() != "" {
		t.Error("key left")
	}
}

func TestUnpairKeyMissing(t *testing.T) {
	r := newRig(t)
	r.paired()
	r.keys.Delete(legacyKeyName)
	if code, _, errs := r.run(r.cli.Unpair, ""); code != 1 || !strings.Contains(errs, "--force") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if r.config() == nil {
		t.Error("config deleted")
	}
	code, out, _ := r.run(r.cli.Unpair, "", "--force")
	if code != 0 || r.config() != nil || !strings.Contains(out, "revoke 77c0de11") || len(r.reg.log()) != 0 {
		t.Errorf("force: exit %d: %s", code, out)
	}
}

func TestUnpairNotPaired(t *testing.T) {
	r := newRig(t)
	code, out, _ := r.run(r.cli.Unpair, "")
	if code != 0 || !strings.Contains(out, "not paired") || len(r.reg.log()) != 0 {
		t.Errorf("exit %d: %s", code, out)
	}
}

// runningListener holds listen.lock in the state dir like a running kwak listen. On shutdown it
// writes state.json, as the real one flushes, then frees the lock, unless stuck.
func (r *rig) runningListener(stuck bool) {
	r.t.Helper()
	release, err := ipc.Lock(filepath.Join(r.stateDir, "listen.lock"))
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(release)
	r.ln.replies[wire.OpShutdown] = []string{`{"v":1,"ok":true}`}
	r.ln.onOp = map[string]func(){wire.OpShutdown: func() {
		os.WriteFile(filepath.Join(r.stateDir, "state.json"), []byte(`{"version":1}`), 0o600)
		if !stuck {
			release()
		}
	}}
}

func TestUnpairStopsListener(t *testing.T) {
	r := newRig(t)
	r.paired()
	r.stateFiles()
	r.runningListener(false)
	code, out, errs := r.run(r.cli.Unpair, "")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if strings.Join(r.ln.ops, ",") != wire.OpShutdown {
		t.Errorf("listener ops %v", r.ln.ops)
	}
	if _, err := os.Stat(r.stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("state dir left after the listener flushed")
	}
	if !strings.Contains(out, "The kwak listener is not running. Next: kwak uninstall") {
		t.Errorf("output %s", out)
	}
}

func TestUnpairListenerStuck(t *testing.T) {
	r := newRig(t)
	r.paired()
	r.stateFiles()
	r.runningListener(true)
	r.reg.unpair = 502
	code, out, errs := r.run(r.cli.Unpair, "", "--force")
	if code != 1 || !strings.Contains(errs, "did not stop, so the state dir") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "Send this to @registrar:kwak.test: revoke 77c0de11") {
		t.Errorf("no revoke line: %s", out)
	}
	if _, err := os.Stat(filepath.Join(r.stateDir, "state.json")); err != nil {
		t.Error("state deleted under a running listener")
	}
	if r.key() != "" || r.config() != nil {
		t.Error("key or config left")
	}
	if n := len(r.clock.slept); n != stopTries-1 {
		t.Errorf("waited %d times", n)
	}
}

func TestUnpairSharedDir(t *testing.T) {
	r := newRig(t)
	r.stateDir = r.cfgDir
	r.paired()
	r.stateFiles()
	r.runningListener(false)
	code, _, errs := r.run(r.cli.Unpair, "")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if _, err := os.Stat(r.cfgDir); !errors.Is(err, os.ErrNotExist) || r.key() != "" {
		t.Error("files left")
	}
}

func TestUnpairForceUnreadableConfig(t *testing.T) {
	r := newRig(t)
	r.paired()
	newer := `{"version":99,"server":"https://kwak.test","server_name":"kwak.test","pairing_id":"` + wiretest.OldPairingID + `"}`
	os.WriteFile(filepath.Join(r.cfgDir, ConfigFile), []byte(newer), 0o600)
	if code, _, _ := r.run(r.cli.Unpair, ""); code != 1 || r.key() == "" {
		t.Fatalf("exit %d without --force", code)
	}
	code, out, errs := r.run(r.cli.Unpair, "", "--force")
	if code != 0 || r.key() != "" || r.config() != nil {
		t.Fatalf("exit %d, key %q: %s %s", code, r.key(), out, errs)
	}
	if !strings.Contains(out, "Send this to @registrar:kwak.test: revoke 77c0de11") || strings.Contains(out, "keyring") {
		t.Errorf("output %s", out)
	}
	if len(r.reg.log()) != 0 {
		t.Error("registrar called")
	}

	r = newRig(t)
	r.paired()
	os.WriteFile(filepath.Join(r.cfgDir, ConfigFile), []byte(`{"server_name":`), 0o600)
	code, out, _ = r.run(r.cli.Unpair, "", "--force")
	if code != 0 || r.config() != nil {
		t.Fatalf("bad json: exit %d: %s", code, out)
	}
	if !strings.Contains(out, "may still be in your keyring: service kwak, user <server name>/owner-key") ||
		!strings.Contains(out, "pairing id is unknown") {
		t.Errorf("bad json: output %s", out)
	}
}

func TestRemoveStateDir(t *testing.T) {
	r := newRig(t)
	r.stateFiles()
	if err := r.cli.removeStateDir(context.Background(), r.stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("state dir left")
	}
}

func TestStateDirGuard(t *testing.T) {
	home := "/home/u"
	for _, d := range []string{"", ".", "relative/kwak", "/", "/home", "/home/u", "/home/u/", "/home/u/.."} {
		if err := stateDirGuard(d, home); err == nil {
			t.Errorf("%q accepted", d)
		}
	}
	for _, d := range []string{"/home/u/.local/state/kwak", "/tmp/x/state", "/home/user2"} {
		if err := stateDirGuard(d, home); err != nil {
			t.Errorf("%q refused: %v", d, err)
		}
	}
	if err := stateDirGuard("/", ""); err == nil {
		t.Error("root accepted without a home")
	}
}
