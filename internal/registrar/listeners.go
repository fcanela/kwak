package registrar

import (
	"context"
	"errors"
	"sort"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
)

// Listener accounts (decision 42, spec 3.10). Each pairing has its own listener account,
// agent.<owner>.<adjective>-<animal>.listener (with "-test" after the animal for a test pairing).
// The name is chosen at `pair <code>` and kept in Pairing.Listener; the account is made at the
// pairing's first POST /standing, and its agent record's Pairing says which pairing holds it. A
// pairing made before decision 42 has no name and gets one at that same first call. A same-kind
// re-pair takes the old pairing's name, and the confirmation moves the record with the agents.
//
// The cut-over of decision 43: the per-owner standing accounts of the earlier builds
// (agent.<owner>.listener and agent.<owner>.listener-test) are closed by every job pass, the one
// at start included. That logs out every listener still on them; each one calls POST /standing
// again and lands on its own account. A machine that cannot recover that way runs `kwak pair`.

// closeReasonRetired is the close reason of an old per-owner standing account (decision 43).
const closeReasonRetired = "retired"

// maxListenerTries bounds the names one ensureListener call tries. A name is given up only when
// tuwunel already has an account of that name (a record pruned long ago), so 8 is far more than
// is ever needed.
const maxListenerTries = 8

// pairUsed reports whether owner ever had a listener account with this word pair, of either kind,
// in any state: a pairing's chosen name, or an agent record, closed and clash records included.
// Caller is inside View or Update.
func (st *State) pairUsed(owner, pair string) bool {
	is := func(lp string) bool {
		n, err := wire.ParseLocalpart(lp)
		return err == nil && n.Kind == wire.KindStanding && n.Owner == owner && n.Pair == pair
	}
	for _, p := range st.Pairings {
		if p.Owner == owner && p.Listener != "" && is(p.Listener) {
			return true
		}
	}
	for lp, a := range st.Agents {
		if a.Owner == owner && a.Kind == wire.KindStanding && is(lp) {
			return true
		}
	}
	return false
}

// freshListener picks a listener name owner never used. Caller is inside Update.
func (s *Server) freshListener(st *State, owner string, test bool) (string, error) {
	pair, err := wire.FreePair(s.Rand, func(p string) bool { return st.pairUsed(owner, p) })
	if err != nil {
		return "", err
	}
	return wire.ListenerLocalpart(owner, pair, test), nil
}

// pairLabel is how the bot names a listener account: its word pair, with "-test" for a test
// pairing ("brave-heron", "calm-otter-test"), or "" for none.
func pairLabel(lp string) string {
	n, err := wire.ParseLocalpart(lp)
	if err != nil || n.Pair == "" {
		return ""
	}
	if n.Test {
		return n.Pair + "-test"
	}
	return n.Pair
}

// ensureListener makes sure pairing id has its listener account: registered, named and joined to
// the room of the pairing's kind. It returns the account's localpart. A pairing with no name yet
// (made before decision 42) gets one. The caller holds the owner's standing lock.
//
// The name of an unconfirmed re-pair may still be held by the pairing it replaces: that is fine,
// the confirmation moves it. A name held by anything else (the replace did not happen) is dropped
// for a fresh one, and so is a name tuwunel already has an account for (a clash).
func (s *Server) ensureListener(ctx context.Context, owner, id string) (string, *wire.APIError) {
	for range maxListenerTries {
		var lp string
		var rec *Agent
		var test, viaReplace bool
		var e *wire.APIError
		s.Store.View(func(st *State) {
			p := st.Pairings[id]
			if p == nil {
				e = errBadKey
				return
			}
			lp, test = p.Listener, p.Test
			if a := st.Agents[lp]; lp != "" && a != nil {
				cp := *a
				rec = &cp
			}
			if r := st.Pairings[p.Replaces]; p.Confirmed == nil && p.Replaces != "" && r != nil {
				viaReplace = r.Revoked == nil && r.Owner == p.Owner && r.Test == p.Test && r.Listener == lp
			}
		})
		if e != nil {
			return "", e
		}
		switch {
		case lp == "":
		case rec == nil, rec.Pairing == id && rec.PendingSince != nil:
			retry, e := s.makeListener(ctx, owner, id, lp, test, rec == nil)
			if e != nil || !retry {
				return lp, e
			}
			continue
		case rec.Pairing == id && rec.Open():
			return lp, nil
		case rec.Pairing == id:
			// Closed while the pairing is live: only a revoke or an owner removal closes it, and
			// both mark the pairing first.
			return "", errBadKey
		case viaReplace && rec.Open():
			return lp, nil
		}
		if e := s.renameListener(id, lp); e != nil {
			return "", e
		}
	}
	return "", newErr(wire.ErrNamesExhausted, "no free listener account name")
}

