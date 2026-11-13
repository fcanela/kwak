// Package listener is the kwak daemon (spec 5): one /sync per machine as its pairing's own listener
// account (the standing account, decision 42), routing to local sessions, per-session inboxes, the local IPC server and the tools.
package listener

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fcanela/kwak/internal/check"
	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/wire"
)

// Registrar is the HTTPS client of the registrar's /_kwak/v1 API, authenticated with the owner key.
// Errors the registrar answered are *wire.APIError with Status set.
type Registrar interface {
	Standing(ctx context.Context) (wire.Standing, error)
	Agent(ctx context.Context, r wire.AgentRequest) (wire.Agent, error)
	Close(ctx context.Context, userIDs []string, reason string) error
	Alive(ctx context.Context, userIDs []string, touch bool) (unknown, closed []string, err error)
	Me(ctx context.Context) (wire.Me, error)
	Unpair(ctx context.Context) error
	// ReportJoin joins userID, one of this pairing's own accounts, to the pairing's report room
	// and returns the room id (5.17). wire.ErrNoReportRoom when the server has none for it.
	ReportJoin(ctx context.Context, userID string) (roomID string, err error)
	// Responder mints a token for the pairing's responder account, making it first when needed
	// (decision 46). Each call logs the previous token out.
	Responder(ctx context.Context) (wire.Agent, error)
}

// KeySetter is implemented by a Registrar that takes its owner key from the listener. The listener
// calls SetKey after every keyring read, so the key of a re-pair is used at once.
type KeySetter interface{ SetKey(ownerKey string) }

// Matrix is one homeserver client with one token.
type Matrix interface {
	Sync(ctx context.Context, since, filter string, timeout time.Duration) (*matrix.SyncResponse, error)
	Send(ctx context.Context, roomID, txnID string, content any) (string, error)
	React(ctx context.Context, roomID, txnID, eventID, key string) (string, error)
	Messages(ctx context.Context, roomID, from string, limit int, filter string) (*matrix.MessagesResponse, error)
	Event(ctx context.Context, roomID, eventID string) (*matrix.Event, error)
	ThreadReplies(ctx context.Context, roomID, rootID string, limit int) ([]matrix.Event, error)
	JoinedMembers(ctx context.Context, roomID string) ([]string, error)
	Leave(ctx context.Context, roomID string) error // after a report (5.17)
}

// Runner starts a background resume or a responder run; os/exec in production. Cancelling ctx
// kills the run.
type Runner interface {
	Start(ctx context.Context, dir string, argv []string, log io.Writer) (wait func() error, err error)
}

// Clock gives wall-clock time and timers. Deadlines are wall-clock times in state.json.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Deps are the outside world of a listener.
type Deps struct {
	Secrets   secrets.Secrets
	Registrar Registrar
	Matrix    func(token string) Matrix
	Procs     proc.Reader
	PeerPID   func(net.Conn) (pid int, ok bool, err error) // ipc.PeerPID in production
	Runner    Runner
	Check     check.Runner // the check commands (5.16); check.Exec when nil
	Clock     Clock
	Rand      io.Reader
	Log       *slog.Logger
}

// Config configures one listener. ConfigPath is required.
type Config struct {
	ConfigPath string               // config.json (spec 5.2)
	Version    string               // this kwak's version; "" means wire.Version
	Budget     map[wire.Harness]int // characters per delivery; default 8,000 on Claude, 6,000 on Codex
	CheckDir   func(string) bool    // the cwd check of 5.6; default: absolute, clean, an existing directory
}

// ConfigVersion is the config.json version this kwak writes.
const ConfigVersion = wire.ConfigVersion

// MachineConfig is config.json (spec 5.2): machine facts only, never a secret.
type MachineConfig = wire.MachineConfig

// ErrConfigNewer stops the listener when config.json or state.json is newer than this kwak.
var ErrConfigNewer = wire.ErrConfigNewer

// ReadMachineConfig reads config.json. A missing file gives an error matching fs.ErrNotExist.
func ReadMachineConfig(path string) (*MachineConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := wire.ParseMachineConfig(data, false)
	if errors.Is(err, ErrConfigNewer) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("listener: %s: %w", path, err)
	}
	return &c, nil
}

