//go:build integration && linux

// Package e2e runs the end-to-end scenarios of spec 10.4: tuwunel in docker, the registrar and two
// listeners in-process in the runner, and fake harness processes that start the real kwak binary.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/cli"
	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/listener"
	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/proc/osproc"
	"github.com/fcanela/kwak/internal/registrar"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/secrets/secretstest"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/test/integration/ittest"
)

func TestMain(m *testing.M) {
	switch os.Getenv(envFake) {
	case fakeHarness:
		os.Exit(fakeHarnessMain())
	case fakeTerm:
		os.Exit(fakeTermMain())
	case fakeShell:
		os.Exit(fakeShellMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// Wait is the default bound of every eventually-style wait.
const Wait = 45 * time.Second

// Server is the homeserver name of every test.
const Server = ittest.ServerName

// World is one test's whole system. Its goroutines and processes live until the top-level test
// ends, so subtests share it.
type World struct {
	Env     *ittest.Env
	Ctx     context.Context // ends with the top-level test
	Kwak    string          // the kwak binary inside the runner
	Room    string          // the agents room id
	Bot     string          // @registrar:kwak.test
	Admin   *Human
	Alice   *Owner
	Bob     *Owner
	Reg     *Registrar
	EdgeURL string       // https://127.0.0.1:<port>, the "server" in config.json
	HTTP    *http.Client // trusts the edge's certificate, never follows redirects

	top  *testing.T
	sink *sink
	cap  capture
}

// Human is a person's Matrix account.
type Human struct {
	Login matrix.Login
	C     *matrix.Client // straight to tuwunel, with X-Forwarded-For
	DM    string         // the DM the registrar made for this person
}

// Owner is one kwak owner with one machine: a keyring double, dirs, a socket and a listener.
type Owner struct {
	Name      string
	Human     *Human
	Machine   string
	Keys      *secretstest.Memory
	ConfigDir string
	StateDir  string
	Socket    string
	Clock     *Clock
	Runner    *RecRunner

	w   *World
	run *running
}

type running struct {
	cancel  context.CancelFunc
	done    chan error
	release func()
}

// Option changes how Start builds the world.
type Option func(*options)

type options struct {
	bare   bool
	regCfg []func(*registrar.Config)
}

// Bare leaves the owners unadded, unpaired and without listeners. E1 does those steps itself.
func Bare() Option { return func(o *options) { o.bare = true } }

// WithRegistrarConfig changes the registrar config before it starts.
func WithRegistrarConfig(f func(*registrar.Config)) Option {
	return func(o *options) { o.regCfg = append(o.regCfg, f) }
}

// Start builds the world. On the host it builds kwak, runs the calling test in the runner and
// returns nil; callers return at once on nil. It must be called from a top-level test.
func Start(t *testing.T, opts ...Option) *World {
	t.Helper()
	var o options
	for _, f := range opts {
		f(&o)
	}
	bin := ""
	if !ittest.Inner() {
		bin = buildKwak(t)
	}
	env := ittest.Start(t, ittest.WithBinary("kwak", bin))
	if env == nil {
		return nil
	}
	w := &World{Env: env, Ctx: t.Context(), Kwak: env.Binaries["kwak"], top: t}
	w.sink = &sink{w: t.Output()}
	t.Cleanup(w.sink.close)
	w.Bot = "@" + env.Registration.SenderLocalpart + ":" + Server
	w.Admin = &Human{Login: env.Admin, C: env.Client(env.Admin.AccessToken)}

	room, err := w.Admin.C.CreateRoom(w.Ctx, registrar.RoomBody(w.Bot))
	if err != nil {
		t.Fatalf("create the agents room: %v", err)
	}
	w.Room = room
	w.startEdge(t)
	w.Reg = w.newRegistrar(t, o.regCfg)
	w.Reg.Start(t)
	w.Admin.DM = w.Reg.WaitDM(t, env.Admin.UserID)
	if _, err := w.Admin.C.Join(w.Ctx, w.Admin.DM); err != nil {
		t.Fatalf("admin joins the DM: %v", err)
	}
	w.Alice = w.newOwner(t, "alice", env.Alice)
	w.Bob = w.newOwner(t, "bob", env.Bob)
	if !o.bare {
		for _, ow := range []*Owner{w.Alice, w.Bob} {
			w.AddOwner(t, ow)
			ow.StartListener(t)
			ow.Pair(t)
		}
	}
	return w
}

func buildKwak(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "kwak")
	gobin := "go"
	if _, err := exec.LookPath(gobin); err != nil {
		gobin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	cmd := exec.Command(gobin, "build", "-o", out, "github.com/fcanela/kwak/cmd/kwak")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build kwak: %v\n%s", err, b)
	}
	return out
}

// Logf writes a line to the test output, also from goroutines, until the test ends.
func (w *World) Logf(format string, a ...any) { fmt.Fprintf(w.sink, format+"\n", a...) }

// Log returns a logger that writes to the test output with the given label.
func (w *World) Log(label string) *slog.Logger {
	return slog.New(slog.NewTextHandler(w.sink, &slog.HandlerOptions{Level: slog.LevelInfo})).With("c", label)
}

// sink drops writes after the test ended, so late goroutines never touch a finished T.
type sink struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.w.Write(p)
	}
	return len(p), nil
}

