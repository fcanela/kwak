package registrar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
)

var internalErr = &wire.APIError{Status: 500, Errcode: "M_UNKNOWN", Message: "internal error"}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, wire.APIError{Errcode: code, Message: msg})
}

func writeErr(w http.ResponseWriter, e *wire.APIError) {
	status := e.Status
	if status == 0 {
		status = wire.StatusFor(e.Errcode)
	}
	apiError(w, status, e.Errcode, e.Message)
}

func newErr(code, msg string) *wire.APIError {
	return &wire.APIError{Status: wire.StatusFor(code), Errcode: code, Message: msg}
}

func bearer(r *http.Request) (string, bool) {
	v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

// decodeBody reads a strict API body; an absent body counts as {}. It answers 400 itself.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, wire.MaxBody))
	if err != nil {
		apiError(w, 400, wire.ErrBadRequest, fmt.Sprintf("body over %d bytes", wire.MaxBody))
		return false
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		data = []byte("{}")
	}
	if err := wire.DecodeAPI(data, v); err != nil {
		msg := strings.TrimPrefix(err.Error(), wire.ErrBadMessage.Error()+": ")
		apiError(w, 400, wire.ErrBadRequest, msg)
		return false
	}
	return true
}

// API is the listener API, /_kwak/v1/... (spec 3.4).
func (s *Server) API() http.Handler {
	s.init()
	mux := http.NewServeMux()
	p := wire.APIPrefix
	mux.HandleFunc("POST "+p+wire.PathPair, s.handlePair)
	mux.HandleFunc("POST "+p+wire.PathPairClaim, s.handleClaim)
	mux.HandleFunc("POST "+p+wire.PathStanding, s.authed(s.handleStanding, false))
	mux.HandleFunc("POST "+p+wire.PathAgents, s.authed(s.handleAgents, true))
	mux.HandleFunc("POST "+p+wire.PathAgentsClose, s.authed(s.handleClose, true))
	mux.HandleFunc("POST "+p+wire.PathAgentsAlive, s.authed(s.handleAlive, true))
	mux.HandleFunc("POST "+p+wire.PathUnpair, s.authed(s.handleUnpair, true))
	mux.HandleFunc("GET "+p+wire.PathMe, s.authed(s.handleMe, true))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apiError(w, 404, "M_UNRECOGNIZED", "unknown route")
	})
	return mux
}

// caller is the authenticated pairing of a request.
type caller struct {
	pairing string
	owner   string
	machine string
	ip      string
}

type authedFunc func(w http.ResponseWriter, r *http.Request, c caller)

// authed checks the owner key, confirms an unconfirmed key (moving a replaced pairing's agents),
// and records a new client IP with a notice (spec 3.4). With confirm false the handler confirms
// the key itself, once it succeeded (POST /standing).
func (s *Server) authed(h authedFunc, confirm bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := bearer(r)
		if !ok {
			writeErr(w, newErr(wire.ErrBadKey, "missing key"))
			return
		}
		c, e := s.authenticate(r.Context(), sha(key), clientIP(r), confirm)
		if e != nil {
			writeErr(w, e)
			return
		}
		h(w, r, c)
	}
}

var errBadKey = newErr(wire.ErrBadKey, "unknown, expired or revoked key")

// pairingErr says why pairing id may no longer act, or nil. Handlers check it again under their
// lock, because a revoke can land after authenticate (I9).
func (st *State) pairingErr(id string, now time.Time) *wire.APIError {
	p := st.Pairings[id]
	if p == nil {
		return errBadKey
	}
	if o := st.Owners[p.Owner]; o == nil || o.Removed != nil {
		return newErr(wire.ErrOwnerRemoved, "owner removed")
	}
	if p.Revoked != nil || !now.Before(p.Expires) {
		return errBadKey
	}
	return nil
}