// Texts the listener answers with (spec 5.1 table, 5.9, 9).
const (
	textNotPaired    = wire.TextNotPaired
	textUnpaired     = wire.TextUnpaired
	textKeyMissing   = wire.TextKeyMissing
	textRemoved      = "your kwak owner account was removed; ask the server admin"
	textKeyring      = "waiting for the keyring (locked?)"
	textNotYours     = "this session's agent belongs to an older pairing of this machine that was revoked, so kwak cannot reopen it. Start a new session."
	textParentClosed = "your session's agent is closed; try again in a moment"
	textBadSession   = "kwak could not use this session id"
	textNames        = "kwak could not name an agent for this session"
	textNotSeen      = "kwak has not seen this session start yet; try again in a moment. If it keeps saying this, the kwak listener was not running when this session started: start a new session, or run /clear in Claude Code"
	textOlder        = wire.TextListenerOlder
	textNoPoll       = "Replies in this thread and messages that mention you are shown to you automatically. There is nothing to poll."
)

// Timings of spec 5.1 and 5.8 to 5.11.
const (
	closeGrace    = 10 * time.Minute
	deadlineEvery = 10 * time.Second
	livenessEvery = time.Minute
	hourly        = time.Hour
	pingEvery     = 24 * time.Hour
	jumpSlack     = 2 * time.Minute
	keyringRetry  = 30 * time.Second
	sessionWait   = 2 * time.Second
	ensureWait    = 10 * time.Second
	noHookEnd     = 14 * 24 * time.Hour
	keepEnded     = 90 * 24 * time.Hour
	resumeDelay   = 30 * time.Second
	membersTTL    = time.Minute
	chainDepth    = proc.ChainDepth
	aliveChunk    = wire.MaxIDs
)

// Listener is the daemon. New builds it; Run serves.
type Listener struct {
	cfg  Config
	dir  string
	d    Deps
	log  *slog.Logger
	ver  string
	bg   context.Context
	stop context.CancelFunc

	conf atomic.Pointer[MachineConfig] // replaced whole on reload, never mutated

	mu         sync.Mutex
	st         *state
	idx        *Index
	phase      string // a wire.State* value
	detail     string // why unpaired or waiting
	syncErr    string
	lastSync   time.Time
	stateReset bool
	lastTick   time.Time
	fatal      error // set when the listener must stop, returned by Run

	tokens    map[string]string // agent user id -> token; never in state
	standing  string
	standUser string // the standing token's account, as POST /standing named it (standingUserLocked)
	flights   map[string]*flight
	retrying  map[string]bool
	conns     map[*client]struct{}
	changed   chan struct{} // closed and replaced when sessions change
	running   map[string]*resumeRun
	self      int                            // this process's pid: a caller chain through it is a run (5.11)
	shown     map[string]map[string][]string // agent (flightKey) -> thread -> messages printed since its last post there
	pausedOn  map[string]bool                // messages that got ⏸️ (5.15)
	blocked   map[string]bool                // agent (flightKey) and message that got ⛔ for a refused post (5.16)
	checkIn   string                         // the incoming check command, read before each routed batch (5.16)
	checks    chan struct{}                  // bounds incoming check commands running at once
	checkMu   sync.Mutex
	live      liveConf        // what was read last from config.json at use (5.2)
	checkBad  bool            // config.json was unreadable at the last read; logged once
	opening   map[string]bool // sessions whose owner's SessionStart is stopping their run (5.11)
	hourRuns  []time.Time     // background runs started in the last hour, oldest first; memory only (5.11)
	hourFull  bool            // the hourly limit held a run since the last start; logged once
	members   []string
	membAt    time.Time
	membGone  map[string]bool // ids a fresh fetch of members did not hold; reset with each fetch
	dirty     bool

	// The responder account (decision 46), memory only. respMu runs one POST /responder at a
	// time; respErr and respWait hold back a new one for a while after a failure.
	respMu   sync.Mutex
	respUser string
	respErr  error
	respWait time.Time

	// Reports (5.17), memory only. repMu runs one report at a time.
	repMu    sync.Mutex
	repNone  time.Time // no report room for this pairing: skip reports until then
	repLimit time.Time // when the hourly limit on runs was last reported

	saveMu   sync.Mutex
	reloadCh chan chan error
	recheck  chan struct{}
	halt     chan struct{} // stops sync after an owner removal
	wg       sync.WaitGroup
}

