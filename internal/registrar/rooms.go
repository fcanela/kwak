package registrar

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
)

// Side rooms (decisions 39 and 40, spec 3.11): the report room, the test room and the test report
// room. Each is optional in config.json. Their ids live in memory only, resolved from the aliases
// at every start, so state.json keeps its shape and an older image can still read it.
const (
	sideReport     = "report_room"
	sideTest       = "test_room"
	sideTestReport = "test_report_room"
)

// sideRetryMax caps the wait between two tries of a side room that is not there yet.
const sideRetryMax = 5 * time.Minute

type sideRoom struct {
	alias string
	id    string
	next  time.Time     // the next background try, after a failure
	delay time.Duration // the wait after the next failure
}

// initSide fills s.side from the config. Caller holds s.once.
func (s *Server) initSide() {
	s.side = map[string]*sideRoom{}
	for key, alias := range map[string]string{
		sideReport: s.Cfg.ReportRoom, sideTest: s.Cfg.TestRoom, sideTestReport: s.Cfg.TestReportRoom,
	} {
		if alias != "" {
			s.side[key] = &sideRoom{alias: alias, delay: roomRetry}
		}
	}
}

// sideID returns the id of a side room, and whether it is configured at all. With ctx set, a room
// that is configured but not joined yet is tried once now, whatever its retry wait.
func (s *Server) sideID(ctx context.Context, key string) (string, bool) {
	s.smu.Lock()
	r := s.side[key]
	var id string
	if r != nil {
		id = r.id
	}
	s.smu.Unlock()
	if r == nil {
		return "", false
	}
	if id == "" && ctx != nil {
		s.joinSide(ctx, key, true)
		s.smu.Lock()
		id = r.id
		s.smu.Unlock()
	}
	return id, true
}

// joinSideRooms tries every configured side room that is not joined yet. A failure is logged and
// tried again later, with a wait that doubles up to sideRetryMax; it never stops the registrar.
// With force, the waits are ignored.
func (s *Server) joinSideRooms(ctx context.Context, force bool) {
	s.smu.Lock()
	keys := make([]string, 0, len(s.side))
	for k := range s.side {
		keys = append(keys, k)
	}
	s.smu.Unlock()
	sort.Strings(keys)
	for _, k := range keys {
		s.joinSide(ctx, k, force)
	}
}

