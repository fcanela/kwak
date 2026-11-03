package registrar

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
)

// KeyPrefix starts every owner key, so secret scanners find it.
const KeyPrefix = "kwak_ok_"

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// pendingPairing lives in memory only (spec 3.2). Owner and PairingID are set once a `pair <code>`
// bound it; bound is closed then.
type pendingPairing struct {
	code          string
	claimHash     string
	machine, os   string
	ip            string
	created       time.Time
	expires       time.Time
	replaces      string // pairing id of a valid old key
	replacesOwner string
	oldKeyBad     bool // a Bearer was sent but did not prove a valid pairing
	binding       bool
	bound         chan struct{}
	owner         string
	pairingID     string
	keyExpires    time.Time
	collected     bool
}

func normCode(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
}

func (s *Server) newCode() (string, error) {
	b, err := s.random(8)
	if err != nil {
		return "", err
	}
	v := binary.BigEndian.Uint64(b) >> 4
	out := make([]byte, 12)
	for i := 11; i >= 0; i-- {
		out[i] = crockford[v&31]
		v >>= 5
	}
	return string(out), nil
}

func fmtCode(c string) string { return c[0:4] + "-" + c[4:8] + "-" + c[8:12] }

// prunePending drops expired pairings. Caller holds pmu.
func (s *Server) prunePending(now time.Time) {
	kept := s.order[:0]
	for _, p := range s.order {
		if now.After(p.expires) || p.collected {
			delete(s.pending, p.code)
			delete(s.claims, p.claimHash)
			continue
		}
		kept = append(kept, p)
	}
	s.order = kept
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var req wire.PairRequest
	if !decodeBody(w, r, &req) {
		return
	}
	now := s.now()
	p := &pendingPairing{
		machine: sanitizeLabel(req.Machine),
		os:      sanitizeLabel(req.OS),
		ip:      clientIP(r),
		created: now,
		expires: now.Add(s.Cfg.PairingTTL.D()),
		bound:   make(chan struct{}),
	}
	if p.machine == "" {
		p.machine = "unnamed"
	}
	if key, ok := bearer(r); ok {
		s.Store.View(func(st *State) {
			id, pr := st.pairingByKey(sha(key))
			if pr == nil || pr.Revoked != nil || !now.Before(pr.Expires) {
				return
			}
			if o := st.Owners[pr.Owner]; o == nil || o.Removed != nil {
				return
			}
			p.replaces, p.replacesOwner = id, pr.Owner
		})
		p.oldKeyBad = p.replaces == ""
	}
	claim, err := s.randomB64(32)
	if err != nil {
		apiError(w, 500, "M_UNKNOWN", "internal error")
		return
	}
	p.claimHash = sha(claim)
	s.pmu.Lock()
	s.prunePending(now)
	for {
		code, err := s.newCode()
		if err != nil {
			s.pmu.Unlock()
			apiError(w, 500, "M_UNKNOWN", "internal error")
			return
		}
		if s.pending[code] == nil {
			p.code = code
			break
		}
	}
	if len(s.order) >= maxPending {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.pending, old.code)
		delete(s.claims, old.claimHash)
	}
	s.pending[p.code] = p
	s.claims[p.claimHash] = p
	s.order = append(s.order, p)
	s.pmu.Unlock()
	s.Log.Info("pairing started", "machine", p.machine, "os", p.os, "ip", p.ip, "replaces", p.replaces)
	writeJSON(w, http.StatusCreated, wire.PairResponse{
		Code: fmtCode(p.code), Claim: claim, ExpiresAt: p.expires,
		Bot: s.Cfg.Bot(), Replaces: p.replaces != "",
	})
}

