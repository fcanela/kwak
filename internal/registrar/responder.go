package registrar

import (
	"context"
	"errors"
	"net/http"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
)

// The responder account of a pairing (decision 46, spec 3.12): agent.<owner>.<pair>.responder, the
// pairing's listener account name with "responder" in place of "listener". The listener asks for
// it with POST /responder when it starts a responder run. Every responder run of that machine
// posts with it. Its record has Kind responder, the pairing's id and no session, so the idle job,
// POST /agents/close and POST /agents/alive leave it alone; a revoke closes it like any account
// of the pairing.

func (s *Server) handleResponder(w http.ResponseWriter, r *http.Request, c caller) {
	var req wire.Empty
	if !decodeBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	out, e := s.responder(ctx, c)
	if e != nil {
		writeErr(w, e)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// responder makes the pairing's responder account, registered, named and joined to the room of
// the pairing's kind, or gives it a new token, which logs the old one out (one live token, as for
// an agent). A closed record is opened again only for its own live pairing. It runs under the
// owner's standing lock, because the name comes from the pairing's listener account
// (ensureListener), and under the account's own lock, which a close also takes.
func (s *Server) responder(ctx context.Context, c caller) (wire.Agent, *wire.APIError) {
	unlock := s.lock("standing|" + c.owner)
	defer unlock()
	listener, e := s.ensureListener(ctx, c.owner, c.pairing)
	if e != nil {
		return wire.Agent{}, e
	}
	lp := wire.ResponderOf(listener)
	if lp == "" {
		return wire.Agent{}, internalErr
	}
	unlockLP := s.lock("lp|" + lp)
	defer unlockLP()
	room, e := s.agentsRoom(ctx, c.test)
	switch {
	case e != nil:
		return wire.Agent{}, e
	case room == "":
		return wire.Agent{}, newErr(wire.ErrHomeserver, matrix.ErrNotFound)
	}
	now := s.now()
	var rec Agent
	err := s.Store.Update(func(st *State) error {
		if e = st.pairingErr(c.pairing, now); e != nil {
			return nil
		}
		a := st.Agents[lp]
		switch {
		case a == nil:
			a = &Agent{Owner: c.owner, Kind: wire.KindResponder, Pairing: c.pairing, Created: now, LastActive: now, PendingSince: &now}
			st.putAgent(lp, a)
		case a.Pairing != c.pairing || a.Kind != wire.KindResponder:
			e = newErr(wire.ErrNotYours, "the responder account belongs to another pairing")
			return nil
		}
		rec = *a
		return nil
	})
	switch {
	case err != nil:
		return wire.Agent{}, internalErr
	case e != nil:
		return wire.Agent{}, e
	}
	resp := wire.Agent{DeviceID: agentDevice, RoomID: room}
	var l matrix.Login
	if rec.Open() {
		if l, err = s.remint(ctx, lp); err != nil {
			return wire.Agent{}, newErr(wire.ErrHomeserver, hsErrcode(err))
		}
	} else {
		// New, left pending by a registrar that stopped, or closed (a failed setup). A name
		// tuwunel already has is this owner's own: the name is derived, not picked.
		resp.New, resp.Reopened = rec.PendingSince != nil, rec.Closed != nil
		if l, e = s.finishPending(ctx, lp, room); e != nil {
			return wire.Agent{}, e
		}
	}
	var block *wire.APIError
	err = s.Store.Update(func(st *State) error {
		a := st.Agents[lp]
		if a == nil || a.Pairing != c.pairing {
			return errors.New("responder record gone")
		}
		if block, _ = st.mintBlock(a, c.pairing, s.now()); block != nil {
			// Revoked meanwhile: the revoke's close list may not hold this account yet.
			t := s.now()
			a.Closed, a.CloseReason, a.CleanupDone, a.PendingSince = &t, "revoked", false, nil
			return nil
		}
		a.PendingSince, a.Closed, a.CloseReason, a.CleanupDone = nil, nil, "", false
		a.LastActive, a.MintedIP, a.MintedAt = now, c.ip, now
		return nil
	})
	if err != nil {
		s.Log.Error("responder record", "account", lp, "err", err)
		return wire.Agent{}, internalErr
	}
	if block != nil {
		s.cleanup(ctx, lp)
		return wire.Agent{}, block
	}
	resp.UserID, resp.AccessToken = s.Cfg.mxid(lp), l.AccessToken
	s.Log.Info("responder token", "account", lp, "pairing", c.pairing, "new", resp.New, "reopened", resp.Reopened)
	return resp, nil
}
