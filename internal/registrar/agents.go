package registrar

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
)

type cachedAgent struct {
	resp wire.Agent
	at   time.Time
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

func lockKey(a *Agent, lp string) string {
	switch {
	case a.Kind == wire.KindStanding:
		return "standing|" + a.Owner
	case a.Session == "":
		return "lp|" + lp
	}
	return "agent|" + sessionKey{a.Owner, a.Harness, a.Session, a.Helper}.String()
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request, c caller) {
	var req wire.AgentRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !isHex32(req.RequestID) {
		writeErr(w, newErr(wire.ErrBadRequest, "request_id must be 32 hex digits"))
		return
	}
	if !wire.ValidHarness(req.Harness) {
		writeErr(w, newErr(wire.ErrBadRequest, fmt.Sprintf("unknown harness %q", req.Harness)))
		return
	}
	hex, err := wire.SessionHex(req.Session)
	if err != nil {
		writeErr(w, newErr(wire.ErrBadSession, "session id is not a UUID"))
		return
	}
	if req.Helper != "" && !wire.ValidHelper(req.Helper) { // "listener" is reserved (decision 42)
		writeErr(w, newErr(wire.ErrBadRequest, "bad helper name"))
		return
	}
	key := sessionKey{c.owner, req.Harness, hex, req.Helper}
	unlock := s.lock("agent|" + key.String())
	defer unlock()
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	resp, e := s.ensureAgent(ctx, c, key, req.RequestID)
	if e != nil {
		writeErr(w, e)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ensureAgent is POST /agents after validation, under the session's lock (spec 3.4).
func (s *Server) ensureAgent(ctx context.Context, c caller, key sessionKey, requestID string) (wire.Agent, *wire.APIError) {
	now := s.now()
	var parent, lp string
	var rec *Agent
	// A test pairing's agents live in the test room, never in the agents room (decision 39).
	room, e := s.agentsRoom(ctx, c.test)
	if e != nil {
		return wire.Agent{}, e
	}
	s.Store.View(func(st *State) {
		if e = st.pairingErr(c.pairing, now); e != nil {
			return
		}
		if key.helper != "" {
			plp, pa := st.lookup(sessionKey{key.owner, key.harness, key.hex, ""})
			switch {
			case pa == nil:
				e = newErr(wire.ErrParentClosed, "parent agent does not exist")
			case pa.Pairing != c.pairing:
				e = newErr(wire.ErrNotYours, "parent agent belongs to another pairing")
			case !pa.Open():
				e = newErr(wire.ErrParentClosed, "parent agent is closed")
			}
			parent = plp
		}
		if l, a := st.lookup(key); a != nil {
			cp := *a
			lp, rec = l, &cp
		}
	})
	if e != nil {
		return wire.Agent{}, e
	}
	ck := c.pairing + "|" + requestID + "|" + key.String()
	s.cmu.Lock()
	hit, ok := s.cache[ck]
	s.cmu.Unlock()
	if ok && now.Sub(hit.at) < requestTTL {
		return hit.resp, nil
	}
	if room == "" {
		return wire.Agent{}, newErr(wire.ErrHomeserver, matrix.ErrNotFound)
	}
	if rec != nil && rec.Pairing != c.pairing {
		return wire.Agent{}, newErr(wire.ErrNotYours, "agent belongs to another pairing")
	}

	var l matrix.Login
	resp := wire.Agent{DeviceID: agentDevice, RoomID: room}
	var err error
	switch {
	case rec == nil:
		lp, l, e = s.createAgent(ctx, c, key, parent, room)
		resp.New = true
	case rec.PendingSince != nil:
		l, e = s.finishPending(ctx, lp, room)
		resp.New = true
	case rec.Closed == nil:
		l, err = s.remint(ctx, lp)
		if err != nil {
			e = newErr(wire.ErrHomeserver, hsErrcode(err))
		}
		if e == nil && !rec.MintedAt.IsZero() && now.Sub(rec.MintedAt) < remintWindow && rec.MintedIP != c.ip {
			s.notice(ctx, s.ownerDM(c.owner), fmt.Sprintf(
				"Agent %s of %s (%s) got a new token from %s, %s after one from %s. If that is not you, send: revoke %s",
				lp, c.machine, c.pairing, c.ip, now.Sub(rec.MintedAt).Round(time.Second), rec.MintedIP, c.pairing))
		}
	default:
		l, e = s.reopen(ctx, lp, room)
		resp.Reopened = true
	}
	if e != nil {
		return wire.Agent{}, e
	}
	var block *wire.APIError
	var why string
	undo := false
	err = s.Store.Update(func(st *State) error {
		a := st.Agents[lp]
		if a == nil {
			return errors.New("agent record gone")
		}
		if block, why = st.mintBlock(a, c.pairing, s.now()); block != nil {
			// An open record a confirmed re-pair moved keeps its tokens (I9): leave it.
			if a.Pairing == c.pairing || !a.Open() {
				t := s.now()
				if a.Closed == nil {
					a.Closed = &t
				}
				a.CloseReason, a.CleanupDone, a.PendingSince = why, false, nil
				undo = true
			}
			return nil
		}
		if resp.New || resp.Reopened {
			a.PendingSince, a.Closed, a.CloseReason, a.CleanupDone = nil, nil, "", false
		} else if a.Closed != nil {
			return errors.New("agent closed meanwhile")
		}
		a.LastActive, a.MintedIP, a.MintedAt = now, c.ip, now
		return nil
	})
	if err != nil {
		s.Log.Error("agent record", "agent", lp, "err", err)
		return wire.Agent{}, newErr(wire.ErrHomeserver, "M_UNKNOWN")
	}
	if block != nil {
		if undo {
			s.Log.Info("agent closed", "agent", lp, "reason", why)
			s.cleanup(ctx, lp)
		}
		return wire.Agent{}, block
	}
	resp.UserID, resp.AccessToken = s.Cfg.mxid(lp), l.AccessToken
	s.cmu.Lock()
	for k, v := range s.cache {
		if now.Sub(v.at) >= requestTTL {
			delete(s.cache, k)
		}
	}
	s.cache[ck] = cachedAgent{resp: resp, at: now}
	s.cmu.Unlock()
	s.Log.Info("agent token", "agent", lp, "pairing", c.pairing, "new", resp.New, "reopened", resp.Reopened)
	return resp, nil
}

// mintBlock is checked in the last Update of POST /agents. A revoke, owner removal or parent close
// that landed while the token was minted wins and the agent is closed again (I7, I9). It returns
// the error to answer and the close reason.
func (st *State) mintBlock(a *Agent, pairing string, now time.Time) (*wire.APIError, string) {
	if e := st.pairingErr(pairing, now); e != nil {
		if e.Errcode == wire.ErrOwnerRemoved {
			return e, "owner_removed"
		}
		return e, "revoked"
	}
	if a.Parent != "" {
		if p := st.Agents[a.Parent]; p == nil || !p.Open() || p.Pairing != pairing {
			return newErr(wire.ErrParentClosed, "parent agent is closed"), "parent_closed"
		}
	}
	return nil, ""
}

// createAgent walks the name ladder (spec 3.4 step 4). Each record is written pending before
// /register, so a name taken in tuwunel but absent from the file is always a clash.
func (s *Server) createAgent(ctx context.Context, c caller, key sessionKey, parent, room string) (string, matrix.Login, *wire.APIError) {
	var cands []string
	kind := wire.KindSession
	if key.helper != "" {
		kind = wire.KindHelper
		hl, err := wire.HelperLocalpart(parent, key.helper)
		if err != nil {
			return "", matrix.Login{}, newErr(wire.ErrBadRequest, "bad helper name")
		}
		cands = []string{hl}
	} else {
		cands = wire.Candidates(key.owner, key.harness, key.hex)
	}
	for _, lp := range cands {
		now := s.now()
		taken := false
		err := s.Store.Update(func(st *State) error {
			if st.Agents[lp] != nil {
				taken = true
				return nil
			}
			st.putAgent(lp, &Agent{
				Owner: key.owner, Kind: kind, Pairing: c.pairing, Harness: key.harness, Session: key.hex,
				Helper: key.helper, Parent: parent, Created: now, LastActive: now, PendingSince: &now,
			})
			return nil
		})
		if err != nil {
			return "", matrix.Login{}, newErr(wire.ErrHomeserver, "M_UNKNOWN")
		}
		if taken {
			continue
		}
		l, err := s.HS.Register(ctx, lp, agentDevice)
		if matrix.IsCode(err, matrix.ErrUserInUse) {
			s.Log.Warn("name clash", "agent", lp)
			if err := s.Store.Update(func(st *State) error {
				t := s.now()
				st.Agents[lp] = &Agent{Owner: key.owner, Kind: kind, Created: t, LastActive: t,
					Closed: &t, CloseReason: "clash", CleanupDone: true}
				return nil
			}); err != nil {
				return "", matrix.Login{}, newErr(wire.ErrHomeserver, "M_UNKNOWN")
			}
			continue
		}
		if err != nil {
			return "", matrix.Login{}, newErr(wire.ErrHomeserver, hsErrcode(err))
		}
		return lp, l, s.setup(ctx, lp, room)
	}
	return "", matrix.Login{}, newErr(wire.ErrNamesExhausted, "all candidate names are taken")
}

// finishPending completes a record left pending by a registrar that stopped mid-create.
func (s *Server) finishPending(ctx context.Context, lp, room string) (matrix.Login, *wire.APIError) {
	l, err := s.HS.Register(ctx, lp, agentDevice)
	if matrix.IsCode(err, matrix.ErrUserInUse) {
		l, err = s.remint(ctx, lp)
	}
	if err != nil {
		return l, newErr(wire.ErrHomeserver, hsErrcode(err))
	}
	return l, s.setup(ctx, lp, room)
}

// remint logs every device of the agent out, then logs in once: one live token per agent.
func (s *Server) remint(ctx context.Context, lp string) (matrix.Login, error) {
	uid := s.Cfg.mxid(lp)
	if err := s.HS.LogoutAll(ctx, uid); err != nil {
		return matrix.Login{}, err
	}
	return s.HS.Login(ctx, uid, agentDevice)
}

// reopen is finishPending: a record closed before its /register finished has no account yet.
func (s *Server) reopen(ctx context.Context, lp, room string) (matrix.Login, *wire.APIError) {
	return s.finishPending(ctx, lp, room)
}

// setup resets the display name (tuwunel adds a suffix at register) and joins the room of the
// pairing's kind. On a failure the agent is closed again (spec 3.4 step 5).
func (s *Server) setup(ctx context.Context, lp, room string) *wire.APIError {
	uid := s.Cfg.mxid(lp)
	err := s.HS.SetDisplayName(ctx, uid, lp)
	if err == nil {
		err = s.joinOrInvite(ctx, uid, room)
	}
	if err == nil {
		return nil
	}
	s.Log.Warn("agent setup failed", "agent", lp, "err", err)
	s.closeAgents(context.WithoutCancel(ctx), []string{lp}, "setup_failed", lp, nil)
	return newErr(wire.ErrHomeserver, hsErrcode(err))
}

// closeAgents closes each agent and its helpers (spec 3.9): state first, then leave and
// logout, both under the agent's lock so a reopen never interleaves with them. held is a localpart
// whose lock the caller holds. still, when set, is checked again under the lock for each agent of
// lps; one it rejects is skipped. It returns every localpart now closed.
func (s *Server) closeAgents(ctx context.Context, lps []string, reason, held string, still func(*Agent) bool) ([]string, error) {
	asked := map[string]bool{}
	for _, lp := range lps {
		asked[lp] = true
	}
	var out []string
	seen := map[string]bool{}
	queue := append([]string(nil), lps...)
	for len(queue) > 0 {
		lp := queue[0]
		queue = queue[1:]
		if seen[lp] {
			continue
		}
		seen[lp] = true
		var lk string
		s.Store.View(func(st *State) {
			if a := st.Agents[lp]; a != nil {
				lk = lockKey(a, lp)
			}
		})
		if lk == "" {
			continue
		}
		unlock := func() {}
		if lp != held {
			unlock = s.lock(lk)
		}
		now := s.now()
		changed, skip := false, false
		err := s.Store.Update(func(st *State) error {
			a := st.Agents[lp]
			if a == nil {
				return nil
			}
			if still != nil && asked[lp] && a.Closed == nil && !still(a) {
				skip = true
				return nil
			}
			if a.Closed == nil {
				a.Closed, a.CloseReason, a.CleanupDone, a.PendingSince = &now, reason, false, nil
				changed = true
			}
			for hlp, h := range st.Agents {
				if h.Parent == lp && h.Closed == nil {
					queue = append(queue, hlp)
				}
			}
			return nil
		})
		if err != nil {
			unlock()
			return out, err
		}
		if skip {
			unlock()
			continue
		}
		out = append(out, lp)
		if changed {
			s.Log.Info("agent closed", "agent", lp, "reason", reason)
			s.cleanup(ctx, lp)
		}
		unlock()
	}
	return out, nil
}

// doneErr reports whether a leave or logout error means there is nothing left to undo.
func doneErr(err error) bool {
	var me *matrix.Error
	if !errors.As(err, &me) {
		return false
	}
	switch me.Status {
	case 400, 403, 404:
		return true
	}
	return false
}

// cleanup leaves the room of the agent's kind and its report room, and logs every device out; the
// jobs retry it until all succeed. The caller holds the agent's lock.
func (s *Server) cleanup(ctx context.Context, lp string) {
	uid := s.Cfg.mxid(lp)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
	defer cancel()
	test := false
	s.Store.View(func(st *State) {
		if a := st.Agents[lp]; a != nil {
			test = st.recordTest(lp, a)
		}
	})
	err1 := s.leaveKind(ctx, uid, test)
	err2 := s.HS.LogoutAll(ctx, uid)
	if err2 != nil && doneErr(err2) {
		err2 = nil
	}
	if err1 != nil || err2 != nil {
		s.Log.Warn("cleanup failed, will retry", "agent", lp, "leave", err1, "logout", err2)
		return
	}
	s.Store.Update(func(st *State) error {
		if a := st.Agents[lp]; a != nil && a.Closed != nil {
			a.CleanupDone = true
		}
		return nil
	})
}

// leaveKind leaves the room an account of this kind lives in, then the report room of its kind:
// the listener leaves that one itself after a report, but its leave may have failed. The room of
// the kind comes first and never waits for the report room, so a closed agent always leaves the
// room whose member list tells askers it is gone (decision 6). A room of the kind that is not
// joined yet is an error, so the jobs try again. A report room that is not configured, or not
// joined yet, is skipped: an account can be in it only through a join that needed its id. A leave
// of a room the account is not in counts as done (doneErr).
func (s *Server) leaveKind(ctx context.Context, uid string, test bool) error {
	main, e := s.agentsRoom(ctx, test)
	switch {
	case e != nil: // a test record on a server whose config no longer has a test room
	case main == "":
		return errors.New("room not joined yet")
	default:
		if err := s.HS.LeaveAs(ctx, uid, main); err != nil && !doneErr(err) {
			return err
		}
	}
	if id, _ := s.sideID(ctx, reportKey(test)); id != "" {
		if err := s.HS.LeaveAs(ctx, uid, id); err != nil && !doneErr(err) {
			return err
		}
	}
	return nil
}

// revoke revokes a pairing (spec 3.9): state, the standing device, then its agents (I9).
func (s *Server) revoke(ctx context.Context, id, reason string) error {
	now := s.now()
	var owner string
	var lps []string
	err := s.Store.Update(func(st *State) error {
		p := st.Pairings[id]
		if p == nil {
			return fmt.Errorf("no pairing %s", id)
		}
		owner = p.Owner
		if p.Revoked == nil {
			p.Revoked, p.RevokeReason, p.LogoutDone = &now, reason, false
		}
		// Its agents, helpers and its own listener account (decision 42). A listener account a
		// re-pair moved away has the new pairing's id and stays.
		for lp, a := range st.Agents {
			if a.Pairing == id && a.Closed == nil {
				lps = append(lps, lp)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.Log.Info("pairing revoked", "pairing", id, "reason", reason)
	s.logoutStandingDevice(ctx, owner, id)
	sort.Strings(lps)
	_, err = s.closeAgents(ctx, lps, "revoked", "", nil)
	return err
}

// logoutStandingDevice takes the lock of POST /standing, so a login there either sees the revoke
// or comes before this logout. The device KWAK-<id> belongs to the pairing's listener account
// (which a re-pair may have moved to the new pairing). A pairing with no listener name never called
// POST /standing on a build with decision 42: its device, if any, is on an old per-owner standing
// account, which the job pass closes with every device (closeLegacy), so there is nothing to log
// out here. The logout is done only when it worked; the jobs retry it otherwise.
func (s *Server) logoutStandingDevice(ctx context.Context, owner, id string) {
	unlock := s.lock("standing|" + owner)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
	defer cancel()
	var lp string
	s.Store.View(func(st *State) {
		if p := st.Pairings[id]; p != nil {
			lp = p.Listener
		}
	})
	if lp != "" {
		err := s.HS.LogoutDevice(ctx, s.Cfg.mxid(lp), "KWAK-"+id)
		if err != nil && !doneErr(err) {
			s.Log.Warn("standing device logout failed, will retry", "pairing", id, "account", lp, "err", err)
			return
		}
	}
	s.Store.Update(func(st *State) error {
		if p := st.Pairings[id]; p != nil {
			p.LogoutDone = true
		}
		return nil
	})
}