func (s *sink) close() { s.mu.Lock(); s.closed = true; s.mu.Unlock() }

// ---- the edge (the reverse proxy's role): one https URL for /_kwak/v1 and /_matrix ----

func (w *World) startEdge(t *testing.T) {
	t.Helper()
	hs, err := url.Parse(w.Env.Homeserver)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(hs)
	director := proxy.Director
	proxy.Director = func(r *http.Request) { director(r); r.Host = hs.Host }
	proxy.ErrorLog = slog.NewLogLogger(w.Log("edge").Handler(), slog.LevelWarn)
	h := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if a, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && a != "" {
			w.cap.secret(a)
		}
		if r.URL.Query().Has("access_token") {
			http.Error(rw, "access_token in the URL", http.StatusBadRequest)
			return
		}
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		r.Header.Del("X-Forwarded-For")
		if strings.HasPrefix(r.URL.Path, wire.APIPrefix+"/") {
			r.Header.Set("X-Forwarded-For", ip)
			w.Reg.ServeAPI(rw, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/_matrix/client/") {
			w.cap.request(r)
			proxy.ServeHTTP(rw, r)
			return
		}
		http.NotFound(rw, r)
	})
	edge := httptest.NewUnstartedServer(h)
	edge.Config.ErrorLog = slog.NewLogLogger(w.Log("edge").Handler(), slog.LevelWarn)
	edge.StartTLS()
	t.Cleanup(edge.Close)
	w.EdgeURL = edge.URL
	w.HTTP = &http.Client{
		Transport:     edge.Client().Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ---- the registrar, in-process, restartable ----

// Registrar is the in-process registrar. Stop and Start keep the state file, like a container
// restart.
type Registrar struct {
	Cfg   registrar.Config
	Store *registrar.Store
	Srv   *registrar.Server
	Clock registrar.Clock

	w      *World
	api    atomic.Pointer[http.Handler]
	as     atomic.Pointer[http.Handler]
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *World) newRegistrar(t *testing.T, mods []func(*registrar.Config)) *Registrar {
	t.Helper()
	env := w.Env
	cfg := registrar.Config{
		ServerName: Server, Homeserver: env.Homeserver, Room: "#agents:" + Server,
		Admins: []string{env.Admin.UserID}, State: filepath.Join(t.TempDir(), "state.json"),
		HSToken: env.Registration.HSToken, BotLocalpart: env.Registration.SenderLocalpart,
		IdleAfter: registrar.Duration(3 * time.Second), CloseGrace: registrar.Duration(2 * time.Second),
		JobsEvery: registrar.Duration(time.Second), ClaimWait: registrar.Duration(20 * time.Second),
	}
	for _, f := range mods {
		f(&cfg)
	}
	cfg.Defaults()
	if err := cfg.Check(); err != nil {
		t.Fatalf("registrar config: %v", err)
	}
	r := &Registrar{Cfg: cfg, Clock: registrar.SystemClock{}, w: w}
	env.ServeAppservice(t, http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if h := r.as.Load(); h != nil {
			(*h).ServeHTTP(rw, req)
			return
		}
		http.Error(rw, `{"errcode":"M_UNAVAILABLE"}`, http.StatusServiceUnavailable)
	}))
	t.Cleanup(func() { r.Stop(t) })
	return r
}