// New loads config.json and state.json. It fails only when either is newer than this kwak.
func New(cfg Config, stateDir string, d Deps) (*Listener, error) {
	if d.Secrets == nil || d.Registrar == nil || d.Matrix == nil || d.Procs == nil || d.Clock == nil {
		return nil, errors.New("listener: Secrets, Registrar, Matrix, Procs and Clock are required")
	}
	if d.Rand == nil {
		d.Rand = rand.Reader
	}
	if d.Check == nil {
		d.Check = check.Exec{}
	}
	d.Clock = wallClock{d.Clock}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.PeerPID == nil {
		d.PeerPID = func(net.Conn) (int, bool, error) { return 0, false, nil }
	}
	if cfg.Version == "" {
		cfg.Version = wire.Version
	}
	if cfg.CheckDir == nil {
		cfg.CheckDir = realDir
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	conf, err := ReadMachineConfig(cfg.ConfigPath)
	switch {
	case errors.Is(err, ErrConfigNewer):
		return nil, err
	case errors.Is(err, fs.ErrNotExist):
		conf = nil
	case err != nil:
		d.Log.Error("config.json unreadable", "err", err)
		conf = nil
	}
	st, reset, err := loadState(stateDir)
	if err != nil {
		return nil, err
	}
	bg, stop := context.WithCancel(context.Background())
	l := &Listener{
		cfg: cfg, dir: stateDir, d: d, log: d.Log, ver: cfg.Version, bg: bg, stop: stop,
		st: st, stateReset: reset,
		tokens: map[string]string{}, flights: map[string]*flight{}, retrying: map[string]bool{}, conns: map[*client]struct{}{},
		changed: make(chan struct{}), running: map[string]*resumeRun{}, self: os.Getpid(),
		shown: map[string]map[string][]string{}, pausedOn: map[string]bool{}, opening: map[string]bool{},
		blocked: map[string]bool{}, checks: make(chan struct{}, checkMax),
		reloadCh: make(chan chan error), recheck: make(chan struct{}, 1), halt: make(chan struct{}, 1),
	}
	l.conf.Store(conf)
	if conf != nil {
		l.live = liveOf(conf)
	}
	if conf == nil {
		l.phase, l.detail = wire.StateUnpaired, textNotPaired
	} else {
		l.phase = wire.StateConnecting
	}
	l.idx = newIndex(st, l.roomID())
	l.idx.Incomplete = reset
	return l, nil
}

func realDir(p string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// wallClock drops the monotonic reading of Now, so durations and deadlines follow the wall clock
// across a suspend (spec 5.8).
type wallClock struct{ Clock }

func (c wallClock) Now() time.Time { return c.Clock.Now().Round(0) }

func (l *Listener) roomID() string {
	if c := l.conf.Load(); c != nil {
		return c.RoomID
	}
	return ""
}

func (l *Listener) serverName() string {
	if c := l.conf.Load(); c != nil {
		return c.ServerName
	}
	return ""
}

// Run serves IPC on ln and runs the workers until ctx ends or the shutdown op arrives. The caller
// holds listen.lock and removed a stale socket before listening (spec 5.1).
func (l *Listener) Run(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		l.stop()
	}()
	l.mu.Lock()
	l.lastTick = l.d.Clock.Now()
	l.mu.Unlock()
	l.save()
	// Checks a restart left waiting start before the sync can add new ones (5.16).
	l.resumeChecks()
	l.spawn(func() { l.supervise(l.bg) })
	l.spawn(func() {
		l.deadlines()
		l.every(deadlineEvery, l.deadlines)
	})
	l.spawn(func() { l.every(livenessEvery, l.liveness) })
	l.spawn(func() { l.every(hourly, l.hourly) })
	l.spawn(func() { l.every(time.Second, l.flushDirty) })
	go func() {
		<-l.bg.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}
		l.spawn(func() { l.serveConn(c) })
	}
	l.stop()
	l.wg.Wait()
	err := l.save()
	l.mu.Lock()
	if l.fatal != nil {
		err = l.fatal
	}
	l.mu.Unlock()
	return err
}

func (l *Listener) spawn(fn func()) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		fn()
	}()
}

func (l *Listener) every(d time.Duration, fn func()) {
	for {
		select {
		case <-l.bg.Done():
			return
		case <-l.d.Clock.After(d):
			fn()
		}
	}
}

// sleep waits d on the Clock, or returns false when the listener stops.
func (l *Listener) sleep(d time.Duration) bool {
	select {
	case <-l.bg.Done():
		return false
	case <-l.d.Clock.After(d):
		return true
	}
}

