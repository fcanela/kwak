// Package registrar is the kwak appservice: the owner list, pairings, owner keys and agent
// accounts, the @registrar bot, and the listener API (spec section 3).
package registrar

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/render"
)

// Server is the registrar. Set the fields, serve API and Appservice, and run RunJobs, which
// first calls Start.
type Server struct {
	Cfg   Config
	Store *Store
	HS    Homeserver
	Clock Clock
	Rand  io.Reader // crypto/rand.Reader in production
	Log   *slog.Logger

	once     sync.Once
	pmu      sync.Mutex
	pending  map[string]*pendingPairing // normalised code -> pairing
	claims   map[string]*pendingPairing // sha256 of the claim -> pairing
	order    []*pendingPairing
	cmu      sync.Mutex
	cache    map[string]cachedAgent
	lmu      sync.Mutex
	locks    map[string]*keyLock
	tmu      sync.Mutex
	txns     map[string]bool
	txnOrder []string
	smu      sync.Mutex
	side     map[string]*sideRoom // side rooms by config key, ids in memory only (rooms.go)
}

const (
	maxPending   = 1000
	maxTxns      = 1024
	requestTTL   = 10 * time.Minute
	remintWindow = 10 * time.Minute
	cleanupEvery = 5 * time.Minute
	flushEvery   = 60 * time.Second
	roomRetry    = 5 * time.Second
	callTimeout  = 20 * time.Second
	agentDevice  = "KWAK"
)

func (s *Server) init() {
	s.once.Do(func() {
		s.pending = map[string]*pendingPairing{}
		s.claims = map[string]*pendingPairing{}
		s.cache = map[string]cachedAgent{}
		s.locks = map[string]*keyLock{}
		s.txns = map[string]bool{}
		if s.Log == nil {
			s.Log = slog.New(slog.DiscardHandler)
		}
		if s.Clock == nil {
			s.Clock = SystemClock{}
		}
		s.initSide()
	})
}

func (s *Server) now() time.Time { return s.Clock.Now().UTC() }

type keyLock struct {
	mu   sync.Mutex
	refs int
}

// lock serialises everything done for one session key (spec 3.4 step 0).
func (s *Server) lock(key string) func() {
	s.lmu.Lock()
	l := s.locks[key]
	if l == nil {
		l = &keyLock{}
		s.locks[key] = l
	}
	l.refs++
	s.lmu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.lmu.Lock()
		if l.refs--; l.refs == 0 {
			delete(s.locks, key)
		}
		s.lmu.Unlock()
	}
}

func (s *Server) random(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(s.Rand, b); err != nil {
		return nil, fmt.Errorf("registrar: random: %w", err)
	}
	return b, nil
}

func (s *Server) randomHex(n int) (string, error) {
	b, err := s.random(n)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) randomB64(n int) (string, error) {
	b, err := s.random(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// notice posts text in a room. A failure is logged, never returned: a notice never blocks a reply.
func (s *Server) notice(ctx context.Context, roomID, text string) {
	if roomID == "" {
		return
	}
	txn, err := s.randomHex(12)
	if err != nil {
		s.Log.Error("notice", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
	defer cancel()
	if err := s.HS.Notice(ctx, roomID, "kwak-"+txn, text); err != nil {
		s.Log.Warn("notice failed", "room", roomID, "err", err)
	}
}

// ownerDM returns the recorded DM of an owner's human.
func (s *Server) ownerDM(owner string) string {
	var room string
	s.Store.View(func(st *State) {
		if o := st.Owners[owner]; o != nil {
			room = st.DMs[o.Human]
		}
	})
	return room
}

// clientIP is the last X-Forwarded-For entry when it parses as an IP (the proxy sets it), else the
// peer address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		parts := strings.Split(xff[len(xff)-1], ",")
		if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return "unknown"
}

// sanitizeLabel cleans a self-reported label before the bot echoes it: render.Sanitize (spec 6.1
// rule 4), every run of white space as one space, at most 64 runes.
func sanitizeLabel(s string) string {
	r := []rune(strings.Join(strings.Fields(render.Sanitize(s)), " "))
	return strings.TrimSpace(string(r[:min(len(r), 64)]))
}

// hsErrcode is the tuwunel errcode of err for a 502 body.
func hsErrcode(err error) string {
	var me *matrix.Error
	if errors.As(err, &me) && me.Code != "" {
		return me.Code
	}
	if errors.As(err, &me) {
		return fmt.Sprintf("HTTP %d", me.Status)
	}
	return "M_UNKNOWN"
}

// Start resolves the agents room, retrying every 5 s until it exists, joins it as @registrar,
// stores its id, tries each configured side room once without waiting for it, and opens a DM for
// each admin who has none (spec 3.5, 3.11). RunJobs keeps trying side rooms that are not there.
func (s *Server) Start(ctx context.Context) error {
	s.init()
	for {
		err := s.joinRoom(ctx)
		if err == nil {
			break
		}
		s.Log.Warn("agents room not ready, retrying", "room", s.Cfg.Room, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(roomRetry):
		}
	}
	s.joinSideRooms(ctx, true)
	s.adminDMs(ctx)
	return nil
}

func (s *Server) joinRoom(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	id, err := s.HS.ResolveAlias(cctx, s.Cfg.Room)
	if err != nil {
		return err
	}
	if err := s.HS.JoinAs(cctx, "", id); err != nil {
		return err
	}
	return s.Store.Update(func(st *State) error {
		if st.RoomID != id && st.RoomID != "" {
			s.Log.Warn("agents room changed", "old", st.RoomID, "new", id)
		}
		st.RoomID = id
		return nil
	})
}

func (s *Server) adminDMs(ctx context.Context) {
	for _, a := range s.Cfg.Admins {
		var has bool
		s.Store.View(func(st *State) { _, has = st.DMs[a] })
		if has {
			continue
		}
		if _, err := s.openDM(ctx, a, ""); err != nil {
			s.Log.Warn("admin DM", "admin", a, "err", err)
		}
	}
}

// openDM creates a new DM with human, records it, and leaves old if it is set.
func (s *Server) openDM(ctx context.Context, human, old string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	room, err := s.HS.CreateDM(cctx, human)
	if err != nil {
		return "", err
	}
	if err := s.Store.Update(func(st *State) error {
		st.DMs[human] = room
		delete(st.DMInvited, human)
		return nil
	}); err != nil {
		return "", err
	}
	if old != "" && old != room {
		if err := s.HS.LeaveAs(cctx, "", old); err != nil {
			s.Log.Warn("leave old DM", "room", old, "err", err)
		}
	}
	return room, nil
}

func (s *Server) roomID() string {
	var id string
	s.Store.View(func(st *State) { id = st.RoomID })
	return id
}