func (s *Server) authenticate(ctx context.Context, hash, ip string, confirm bool) (caller, *wire.APIError) {
	now := s.now()
	var c caller
	var e *wire.APIError
	var write, newIP bool
	s.Store.View(func(st *State) {
		id, p := st.pairingByKey(hash)
		if p == nil {
			e = errBadKey
			return
		}
		if e = st.pairingErr(id, now); e != nil {
			return
		}
		if p.Confirmed == nil && now.Sub(p.Created) > s.Cfg.UnconfirmedTTL.D() {
			e = errBadKey
			return
		}
		c = caller{pairing: id, owner: p.Owner, machine: p.Machine, ip: ip}
		_, seen := p.IPs[ip]
		newIP = !seen
		write = confirm && p.Confirmed == nil || newIP
	})
	if e != nil {
		return c, e
	}
	s.Store.TouchPairing(c.pairing, now)
	if !write {
		return c, nil
	}
	var replaced string
	err := s.Store.Update(func(st *State) error {
		p := st.Pairings[c.pairing]
		if p == nil || p.Revoked != nil {
			return errors.New("pairing revoked")
		}
		if newIP {
			if p.IPs == nil {
				p.IPs = map[string]time.Time{}
			}
			p.IPs[ip] = now
			p.LastIP = ip
		}
		if confirm {
			replaced = s.confirmLocked(st, c.pairing, p, now)
		}
		return nil
	})
	if err != nil {
		s.Log.Error("auth update", "pairing", c.pairing, "err", err)
		return c, errBadKey
	}
	s.replaced(ctx, c, replaced)
	if newIP {
		s.notice(ctx, s.ownerDM(c.owner), fmt.Sprintf("Key for %s (%s) used from %s. If that is not you, send: revoke %s",
			c.machine, c.pairing, ip, c.pairing))
	}
	return c, nil
}

// confirmLocked confirms an unconfirmed pairing inside the caller's Update and returns the pairing
// it replaced, or "".
func (s *Server) confirmLocked(st *State, id string, p *Pairing, now time.Time) string {
	if p.Confirmed != nil {
		return ""
	}
	p.Confirmed = &now
	return s.confirmReplace(st, id, p, now)
}

func (s *Server) replaced(ctx context.Context, c caller, old string) {
	if old != "" {
		s.Log.Info("re-pair confirmed", "pairing", c.pairing, "replaced", old)
		s.logoutStandingDevice(ctx, c.owner, old)
	}
}

// confirmReplace moves every agent record of the pairing p replaces to p and revokes the old one,
// inside the caller's Update (spec 3.2). It returns the replaced pairing id, or "".
func (s *Server) confirmReplace(st *State, id string, p *Pairing, now time.Time) string {
	r := st.Pairings[p.Replaces]
	if p.Replaces == "" || r == nil || r.Revoked != nil || r.Owner != p.Owner {
		return ""
	}
	for _, a := range st.Agents {
		if a.Pairing == p.Replaces {
			a.Pairing = id
		}
	}
	r.Revoked, r.RevokeReason, r.LogoutDone = &now, "replaced", false
	return p.Replaces
}

func (s *Server) handleStanding(w http.ResponseWriter, r *http.Request, c caller) {
	var req wire.Empty
	if !decodeBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	out, replaced, e := s.standing(ctx, c)
	if e != nil {
		writeErr(w, e)
		return
	}
	s.replaced(ctx, c, replaced)
	writeJSON(w, http.StatusOK, out)
}

// standing logs in a fresh standing device under the owner's standing lock, then confirms the key.
// The key is confirmed only once the login worked, so a failed call leaves it unconfirmed (spec 4
// step 6).
func (s *Server) standing(ctx context.Context, c caller) (wire.Standing, string, *wire.APIError) {
	uid := s.Cfg.mxid(wire.StandingLocalpart(c.owner))
	dev := "KWAK-" + c.pairing
	unlock := s.lock("standing|" + c.owner)
	defer unlock()
	var e *wire.APIError
	s.Store.View(func(st *State) { e = st.pairingErr(c.pairing, s.now()) })
	if e != nil {
		return wire.Standing{}, "", e
	}
	if err := s.HS.LogoutDevice(ctx, uid, dev); err != nil && !clientErr(err) {
		return wire.Standing{}, "", newErr(wire.ErrHomeserver, hsErrcode(err))
	}
	l, err := s.HS.Login(ctx, uid, dev)
	if err != nil {
		return wire.Standing{}, "", newErr(wire.ErrHomeserver, hsErrcode(err))
	}
	var replaced string
	revoked := false
	err = s.Store.Update(func(st *State) error {
		p := st.Pairings[c.pairing]
		if revoked = p == nil || p.Revoked != nil; revoked {
			return nil
		}
		replaced = s.confirmLocked(st, c.pairing, p, s.now())
		return nil
	})
	if err != nil || revoked {
		s.HS.LogoutDevice(context.WithoutCancel(ctx), uid, dev)
		if err != nil {
			return wire.Standing{}, "", internalErr
		}
		return wire.Standing{}, "", errBadKey
	}
	return wire.Standing{UserID: uid, AccessToken: l.AccessToken, DeviceID: dev}, replaced, nil
}