func (l *Listener) randomHex(n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(l.d.Rand, b); err != nil {
		panic(fmt.Sprintf("listener: random source failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// backoff is 1 s doubling to 60 s, with jitter (spec 5.1).
func (l *Listener) backoff(n int) time.Duration {
	d := time.Minute
	if n < 6 {
		d = time.Second << n
	}
	var b [8]byte
	io.ReadFull(l.d.Rand, b[:])
	return d/2 + time.Duration(binary.LittleEndian.Uint64(b[:])%uint64(d/2+1))
}

// ---- pairing state and the standing token ----

// errUnpaired carries the reload detail and the status detail of an unpaired listener.
type errUnpaired struct{ reload, status string }

func (e *errUnpaired) Error() string { return e.reload }

var errKeyring = errors.New(textKeyring)

func apiStatus(err error) (int, string, bool) {
	var ae *wire.APIError
	if errors.As(err, &ae) {
		return ae.Status, ae.Errcode, true
	}
	return 0, "", false
}

// describe is short human text for a registrar failure. It never holds a URL or a token.
func describe(err error) string {
	if st, code, ok := apiStatus(err); ok {
		if code != "" {
			return fmt.Sprintf("registrar answered HTTP %d %s", st, code)
		}
		return fmt.Sprintf("registrar unreachable, HTTP %d", st)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "registrar did not answer in time"
	}
	return "registrar unreachable"
}

func (l *Listener) setUnpaired(detail string) {
	l.mu.Lock()
	l.phase, l.detail, l.standing, l.standUser = wire.StateUnpaired, detail, "", ""
	l.mu.Unlock()
	l.log.Warn("unpaired", "why", detail)
}

// connect reads config.json and the key, gets the standing token and the owner's settings.
func (l *Listener) connect(ctx context.Context) error {
	conf, err := ReadMachineConfig(l.cfg.ConfigPath)
	if errors.Is(err, ErrConfigNewer) {
		l.log.Error("stopping", "err", err)
		l.mu.Lock()
		l.fatal = err
		l.mu.Unlock()
		return err
	}
	if err != nil {
		d := textNotPaired
		if !errors.Is(err, fs.ErrNotExist) {
			d = err.Error()
		}
		l.conf.Store(nil)
		l.setUnpaired(d)
		return &errUnpaired{d, d}
	}
	l.mu.Lock()
	l.conf.Store(conf)
	l.idx.Room = conf.RoomID
	l.mu.Unlock()
	key, err := l.d.Secrets.Get(conf.KeyName())
	if errors.Is(err, secrets.ErrNotFound) {
		l.setUnpaired(textKeyMissing)
		return &errUnpaired{textKeyMissing, textKeyMissing}
	}
	if err != nil {
		l.mu.Lock()
		l.phase, l.detail = wire.StateWaitingKeyring, textKeyring
		l.mu.Unlock()
		return errKeyring
	}
	if ks, ok := l.d.Registrar.(KeySetter); ok {
		ks.SetKey(key)
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	s, err := l.d.Registrar.Standing(sctx)
	cancel()
	if err != nil {
		st, code, _ := apiStatus(err)
		switch {
		case st == 401:
			l.setUnpaired(textUnpaired)
			return &errUnpaired{fmt.Sprintf("the registrar refused the key: %d %s", st, code), textUnpaired}
		case code == wire.ErrOwnerRemoved:
			l.setUnpaired(textRemoved)
			return &errUnpaired{textRemoved, textRemoved}
		}
		l.mu.Lock()
		l.phase, l.detail, l.syncErr = wire.StateConnecting, "", describe(err)
		l.mu.Unlock()
		return err
	}
	l.mu.Lock()
	if l.standUser != "" && l.standUser != s.UserID {
		// The registrar moved this pairing to another account (decision 43: the per-owner account
		// was closed and this pairing got its own, or a rename). The sync keeps its next_batch: the gap fill and
		// last_event_id (5.3) route each message once, and a reset would drop the gap.
		l.log.Info("listener account changed", "from", l.standUser, "to", s.UserID)
	}
	l.standing, l.standUser = s.AccessToken, s.UserID
	l.phase, l.detail, l.syncErr = wire.StateConnecting, "", ""
	l.repNone = time.Time{} // a reload asks for the report room again (5.17)
	l.mu.Unlock()
	l.refreshMe(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.phase == wire.StateUnpaired {
		return &errUnpaired{l.detail, l.detail}
	}
	return nil
}

func (l *Listener) refreshMe(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	me, err := l.d.Registrar.Me(ctx)
	if err != nil {
		l.regFailed(err)
		return
	}
	l.mu.Lock()
	s := me.Settings
	l.st.Settings = &s
	l.st.KeyExpires = me.KeyExpiresAt
	l.st.MeAt = l.d.Clock.Now()
	l.dirty = true
	l.mu.Unlock()
}

// regFailed reacts to a registrar 401 or owner removal seen outside connect (spec 5.1).
func (l *Listener) regFailed(err error) {
	st, code, _ := apiStatus(err)
	switch {
	case st == 401:
		select {
		case l.recheck <- struct{}{}:
		default:
		}
	case code == wire.ErrOwnerRemoved:
		l.setUnpaired(textRemoved)
		select {
		case l.halt <- struct{}{}:
		default:
		}
	}
}

// supervise owns the standing token and the sync loop: connect, sync, reconnect, reload.
func (l *Listener) supervise(ctx context.Context) {
	var (
		cancelSync context.CancelFunc
		syncDone   chan error
		retry      <-chan time.Time
		fails      int
		first      = true
	)
	stopSync := func() {
		if cancelSync != nil {
			cancelSync()
			<-syncDone
			cancelSync, syncDone = nil, nil
		}
	}
	attempt := func() error {
		retry = nil
		err := l.connect(ctx)
		var up *errUnpaired
		switch {
		case err == nil:
			fails = 0
			l.mu.Lock()
			tok := l.standing
			l.mu.Unlock()
			sctx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			cancelSync, syncDone = cancel, done
			go func() { done <- l.syncLoop(sctx, tok) }()
			if first {
				first = false
				go l.alive(pingReconcile)
			}
		case errors.As(err, &up):
		case errors.Is(err, errKeyring):
			retry = l.d.Clock.After(keyringRetry)
		default:
			retry = l.d.Clock.After(l.backoff(fails))
			fails++
		}
		return err
	}
	if l.conf.Load() != nil {
		attempt()
	}
	for {
		l.mu.Lock()
		fatal := l.fatal
		l.mu.Unlock()
		if fatal != nil {
			stopSync()
			l.stop()
			return
		}
		select {
		case <-ctx.Done():
			stopSync()
			return
		case reply := <-l.reloadCh:
			stopSync()
			wasFirst := first
			first = false
			err := attempt()
			reply <- err
			if err != nil {
				first = wasFirst
			} else {
				go l.reensure()
			}
		case <-l.recheck:
			stopSync()
			attempt()
		case <-l.halt:
			if l.unpaired() {
				stopSync()
			}
		case err := <-syncDone:
			cancelSync, syncDone = nil, nil
			l.log.Info("sync stopped", "err", err)
			if !errors.Is(err, errHalted) {
				attempt()
			}
		case <-retry:
			attempt()
		}
	}
}

func (l *Listener) unpaired() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.phase == wire.StateUnpaired
}

// reload runs the reload op (spec 5.1) and answers once the standing token works.
func (l *Listener) reload() wire.ReloadResponse {
	reply := make(chan error, 1)
	select {
	case l.reloadCh <- reply:
	case <-l.bg.Done():
		return wire.ReloadResponse{V: wire.ProtocolVersion, Err: wire.IPCErrInternal, Detail: "the listener is stopping"}
	}
	var err error
	select {
	case err = <-reply:
	case <-l.bg.Done():
		select {
		case err = <-reply:
		default:
			return wire.ReloadResponse{V: wire.ProtocolVersion, Err: wire.IPCErrInternal, Detail: "the listener is stopping"}
		}
	}
	var up *errUnpaired
	switch {
	case err == nil:
		return wire.ReloadResponse{V: wire.ProtocolVersion, OK: true}
	case errors.As(err, &up):
		return wire.ReloadResponse{V: wire.ProtocolVersion, Err: wire.IPCErrUnpaired, Detail: up.reload}
	case errors.Is(err, errKeyring):
		return wire.ReloadResponse{V: wire.ProtocolVersion, Err: wire.IPCErrInternal, Detail: textKeyring}
	case errors.Is(err, ErrConfigNewer):
		return wire.ReloadResponse{V: wire.ProtocolVersion, Err: wire.IPCErrInternal, Detail: err.Error()}
	}
	return wire.ReloadResponse{V: wire.ProtocolVersion, Err: wire.IPCErrInternal, Detail: describe(err)}
}