var errExpired = &wire.APIError{Status: 410, Errcode: wire.ErrExpired, Message: "pairing expired; run kwak pair again"}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req wire.Empty
	claim, ok := bearer(r)
	if !ok {
		writeErr(w, errExpired)
		return
	}
	if !decodeBody(w, r, &req) {
		return
	}
	h := sha(claim)
	s.pmu.Lock()
	s.prunePending(s.now())
	p := s.claims[h]
	s.pmu.Unlock()
	if p == nil {
		writeErr(w, errExpired)
		return
	}
	wait := time.NewTimer(s.Cfg.ClaimWait.D())
	defer wait.Stop()
	expiry := time.NewTimer(p.expires.Sub(s.now()))
	defer expiry.Stop()
	select {
	case <-p.bound:
	default:
		select {
		case <-p.bound:
		case <-wait.C:
			writeJSON(w, http.StatusAccepted, wire.ClaimResponse{Status: wire.ClaimPending})
			return
		case <-expiry.C:
			writeErr(w, errExpired)
			return
		case <-r.Context().Done():
			return
		}
	}
	resp, apiErr := s.collect(p)
	if apiErr != nil {
		writeErr(w, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// collect generates the owner key of a bound pairing, stores its hash, and burns the claim.
func (s *Server) collect(p *pendingPairing) (wire.ClaimResponse, *wire.APIError) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	if p.collected || s.now().After(p.expires) {
		return wire.ClaimResponse{}, errExpired
	}
	raw, err := s.randomB64(32)
	if err != nil {
		return wire.ClaimResponse{}, internalErr
	}
	key := KeyPrefix + raw
	var room string
	err = s.Store.Update(func(st *State) error {
		pr := st.Pairings[p.pairingID]
		if pr == nil || pr.Revoked != nil {
			return errors.New("pairing gone")
		}
		pr.KeySHA256 = sha(key)
		room = st.RoomID
		return nil
	})
	if err != nil {
		s.Log.Error("claim", "pairing", p.pairingID, "err", err)
		return wire.ClaimResponse{}, errExpired
	}
	p.collected = true
	delete(s.pending, p.code)
	delete(s.claims, p.claimHash)
	return wire.ClaimResponse{
		Status: wire.ClaimPaired, Owner: p.owner, PairingID: p.pairingID, OwnerKey: key,
		KeyExpiresAt: p.keyExpires, RoomID: room,
		StandingUser: s.Cfg.mxid(wire.StandingLocalpart(p.owner)), ServerName: s.Cfg.ServerName,
	}, nil
}

// bind runs the bot's `pair <code>` for owner (spec 4 step 4) and returns the reply.
func (s *Server) bind(ctx context.Context, owner, code string) (string, error) {
	now := s.now()
	s.pmu.Lock()
	s.prunePending(now)
	p := s.pending[normCode(code)]
	if p == nil || p.binding || p.owner != "" {
		s.pmu.Unlock()
		return "No pairing with that code. Codes expire after 10 minutes; run kwak pair again.", nil
	}
	p.binding = true
	s.pmu.Unlock()
	reply, err := s.bindLocked(ctx, owner, p, now)
	s.pmu.Lock()
	p.binding = false
	s.pmu.Unlock()
	return reply, err
}

func (s *Server) bindLocked(ctx context.Context, owner string, p *pendingPairing, now time.Time) (string, error) {
	var notes []string
	replaces := p.replaces
	if replaces != "" {
		ok := false
		s.Store.View(func(st *State) {
			r := st.Pairings[replaces]
			ok = r != nil && r.Revoked == nil && r.Owner == owner && p.replacesOwner == owner
		})
		if !ok {
			replaces = ""
			notes = append(notes, "This pairing does not replace an earlier one.")
		}
	} else if p.oldKeyBad {
		notes = append(notes, "The old key sent with it was not valid, so this pairing does not replace an earlier one.")
	}
	id, err := s.newPairingID()
	if err != nil {
		return "", err
	}
	if err := s.ensureStanding(ctx, owner, id); err != nil {
		return "", err
	}
	expires := now.Add(s.Cfg.KeyLifetime.D())
	err = s.Store.Update(func(st *State) error {
		if o := st.Owners[owner]; o == nil || o.Removed != nil {
			return errors.New("not an owner")
		}
		st.Pairings[id] = &Pairing{
			Owner: owner, Machine: p.machine, OS: p.os, IP: p.ip,
			Created: now, Expires: expires, Replaces: replaces,
			IPs: map[string]time.Time{p.ip: now}, LastIP: p.ip, LastUsed: now,
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	s.pmu.Lock()
	p.owner, p.pairingID, p.keyExpires = owner, id, expires
	close(p.bound)
	s.pmu.Unlock()
	s.Log.Info("paired", "owner", owner, "pairing", id, "replaces", replaces)
	reply := fmt.Sprintf("Paired machine '%s' (%s) as pairing %s, asked from %s at %s UTC. The key expires %s. "+
		"If you did not just run kwak pair on that machine, send: revoke %s",
		p.machine, p.os, id, p.ip, p.created.Format("15:04"), expires.Format("2006-01-02"), id)
	for _, n := range notes {
		reply += "\n" + n
	}
	return reply, nil
}

func (s *Server) newPairingID() (string, error) {
	for {
		id, err := s.randomHex(4)
		if err != nil {
			return "", err
		}
		taken := false
		s.Store.View(func(st *State) { _, taken = st.Pairings[id] })
		if !taken {
			return id, nil
		}
	}
}

// ensureStanding creates the owner's standing account at the first pairing, or reopens it after
// an owner restore (spec 3.10). Its first device is the new pairing's, KWAK-<id>.
func (s *Server) ensureStanding(ctx context.Context, owner, pairingID string) error {
	lp := wire.StandingLocalpart(owner)
	uid := s.Cfg.mxid(lp)
	var rec *Agent
	var room string
	s.Store.View(func(st *State) {
		if a := st.Agents[lp]; a != nil {
			cp := *a
			rec = &cp
		}
		room = st.RoomID
	})
	if room == "" {
		return errors.New("the agents room is not joined yet")
	}
	if rec != nil && rec.Closed == nil && rec.PendingSince == nil {
		return nil
	}
	now := s.now()
	if rec == nil || rec.PendingSince != nil {
		if rec == nil {
			if err := s.Store.Update(func(st *State) error {
				st.putAgent(lp, &Agent{Owner: owner, Kind: wire.KindStanding, Created: now, LastActive: now, PendingSince: &now})
				return nil
			}); err != nil {
				return err
			}
		}
		_, err := s.HS.Register(ctx, lp, "KWAK-"+pairingID)
		if err != nil && !matrix.IsCode(err, matrix.ErrUserInUse) {
			return err
		}
		if err := s.HS.SetDisplayName(ctx, uid, lp); err != nil {
			return err
		}
	}
	if err := s.HS.JoinAs(ctx, uid, room); err != nil {
		return err
	}
	return s.Store.Update(func(st *State) error {
		a := st.Agents[lp]
		if a == nil {
			return errors.New("standing record gone")
		}
		a.PendingSince, a.Closed, a.CloseReason, a.CleanupDone = nil, nil, "", false
		return nil
	})
}