func (s *Server) handleUnpair(w http.ResponseWriter, r *http.Request, c caller) {
	var req wire.Empty
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.revoke(r.Context(), c.pairing, "unpaired"); err != nil {
		writeErr(w, internalErr)
		return
	}
	writeJSON(w, http.StatusOK, wire.Empty{})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, c caller) {
	var me wire.Me
	s.Store.View(func(st *State) {
		p := st.Pairings[c.pairing]
		me = wire.Me{Owner: c.owner, PairingID: c.pairing, Machine: p.Machine, KeyExpiresAt: p.Expires}
		if o := st.Owners[c.owner]; o != nil {
			me.Settings = o.Settings
		}
		for _, a := range st.Agents {
			if a.Pairing == c.pairing && a.Kind != wire.KindStanding && a.Open() {
				me.AgentsOpen++
			}
		}
	})
	writeJSON(w, http.StatusOK, me)
}

// agentLocalpart returns the localpart of a user id on this server.
func (s *Server) agentLocalpart(uid string) string {
	lp, ok := localpart(uid, s.Cfg.ServerName)
	if !ok {
		return ""
	}
	return lp
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request, c caller) {
	var req wire.CloseRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.UserIDs) > wire.MaxIDs {
		writeErr(w, newErr(wire.ErrBadRequest, fmt.Sprintf("at most %d user ids", wire.MaxIDs)))
		return
	}
	lps := make([]string, 0, len(req.UserIDs))
	var bad string
	s.Store.View(func(st *State) {
		for _, uid := range req.UserIDs {
			lp := s.agentLocalpart(uid)
			a := st.Agents[lp]
			if a == nil || a.Pairing != c.pairing || a.Kind == wire.KindStanding || a.Session == "" {
				bad = uid
				return
			}
			lps = append(lps, lp)
		}
	})
	if bad != "" {
		writeErr(w, newErr(wire.ErrNotYours, "not this pairing's agent: "+bad))
		return
	}
	reason := req.Reason
	if reason == "" {
		reason = wire.ReasonSessionEnded
	}
	closed, err := s.closeAgents(r.Context(), lps, sanitizeLabel(reason), "", nil)
	if err != nil {
		writeErr(w, internalErr)
		return
	}
	out := wire.CloseResponse{Closed: make([]string, 0, len(closed))}
	for _, lp := range closed {
		out.Closed = append(out.Closed, s.Cfg.mxid(lp))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAlive(w http.ResponseWriter, r *http.Request, c caller) {
	var req wire.AliveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.UserIDs) > wire.MaxIDs {
		writeErr(w, newErr(wire.ErrBadRequest, fmt.Sprintf("at most %d user ids", wire.MaxIDs)))
		return
	}
	out := wire.AliveResponse{Unknown: []string{}, Closed: []string{}}
	var open []string
	s.Store.View(func(st *State) {
		for _, uid := range req.UserIDs {
			lp := s.agentLocalpart(uid)
			a := st.Agents[lp]
			switch {
			case a == nil || a.Pairing != c.pairing || a.Kind == wire.KindStanding || a.Session == "":
				out.Unknown = append(out.Unknown, uid)
			case !a.Open():
				out.Closed = append(out.Closed, uid)
			default:
				open = append(open, lp)
			}
		}
	})
	if req.Touch {
		now := s.now()
		for _, lp := range open {
			s.Store.Touch(lp, now)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// clientErr reports whether err is a 4xx answer from tuwunel, for calls whose 4xx is expected,
// such as logging out a device that does not exist yet.
func clientErr(err error) bool {
	var me *matrix.Error
	return errors.As(err, &me) && me.Status >= 400 && me.Status < 500
}