func (s *Server) joinSide(ctx context.Context, key string, force bool) {
	now := s.now()
	s.smu.Lock()
	r := s.side[key]
	if r == nil || r.id != "" || (!force && now.Before(r.next)) {
		s.smu.Unlock()
		return
	}
	alias := r.alias
	s.smu.Unlock()
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
	defer cancel()
	id, err := s.HS.ResolveAlias(cctx, alias)
	if err == nil {
		err = s.HS.JoinAs(cctx, "", id)
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	if err != nil {
		r.next = now.Add(r.delay)
		s.Log.Warn("side room not ready, will retry", "room", alias, "in", r.delay.String(), "err", err)
		r.delay = min(2*r.delay, sideRetryMax)
		return
	}
	r.id, r.delay = id, roomRetry
	s.Log.Info("side room joined", "room", alias, "id", id)
}

// knownRoom reports whether room is the agents room or a joined side room.
func (s *Server) knownRoom(room string) bool {
	if room == s.roomID() {
		return true
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	for _, r := range s.side {
		if r.id == room {
			return true
		}
	}
	return false
}

// agentsRoom returns the room the agents of a pairing kind live in: the agents room for a normal
// pairing, the test room for a test pairing (decision 39). "" means it is not joined yet; e is set
// when a test pairing meets a server with no test room.
func (s *Server) agentsRoom(ctx context.Context, test bool) (string, *wire.APIError) {
	if !test {
		return s.roomID(), nil
	}
	id, ok := s.sideID(ctx, sideTest)
	if !ok {
		// A test pairing left over after test_room was taken out of config.json: it gets no room,
		// and never the agents room.
		s.Log.Warn("test pairing, but no test_room in the config")
		return "", newErr(wire.ErrHomeserver, matrix.ErrNotFound)
	}
	return id, nil
}

// cachedAgentsRoom is agentsRoom with no homeserver call: "" when the room is not joined yet or
// not configured.
func (s *Server) cachedAgentsRoom(test bool) string {
	if !test {
		return s.roomID()
	}
	id, _ := s.sideID(nil, sideTest)
	return id
}

// reportKey is the side room a pairing kind reports to.
func reportKey(test bool) string {
	if test {
		return sideTestReport
	}
	return sideReport
}

// recordTest reports whether an agent record belongs to the test side: the test standing account,
// or an agent of a test pairing. Caller is inside View or Update.
func (st *State) recordTest(lp string, a *Agent) bool {
	if a.Kind == wire.KindStanding {
		n, err := wire.ParseLocalpart(lp)
		return err == nil && n.Test
	}
	p := st.Pairings[a.Pairing]
	return p != nil && p.Test
}

// joinOrInvite joins uid to room with the as_token. When the room refuses (an invite-only room),
// @registrar invites uid first and the join is tried once more. @registrar must be a member of
// the room with invite power (spec 3.11).
func (s *Server) joinOrInvite(ctx context.Context, uid, room string) error {
	err := s.HS.JoinAs(ctx, uid, room)
	var me *matrix.Error
	if err == nil || !errors.As(err, &me) || me.Status != http.StatusForbidden {
		return err
	}
	if err := s.HS.Invite(ctx, room, uid); err != nil {
		return err
	}
	return s.HS.JoinAs(ctx, uid, room)
}

var errNotYoursReport = newErr(wire.ErrNotYours, "not an open account of this pairing")

// handleReportJoin is POST /report/join (spec 3.4): it joins one of the caller's own accounts to
// the report room of the caller's pairing kind, so the listener can post one report line with that
// account's token; an agent leaves again, the standing account stays (decisions 33, 39 and 40,
// spec DV-20).
func (s *Server) handleReportJoin(w http.ResponseWriter, r *http.Request, c caller) {
	var req wire.ReportJoinRequest
	if !decodeBody(w, r, &req) {
		return
	}
	lp := s.agentLocalpart(req.UserID)
	mine := func(st *State) (string, *wire.APIError) {
		if e := st.pairingErr(c.pairing, s.now()); e != nil {
			return "", e
		}
		a := st.Agents[lp]
		switch {
		case a == nil:
			return "", errNotYoursReport
		case a.Kind == wire.KindStanding:
			// Only the caller pairing's own listener account (decision 42): never another
			// pairing's, and never the owner's old per-owner standing account.
			if p := st.Pairings[c.pairing]; p == nil || lp != p.Listener || a.Pairing != c.pairing || !a.Open() {
				return "", errNotYoursReport
			}
		case a.Pairing != c.pairing || a.Session == "" || !a.Open():
			return "", errNotYoursReport
		}
		return lockKey(a, lp), nil
	}
	var lk string
	var e *wire.APIError
	s.Store.View(func(st *State) { lk, e = mine(st) })
	if e != nil {
		writeErr(w, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	room, ok := s.sideID(ctx, reportKey(c.test))
	if !ok {
		writeErr(w, newErr(wire.ErrNoReportRoom, "this server has no report room for this pairing"))
		return
	}
	if room == "" {
		writeErr(w, newErr(wire.ErrHomeserver, matrix.ErrNotFound))
		return
	}
	// Under the account's lock, so a close cannot slip between the check and the join and leave
	// a closed agent in the report room.
	unlock := s.lock(lk)
	defer unlock()
	s.Store.View(func(st *State) { _, e = mine(st) })
	if e != nil {
		writeErr(w, e)
		return
	}
	if err := s.joinOrInvite(ctx, req.UserID, room); err != nil {
		s.Log.Warn("report room join failed", "account", lp, "err", err)
		writeErr(w, newErr(wire.ErrHomeserver, hsErrcode(err)))
		return
	}
	s.Log.Info("report room joined", "account", lp, "pairing", c.pairing, "test", c.test)
	writeJSON(w, http.StatusOK, wire.ReportJoin{RoomID: room})
}