// ServeAPI serves the listener API, or 502 while the registrar is stopped (what the proxy answers).
func (r *Registrar) ServeAPI(rw http.ResponseWriter, req *http.Request) {
	if h := r.api.Load(); h != nil {
		(*h).ServeHTTP(rw, req)
		return
	}
	http.Error(rw, "bad gateway", http.StatusBadGateway)
}

// Start opens the store and runs the registrar: Start (room join, admin DMs) and the jobs.
func (r *Registrar) Start(t *testing.T) {
	t.Helper()
	store, err := registrar.OpenStore(r.Cfg.State)
	if err != nil {
		t.Fatalf("open the registrar store: %v", err)
	}
	r.Store = store
	r.Srv = &registrar.Server{
		Cfg: r.Cfg, Store: store,
		HS:    registrar.NewHomeserver(r.Cfg.Homeserver, r.w.Env.Registration.ASToken, r.w.Env.HTTP),
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

// Stop stops serving and the jobs, then flushes the store. Safe to call twice.
func (r *Registrar) Stop(t *testing.T) {
	r.api.Store(nil)
	r.as.Store(nil)
	if r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
	r.cancel = nil
	if err := r.Store.Flush(); err != nil {
		t.Errorf("registrar flush: %v", err)
	}
}

// View reads the registrar state.
func (r *Registrar) View(f func(*registrar.State)) { r.Store.View(f) }

// WaitDM waits for the DM the registrar made for human.
func (r *Registrar) WaitDM(t *testing.T, human string) string {
	t.Helper()
	var dm string
	Eventually(t, "the DM of "+human, func() bool {
		r.View(func(st *registrar.State) { dm = st.DMs[human] })
		return dm != ""
	})
	return dm
}

// Agent returns the registrar's record of localpart, or nil.
func (r *Registrar) Agent(localpart string) *registrar.Agent {
	var a *registrar.Agent
	r.View(func(st *registrar.State) {
		if x := st.Agents[localpart]; x != nil {
			c := *x
			a = &c
		}
	})
	return a
}

// ---- owners, pairing and listeners ----

func (w *World) newOwner(t *testing.T, name string, l matrix.Login) *Owner {
	t.Helper()
	dir := t.TempDir()
	sock, err := os.MkdirTemp("", "kw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sock) })
	o := &Owner{
		Name: name, Human: &Human{Login: l, C: w.Env.Client(l.AccessToken)}, Machine: name + "-box",
		Keys: secretstest.Mem(), ConfigDir: filepath.Join(dir, "config"), StateDir: filepath.Join(dir, "state"),
		Socket: filepath.Join(sock, "kwak.sock"), Clock: &Clock{}, Runner: &RecRunner{}, w: w,
	}
	for _, d := range []string{o.ConfigDir, o.StateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { o.StopListener(t) })
	return o
}

// AddOwner has the admin send "owner add" and the human join the DM the registrar made.
func (w *World) AddOwner(t *testing.T, o *Owner) {
	t.Helper()
	Send(t, w.Admin.C, w.Admin.DM, "owner add "+o.Human.Login.UserID)
	WaitNotice(t, w.Admin.C, w.Admin.DM, "Added "+o.Human.Login.UserID+" as owner "+o.Name+".")
	o.Human.DM = w.Reg.WaitDM(t, o.Human.Login.UserID)
	if _, err := o.Human.C.Join(w.Ctx, o.Human.DM); err != nil {
		t.Fatalf("%s joins the DM: %v", o.Name, err)
	}
	WaitNotice(t, o.Human.C, o.Human.DM, "You are now a kwak owner as `"+o.Name+"`.")
}

// ConfigPath is the owner's config.json.
func (o *Owner) ConfigPath() string { return filepath.Join(o.ConfigDir, cli.ConfigFile) }

// Config reads config.json.
func (o *Owner) Config(t *testing.T) *wire.MachineConfig {
	t.Helper()
	c, err := listener.ReadMachineConfig(o.ConfigPath())
	if err != nil {
		t.Fatalf("read %s config: %v", o.Name, err)
	}
	return c
}

// Key returns the owner key in the keyring double, or "": from the item config.json names
// (secrets.KeyName, decision 40), or the old shared item when there is no config.json.
func (o *Owner) Key() string {
	k, _ := o.Keys.Get(o.KeyName())
	return k
}

// KeyName is the keyring item of this owner's pairing.
func (o *Owner) KeyName() string {
	if c, err := listener.ReadMachineConfig(o.ConfigPath()); err == nil {
		return c.KeyName()
	}
	return secrets.OwnerKeyName(Server)
}

// CLI returns kwak's human commands for this owner, with a terminal and no service manager.
func (o *Owner) CLI() *cli.CLI {
	return cli.New(cli.Env{
		Secrets: o.Keys, Service: noService{}, Preflight: func() error { return nil },
		Call: cli.SocketCaller(o.Socket), PrintHarness: func(io.Writer, wire.Harness) error { return nil },
		IsTerminal: func() bool { return true }, HTTP: o.w.HTTP,
		ConfigDir: func() (string, error) { return o.ConfigDir, nil },
		StateDir:  func() (string, error) { return o.StateDir, nil },
		Hostname:  func() (string, error) { return o.Machine, nil },
	})
}

// PairRun is a kwak pair in progress.
type PairRun struct {
	Code string // as printed, XXXX-XXXX-XXXX
	o    *Owner
	out  *lockedBuf
	err  *lockedBuf
	done chan int
}

var codeLine = regexp.MustCompile(`(?m)^    pair ([0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4})$`)

// StartPair runs kwak pair --owner <name> --server <edge> until it prints the code.
func (o *Owner) StartPair(t *testing.T) *PairRun {
	t.Helper()
	p := &PairRun{o: o, out: &lockedBuf{}, err: &lockedBuf{}, done: make(chan int, 1)}
	args := []string{"--owner", o.Name, "--server", o.w.EdgeURL}
	go func() { p.done <- o.CLI().Pair(args, strings.NewReader(""), p.out, p.err) }()
	Eventually(t, o.Name+"'s pairing code", func() bool {
		select {
		case c := <-p.done:
			p.done <- c
			t.Fatalf("kwak pair exited %d before printing a code:\n%s\n%s", c, p.out, p.err)
		default:
		}
		m := codeLine.FindStringSubmatch(p.out.String())
		if m != nil {
			p.Code = m[1]
		}
		return m != nil
	})
	return p
}

// Wait waits for kwak pair to exit and returns its code, stdout and stderr.
func (p *PairRun) Wait(t *testing.T) (int, string, string) {
	t.Helper()
	select {
	case c := <-p.done:
		return c, p.out.String(), p.err.String()
	case <-time.After(90 * time.Second):
		t.Fatalf("kwak pair did not finish:\n%s\n%s", p.out, p.err)
	}
	return 0, "", ""
}

// PairResult is a finished pairing.
type PairResult struct {
	Out       string // kwak pair's stdout
	PairingID string
	Key       string
}

// Pair runs kwak pair, sends the code from the owner's DM, and waits for it to succeed and for the
// listener (if running) to sync.
func (o *Owner) Pair(t *testing.T) PairResult {
	t.Helper()
	p := o.StartPair(t)
	Send(t, o.Human.C, o.Human.DM, "pair "+p.Code)
	code, out, errOut := p.Wait(t)
	if code != 0 {
		t.Fatalf("kwak pair for %s exited %d:\n%s\n%s", o.Name, code, out, errOut)
	}
	r := PairResult{Out: out, PairingID: o.Config(t).PairingID, Key: o.Key()}
	o.w.cap.secret(r.Key)
	if o.run != nil {
		o.WaitSyncing(t)
	}
	return r
}

// StartListener runs the real listener in-process, as kwak listen does: listen.lock, stale socket,
// ipc.Listen, listener.New, Run.
func (o *Owner) StartListener(t *testing.T) {
	t.Helper()
	if o.run != nil {
		t.Fatalf("%s's listener is already running", o.Name)
	}
	release, err := ipc.Lock(filepath.Join(o.StateDir, "listen.lock"))
	if err != nil {
		t.Fatalf("listen.lock: %v", err)
	}
	if err := ipc.RemoveStale(o.Socket); err != nil {
		release()
		t.Fatal(err)
	}
	ln, err := ipc.Listen(o.Socket, ipc.Policy{})
	if err != nil {
		release()
		t.Fatalf("listen on %s: %v", o.Socket, err)
	}
	l, err := listener.New(listener.Config{ConfigPath: o.ConfigPath()}, o.StateDir, o.Deps())
	if err != nil {
		ln.Close()
		release()
		t.Fatalf("listener.New: %v", err)
	}
	ctx, cancel := context.WithCancel(o.w.Ctx)
	r := &running{cancel: cancel, done: make(chan error, 1), release: release}
	go func() {
		err := l.Run(ctx, ln)
		ln.Close()
		os.Remove(o.Socket)
		r.done <- err
	}()
	o.run = r
}

// Deps are the listener's real dependencies, with the owner's keyring double, clock and runner.
func (o *Owner) Deps() listener.Deps {
	cfg := o.ConfigPath()
	httpc := o.w.HTTP
	return listener.Deps{
		Secrets:   o.Keys,
		Registrar: cli.ListenerRegistrar(cfg, httpc),
		Matrix: func(token string) listener.Matrix {
			base := ""
			if c, err := listener.ReadMachineConfig(cfg); err == nil {
				base = c.Server
			}
			return &matrix.Client{Base: base, Token: token, HTTP: httpc}
		},
		Procs:   osproc.New(),
		PeerPID: ipc.PeerPID,
		Runner:  o.Runner,
		Clock:   o.Clock,
		Rand:    rand.Reader,
		Log:     o.w.Log("listener-" + o.Name),
	}
}

// StopListener stops the listener like a SIGTERM and waits for it. Safe when it is not running.
func (o *Owner) StopListener(t *testing.T) {
	if o.run == nil {
		return
	}
	r := o.run
	o.run = nil
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("%s's listener: %v", o.Name, err)
		}
	case <-time.After(30 * time.Second):
		t.Errorf("%s's listener did not stop", o.Name)
	}
	r.release()
}

// Call sends one IPC request to the listener, as the human commands do.
func (o *Owner) Call(t *testing.T, op string, reply any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(o.w.Ctx, 10*time.Second)
	defer cancel()
	if err := cli.SocketCaller(o.Socket)(ctx, wire.Head{V: wire.ProtocolVersion, Op: op}, reply); err != nil {
		t.Fatalf("%s %s: %v", o.Name, op, err)
	}
}

// Status is the listener's status op.
func (o *Owner) Status(t *testing.T) wire.StatusResponse {
	t.Helper()
	var s wire.StatusResponse
	o.Call(t, wire.OpStatus, &s)
	return s
}

// WaitSyncing waits until the listener reports a running sync.
func (o *Owner) WaitSyncing(t *testing.T) {
	t.Helper()
	var last wire.StatusResponse
	EventuallyMsg(t, o.Name+"'s listener syncing", func() (bool, string) {
		last = o.Status(t)
		return last.State == wire.StateSyncing && !last.LastSync.IsZero(), fmt.Sprintf("%+v", last)
	})
}

// Session returns the status of session h, and whether the listener knows it.
func (o *Owner) Session(t *testing.T, h *FakeHarness) (wire.SessionStatus, bool) {
	t.Helper()
	key := string(h.Harness) + ":" + h.SessionID
	for _, s := range o.Status(t).Sessions {
		if s.Key == key {
			return s, true
		}
	}
	return wire.SessionStatus{}, false
}

// WaitAgent waits until the listener reports an agent for h's session and returns its user id. A
// session gets its agent at its first send, reply or helper call (decision 48), so a session with
// none yet first sends one open message, as a session does before others can write to it.
func (o *Owner) WaitAgent(t *testing.T, h *FakeHarness) string {
	t.Helper()
	var s wire.SessionStatus
	if s, _ = o.Session(t, h); s.Agent == "" {
		h.MustCall(t, wire.ToolSend, map[string]any{"text": "E2E: " + h.SessionID + " joins kwak"})
	}
	Eventually(t, "an agent for "+h.SessionID, func() bool {
		s, _ = o.Session(t, h)
		return s.Agent != ""
	})
	return s.Agent
}

// WaitInbox waits until h's session has exactly n waiting items, as kwak status counts them: the
// session's queue plus its helpers' queues.
func (o *Owner) WaitInbox(t *testing.T, h *FakeHarness, n int) {
	t.Helper()
	var s wire.SessionStatus
	EventuallyMsg(t, fmt.Sprintf("%d items for %s", n, h.SessionID), func() (bool, string) {
		s, _ = o.Session(t, h)
		return s.Inbox == n, fmt.Sprintf("inbox %d", s.Inbox)
	})
}

type noService struct{}

func (noService) Install(string) error    { return nil }
func (noService) Uninstall() error        { return nil }
func (noService) Start() error            { return nil }
func (noService) Stop() error             { return nil }
func (noService) Status() (string, error) { return "not installed", nil }

// ---- doubles: clock and runner ----

// Clock is the wall clock plus an offset that Advance moves, so tests can make the listener's
// wall-clock deadlines (close grace, question timeout) fall due.
type Clock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *Clock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); c.off += d; c.mu.Unlock() }

