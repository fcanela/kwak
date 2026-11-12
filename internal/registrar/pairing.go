package registrar

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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
	test          bool // bound with `pair test <code>` (decision 40)
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
	// The room of the pairing's kind: a test pairing gets the test room (decision 39). bind checked
	// it was joined, so it is known; the listener uses the room id it is handed.
	room := s.cachedAgentsRoom(p.test)
	if room == "" {
		s.Log.Error("claim: no room for the pairing", "pairing", p.pairingID, "test", p.test)
		return wire.ClaimResponse{}, errExpired
	}
	var listener string
	err = s.Store.Update(func(st *State) error {
		pr := st.Pairings[p.pairingID]
		if pr == nil || pr.Revoked != nil {
			return errors.New("pairing gone")
		}
		pr.KeySHA256 = sha(key)
		listener = pr.Listener
		return nil
	})
	if err != nil {
		s.Log.Error("claim", "pairing", p.pairingID, "err", err)
		return wire.ClaimResponse{}, errExpired
	}
	p.collected = true
	delete(s.pending, p.code)
	delete(s.claims, p.claimHash)
	standing := ""
	if listener != "" { // always set by bind; POST /standing has the final name anyway
		standing = s.Cfg.mxid(listener)
	}
	return wire.ClaimResponse{
		Status: wire.ClaimPaired, Owner: p.owner, PairingID: p.pairingID, OwnerKey: key,
		KeyExpiresAt: p.keyExpires, RoomID: room,
		StandingUser: standing, ServerName: s.Cfg.ServerName,
		Test: p.test,
	}, nil
}

// bind runs the bot's `pair <code>` (or `pair test <code>`, decision 40) for owner (spec 4 step 4)
// and returns the reply.
func (s *Server) bind(ctx context.Context, owner, code string, test bool) (string, error) {
	if test && s.Cfg.TestRoom == "" {
		return "Test pairings are off on this server: it has no test room. Send pair <code> for a normal pairing.", nil
	}
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
	reply, err := s.bindLocked(ctx, owner, p, now, test)
	s.pmu.Lock()
	p.binding = false
	s.pmu.Unlock()
	return reply, err
}

func (s *Server) bindLocked(ctx context.Context, owner string, p *pendingPairing, now time.Time, test bool) (string, error) {
	var notes []string
	replaces := p.replaces
	if replaces != "" {
		ok, otherKind := false, false
		s.Store.View(func(st *State) {
			r := st.Pairings[replaces]
			ok = r != nil && r.Revoked == nil && r.Owner == owner && p.replacesOwner == owner
			otherKind = ok && r.Test != test
		})
		switch {
		case otherKind:
			// A pairing of the other kind is a new pairing, as if the config had been deleted and
			// paired fresh (decision 41): the old one is not moved and not revoked, so its agents
			// never change rooms (decision 39).
			notes = append(notes, fmt.Sprintf("The earlier pairing %s stays live until you revoke it or it expires; to end it, send: revoke %s",
				replaces, replaces))
			replaces = ""
		case !ok:
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
	// The room of the pairing's kind must be joined: the claim answers its id, and the pairing's
	// listener account joins it at the first POST /standing.
	room, e := s.agentsRoom(ctx, test)
	switch {
	case e != nil:
		return "", botErr("Test pairings are off on this server: it has no test room.")
	case room == "" && test:
		return "", errors.New("the test room is not joined yet")
	case room == "":
		return "", errors.New("the agents room is not joined yet")
	}
	expires := now.Add(s.Cfg.KeyLifetime.D())
	var listener string
	err = s.Store.Update(func(st *State) error {
		if o := st.Owners[owner]; o == nil || o.Removed != nil {
			return errors.New("not an owner")
		}
		// The pairing's own listener account (decision 42): a same-kind re-pair keeps the name of
		// the pairing it replaces, so the account moves with the agents at the confirmation;
		// anything else gets a word pair the owner never had.
		if r := st.Pairings[replaces]; replaces != "" && r != nil {
			listener = r.Listener
		}
		if listener == "" {
			var err error
			if listener, err = s.freshListener(st, owner, test); err != nil {
				return err
			}
		}
		st.Pairings[id] = &Pairing{
			Owner: owner, Machine: p.machine, OS: p.os, IP: p.ip,
			Created: now, Expires: expires, Replaces: replaces,
			IPs: map[string]time.Time{p.ip: now}, LastIP: p.ip, LastUsed: now, Test: test,
			Listener: listener,
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	s.pmu.Lock()
	p.owner, p.pairingID, p.keyExpires, p.test = owner, id, expires, test
	close(p.bound)
	s.pmu.Unlock()
	s.Log.Info("paired", "owner", owner, "pairing", id, "replaces", replaces, "test", test)
	kind := "pairing"
	if test {
		kind = "test pairing"
	}
	reply := fmt.Sprintf("Paired machine '%s' (%s) as %s (%s %s), asked from %s at %s UTC. Its listener account is %s. "+
		"The key expires %s. If you did not just run kwak pair on that machine, send: revoke %s",
		p.machine, p.os, pairLabel(listener), kind, id, p.ip, p.created.Format("15:04"), listener,
		expires.Format("2006-01-02"), id)
	if test {
		rooms := s.Cfg.TestRoom
		if s.Cfg.TestReportRoom != "" {
			rooms += " (and " + s.Cfg.TestReportRoom + " for reports)"
		}
		reply += fmt.Sprintf("\nThis is a test pairing: its agents and its listener account %s join only %s, never %s.",
			listener, rooms, s.Cfg.Room)
	}
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