// renameListener gives pairing id a fresh listener name, if its name is still old.
func (s *Server) renameListener(id, old string) *wire.APIError {
	err := s.Store.Update(func(st *State) error {
		p := st.Pairings[id]
		if p == nil || p.Listener != old {
			return nil
		}
		lp, err := s.freshListener(st, p.Owner, p.Test)
		if err != nil {
			return err
		}
		p.Listener = lp
		return nil
	})
	if err != nil {
		s.Log.Error("listener name", "pairing", id, "err", err)
		return internalErr
	}
	return nil
}

// makeListener registers, names and joins the listener account lp of pairing id. With fresh set
// there is no record yet: it is written pending first, so a crash before /register leaves a record
// that the next call finishes. On a fresh name that tuwunel already has, the name is marked as a
// clash and dropped, and retry is set. The record stays pending until the join worked.
func (s *Server) makeListener(ctx context.Context, owner, id, lp string, test, fresh bool) (retry bool, e *wire.APIError) {
	room, e := s.agentsRoom(ctx, test)
	switch {
	case e != nil:
		return false, e
	case room == "":
		return false, newErr(wire.ErrHomeserver, matrix.ErrNotFound)
	}
	now := s.now()
	if fresh {
		taken := false
		var dead *wire.APIError
		err := s.Store.Update(func(st *State) error {
			// A revoke that landed after the caller's check must not get an account it never
			// sees: its list of accounts to close is already taken.
			if dead = st.pairingErr(id, now); dead != nil {
				return nil
			}
			if st.Agents[lp] != nil {
				taken = true
				return nil
			}
			st.putAgent(lp, &Agent{Owner: owner, Kind: wire.KindStanding, Pairing: id, Created: now, LastActive: now, PendingSince: &now})
			return nil
		})
		if err != nil {
			return false, internalErr
		}
		if dead != nil {
			return false, dead
		}
		if taken {
			return true, nil
		}
	}
	uid := s.Cfg.mxid(lp)
	_, err := s.HS.Register(ctx, lp, "KWAK-"+id)
	if matrix.IsCode(err, matrix.ErrUserInUse) && fresh {
		s.Log.Warn("listener name clash", "account", lp, "pairing", id)
		err := s.Store.Update(func(st *State) error {
			st.Agents[lp] = &Agent{Owner: owner, Kind: wire.KindStanding, Created: now, LastActive: now,
				Closed: &now, CloseReason: "clash", CleanupDone: true}
			return nil
		})
		if err != nil {
			return false, internalErr
		}
		return true, s.renameListener(id, lp)
	}
	if err != nil && !matrix.IsCode(err, matrix.ErrUserInUse) {
		return false, newErr(wire.ErrHomeserver, hsErrcode(err))
	}
	if err := s.HS.SetDisplayName(ctx, uid, lp); err != nil {
		return false, newErr(wire.ErrHomeserver, hsErrcode(err))
	}
	if err := s.joinOrInvite(ctx, uid, room); err != nil {
		s.Log.Warn("listener account join failed", "account", lp, "err", err)
		return false, newErr(wire.ErrHomeserver, hsErrcode(err))
	}
	revoked := false
	err = s.Store.Update(func(st *State) error {
		a := st.Agents[lp]
		if a == nil || a.Pairing != id {
			return errors.New("listener record gone")
		}
		if p := st.Pairings[id]; p == nil || p.Revoked != nil {
			// Revoked while the account was made: the caller closes it (closeRevokedListeners).
			revoked = true
			return nil
		}
		a.PendingSince = nil
		return nil
	})
	if err != nil {
		s.Log.Error("listener record", "account", lp, "err", err)
		return false, internalErr
	}
	if revoked {
		return false, errBadKey
	}
	s.Log.Info("listener account ready", "account", lp, "pairing", id)
	return false, nil
}