// RecRunner records background resume runs and runs nothing.
type RecRunner struct {
	mu   sync.Mutex
	runs []Run
}

// Run is one recorded background resume.
type Run struct {
	Dir  string
	Argv []string
}

func (r *RecRunner) Start(_ context.Context, dir string, argv []string, _ io.Writer) (func() error, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, Run{Dir: dir, Argv: slices.Clone(argv)})
	return func() error { return nil }, nil
}

// Runs returns the recorded runs.
func (r *RecRunner) Runs() []Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.runs)
}

// ---- capture for the I1 scan (E16) ----

type capture struct {
	mu       sync.Mutex
	outputs  []Captured
	secrets  map[string]bool
	requests []EdgeRequest
}

// EdgeRequest is one /_matrix request that passed the edge.
type EdgeRequest struct {
	At     time.Time
	Method string
	Path   string // unescaped path, no query
	Query  url.Values
	Token  string
}

func (c *capture) request(r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c.mu.Lock()
	c.requests = append(c.requests, EdgeRequest{time.Now(), r.Method, r.URL.Path, r.URL.Query(), tok})
	c.mu.Unlock()
}

// Requests returns the /_matrix requests the edge passed since t0.
func (w *World) Requests(t0 time.Time) []EdgeRequest {
	w.cap.mu.Lock()
	defer w.cap.mu.Unlock()
	var out []EdgeRequest
	for _, r := range w.cap.requests {
		if !r.At.Before(t0) {
			out = append(out, r)
		}
	}
	return out
}