// closeRevokedListeners closes every open listener account whose pairing is revoked or gone, of
// one owner, or of every owner when owner is "" (decision 42). Such an account is left over when a
// revoke lands while the account is made, or when the registrar stops between a re-pair's
// confirmation and the close of the replaced pairing's account. A revoked pairing never gets an
// account back (a re-pair moves records only from a live pairing), so the close needs no recheck.
// With owner set, the caller holds that owner's standing lock.
func (s *Server) closeRevokedListeners(ctx context.Context, owner string) {
	type orphan struct{ lp, reason string }
	var out []orphan
	s.Store.View(func(st *State) {
		for lp, a := range st.Agents {
			if a.Kind != wire.KindStanding || a.Closed != nil || a.Pairing == "" || (owner != "" && a.Owner != owner) {
				continue
			}
			p := st.Pairings[a.Pairing]
			switch {
			case p == nil:
				out = append(out, orphan{lp, "revoked"})
			case p.Revoked != nil && p.RevokeReason == "replaced":
				out = append(out, orphan{lp, "replaced"})
			case p.Revoked != nil:
				out = append(out, orphan{lp, "revoked"})
			}
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].lp < out[j].lp })
	for _, o := range out {
		held := ""
		if owner != "" {
			held = o.lp
		}
		s.Log.Info("closing the listener account of a revoked pairing", "account", o.lp)
		if _, err := s.closeAgents(ctx, []string{o.lp}, o.reason, held, nil); err != nil {
			s.Log.Warn("closing a listener account", "account", o.lp, "err", err)
		}
	}
}

// listenerJobs is the job pass of decisions 42 and 43. It closes the listener accounts of revoked
// pairings that are still open, then every open per-owner standing account of the builds before
// decision 42 (closeLegacy).
func (s *Server) listenerJobs(ctx context.Context) {
	s.closeRevokedListeners(ctx, "")
	s.closeLegacy(ctx)
}

// closeLegacy closes every open per-owner standing account, agent.<owner>.listener and
// agent.<owner>.listener-test, like any account: it leaves every room and logs out every device
// (decision 43). Nothing hands these accounts out any more. A listener still syncing on one gets
// M_UNKNOWN_TOKEN, calls POST /standing again, and gets its pairing's own account. The close is
// marked in the state before the leaves and the logout, so a failed call is finished by the
// cleanup retries, and a failed state write by the next job pass.
func (s *Server) closeLegacy(ctx context.Context) {
	var lps []string
	s.Store.View(func(st *State) {
		for lp, a := range st.Agents {
			if a.Kind != wire.KindStanding || a.Closed != nil {
				continue
			}
			if n, err := wire.ParseLocalpart(lp); err == nil && n.Pair == "" {
				lps = append(lps, lp)
			}
		}
	})
	if len(lps) == 0 {
		return
	}
	sort.Strings(lps)
	if _, err := s.closeAgents(ctx, lps, closeReasonRetired, "", nil); err != nil {
		s.Log.Warn("closing the old per-owner standing accounts", "err", err)
	}
}