// Captured is one hook stdout or MCP line a harness received.
type Captured struct {
	What string
	Data []byte
}

func (c *capture) add(what string, b []byte) {
	c.mu.Lock()
	c.outputs = append(c.outputs, Captured{what, slices.Clone(b)})
	c.mu.Unlock()
}

func (c *capture) secret(s string) {
	if s == "" {
		return
	}
	c.mu.Lock()
	if c.secrets == nil {
		c.secrets = map[string]bool{}
	}
	c.secrets[s] = true
	c.mu.Unlock()
}

// Captured returns every hook stdout and MCP output line so far.
func (w *World) Captured() []Captured {
	w.cap.mu.Lock()
	defer w.cap.mu.Unlock()
	return slices.Clone(w.cap.outputs)
}

// Secrets returns every bearer value the edge saw (owner keys, claim secrets, standing and agent
// tokens) and every owner key the pairings stored.
func (w *World) Secrets() []string {
	w.cap.mu.Lock()
	defer w.cap.mu.Unlock()
	var out []string
	for s := range w.cap.secrets {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// ---- Matrix helpers ----

var txn atomic.Int64

// Send sends an m.text message and returns its event id.
func Send(t *testing.T, c *matrix.Client, room, body string) string {
	t.Helper()
	return SendContent(t, c, room, map[string]any{"msgtype": "m.text", "body": body})
}

// SendContent sends an m.room.message with content.
func SendContent(t *testing.T, c *matrix.Client, room string, content any) string {
	t.Helper()
	id, err := c.Send(t.Context(), room, fmt.Sprintf("e2e-%d-%d", time.Now().UnixNano(), txn.Add(1)), content)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return id
}

// ThreadReply is the content of a reply in thread root with no mentions.
func ThreadReply(root, body string) map[string]any {
	return map[string]any{
		"msgtype": "m.text", "body": body,
		"m.relates_to": map[string]any{
			"rel_type": "m.thread", "event_id": root, "is_falling_back": true,
			"m.in_reply_to": map[string]any{"event_id": root},
		},
	}
}

// WaitNotice waits for a message in room whose body starts with prefix and returns the body.
func WaitNotice(t *testing.T, c *matrix.Client, room, prefix string) string {
	t.Helper()
	var found string
	var seen []string
	EventuallyMsg(t, "a message starting "+prefix, func() (bool, string) {
		page, err := c.Messages(t.Context(), room, "", 50, "")
		if err != nil {
			return false, err.Error()
		}
		seen = seen[:0]
		for _, ev := range page.Chunk {
			var m struct{ Body string }
			json.Unmarshal(ev.Content, &m)
			seen = append(seen, m.Body)
			if strings.HasPrefix(m.Body, prefix) {
				found = m.Body
				return true, ""
			}
		}
		return false, fmt.Sprintf("seen %q", seen)
	})
	return found
}

// HS calls tuwunel directly with token (and ?user_id=asUser when set), with X-Forwarded-For.
func (w *World) HS(t *testing.T, method, path, token, asUser string) (int, []byte) {
	t.Helper()
	u := w.Env.Homeserver + path
	if asUser != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		u += sep + "user_id=" + url.QueryEscape(asUser)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", ittest.ForwardedFor)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := w.Env.HTTP.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b
}

// DisplayName returns uid's display name.
func (w *World) DisplayName(t *testing.T, uid string) string {
	t.Helper()
	code, b := w.HS(t, "GET", "/_matrix/client/v3/profile/"+url.PathEscape(uid)+"/displayname", w.Admin.Login.AccessToken, "")
	var p struct {
		Displayname string `json:"displayname"`
	}
	json.Unmarshal(b, &p)
	if code != 200 {
		t.Fatalf("display name of %s: %d %s", uid, code, b)
	}
	return p.Displayname
}

// Devices returns the device ids of uid, read with the as_token.
func (w *World) Devices(t *testing.T, uid string) []string {
	t.Helper()
	code, b := w.HS(t, "GET", "/_matrix/client/v3/devices", w.Env.Registration.ASToken, uid)
	if code != 200 {
		t.Fatalf("devices of %s: %d %s", uid, code, b)
	}
	var r struct {
		Devices []struct {
			DeviceID string `json:"device_id"`
		} `json:"devices"`
	}
	json.Unmarshal(b, &r)
	var ids []string
	for _, d := range r.Devices {
		ids = append(ids, d.DeviceID)
	}
	return ids
}

// Members returns the joined members of the agents room.
func (w *World) Members(t *testing.T) []string {
	t.Helper()
	m, err := w.Admin.C.JoinedMembers(t.Context(), w.Room)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	return m
}

// WaitMember waits until uid is (in true) or is not (in false) a member of the agents room.
func (w *World) WaitMember(t *testing.T, uid string, in bool) {
	t.Helper()
	Eventually(t, fmt.Sprintf("%s in the room = %v", uid, in), func() bool {
		return slices.Contains(w.Members(t), uid) == in
	})
}

// AgentID is the first-ladder user id of a session agent (spec 3.6).
func AgentID(owner string, h wire.Harness, sessionID string) string {
	hx, err := wire.SessionHex(sessionID)
	if err != nil {
		panic(err)
	}
	return "@" + wire.Candidates(owner, h, hx)[0] + ":" + Server
}

// Short drops ":kwak.test", as the listener does in what it shows.
func Short(uid string) string { return strings.TrimSuffix(uid, ":"+Server) }

// Item builds the render item the listener should queue for event id, read back from tuwunel.
func (w *World) Item(t *testing.T, eventID string, reason render.Reason, root string) render.Item {
	t.Helper()
	ev, err := w.Admin.C.Event(t.Context(), w.Room, eventID)
	if err != nil {
		t.Fatalf("read %s: %v", eventID, err)
	}
	var c struct {
		Body string `json:"body"`
	}
	json.Unmarshal(ev.Content, &c)
	return render.Item{EventID: ev.EventID, Sender: Short(ev.Sender), ThreadRoot: root,
		SentAt: time.UnixMilli(ev.TS).UTC(), Reason: reason, Body: c.Body}
}

// Own marks an item as a reply in a thread its recipient started, so it gets no refusal sentence
// (spec 6.1).
func Own(it render.Item) render.Item {
	it.OwnThread = true
	return it
}

// Budget is the per-delivery budget of a harness (spec 5.5).
func Budget(h wire.Harness) int {
	if h == wire.HarnessCodex {
		return render.BudgetCodex
	}
	return render.BudgetClaude
}

// Rendered is the text a hook carries for items delivered to me.
func Rendered(me string, h wire.Harness, items ...render.Item) string {
	text, _ := render.Delivery(Short(me), items, Budget(h))
	return text
}

// ContextJSON is the exact stdout of a hook that adds context (spec 6.2, 6.3).
func ContextJSON(event, text string) []byte {
	type specific struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	return encode(struct {
		HSO specific `json:"hookSpecificOutput"`
	}{specific{event, text}})
}

// BlockJSON is the exact stdout of Codex's Stop notice (spec 6.3).
func BlockJSON(reason string) []byte {
	return encode(struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}{"block", reason})
}

func encode(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.Encode(v)
	return b.Bytes()
}

// SameJSON fails unless got is exactly want, byte for byte.
func SameJSON(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	t.Fatalf("%s:\n got %s\nwant %s", what, got, want)
}

// ---- waits and ids ----

// Eventually polls f every 100 ms until it is true, or fails after Wait.
func Eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	EventuallyMsg(t, what, func() (bool, string) { return f(), "" })
}

// EventuallyMsg is Eventually with a last-state message for the failure.
func EventuallyMsg(t *testing.T, what string, f func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(Wait)
	for {
		ok, msg := f()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s; %s", Wait, what, msg)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// UUIDv4 is a random Claude-shaped session id.
func UUIDv4() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmtUUID(b)
}

// UUIDv7 is a Codex-shaped session id: 48 bits of milliseconds, then random.
func UUIDv7() string {
	var b [16]byte
	rand.Read(b[:])
	ms := uint64(time.Now().UnixMilli())
	for i := range 6 {
		b[i] = byte(ms >> (40 - 8*i))
	}
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmtUUID(b)
}

func fmtUUID(b [16]byte) string {
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// lines splits a reader into lines on a channel, closed at EOF.
func lines(r io.Reader) <-chan []byte {
	ch := make(chan []byte, 64)
	go func() {
		defer close(ch)
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 64<<10), 16<<20)
		for s.Scan() {
			ch <- slices.Clone(s.Bytes())
		}
	}()
	return ch
}
