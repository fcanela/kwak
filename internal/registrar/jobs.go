package registrar

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/fcanela/kwak/internal/wire"
)

// RunJobs calls Start, then runs the periodic jobs until ctx ends (spec 3.9): every jobs_every
// the idle close, key warnings, expiry and prune; every 5 minutes (or jobs_every if shorter) the
// cleanup retries; every 60 s a Flush. It flushes once more and returns nil when ctx ends.
func (s *Server) RunJobs(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		return nil
	}
	jobs := time.NewTicker(s.Cfg.JobsEvery.D())
	defer jobs.Stop()
	retry := time.NewTicker(min(cleanupEvery, s.Cfg.JobsEvery.D()))
	defer retry.Stop()
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	side := time.NewTicker(roomRetry)
	defer side.Stop()
	s.runJobs(ctx)
	for {
		select {
		case <-ctx.Done():
			if err := s.Store.Flush(); err != nil {
				s.Log.Error("flush", "err", err)
			}
			return nil
		case <-jobs.C:
			s.runJobs(ctx)
		case <-retry.C:
			s.retryCleanups(ctx)
		case <-flush.C:
			if err := s.Store.Flush(); err != nil {
				s.Log.Error("flush", "err", err)
			}
		case <-side.C:
			s.joinSideRooms(ctx, false)
		}
	}
}

func (s *Server) runJobs(ctx context.Context) {
	now := s.now()
	s.idleClose(ctx, now)
	s.keyJobs(ctx, now)
	s.listenerJobs(ctx)
	s.retryCleanups(ctx)
	s.prune(now)
	s.adminDMs(ctx)
	if err := s.Store.Flush(); err != nil {
		s.Log.Error("flush", "err", err)
	}
}

func (s *Server) idleClose(ctx context.Context, now time.Time) {
	idle := s.Cfg.IdleAfter.D()
	var lps []string
	s.Store.View(func(st *State) {
		for lp, a := range st.Agents {
			if a.Kind == wire.KindStanding || a.Session == "" || !a.Open() {
				continue
			}
			if now.Sub(maxTime(a.Created, a.LastActive)) > idle {
				lps = append(lps, lp)
			}
		}
	})
	sort.Strings(lps)
	if len(lps) > 0 {
		s.closeAgents(ctx, lps, "idle", "", func(a *Agent) bool {
			return a.Open() && now.Sub(maxTime(a.Created, a.LastActive)) > idle
		})
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

type warn struct{ owner, id, machine, expires string }

func (s *Server) keyJobs(ctx context.Context, now time.Time) {
	var warns []warn
	var revoke []struct{ id, reason string }
	s.Store.View(func(st *State) {
		for id, p := range st.Pairings {
			if p.Revoked != nil {
				continue
			}
			switch {
			case !now.Before(p.Expires):
				revoke = append(revoke, struct{ id, reason string }{id, "expired"})
			case p.Confirmed == nil && now.Sub(p.Created) > s.Cfg.UnconfirmedTTL.D():
				revoke = append(revoke, struct{ id, reason string }{id, "unconfirmed"})
			case p.Confirmed != nil && !p.Warned && p.Expires.Sub(now) < s.Cfg.KeyWarnBefore.D():
				warns = append(warns, warn{p.Owner, id, p.Machine, day(p.Expires)})
			}
		}
	})
	sort.Slice(revoke, func(i, j int) bool { return revoke[i].id < revoke[j].id })
	for _, r := range revoke {
		if err := s.revoke(ctx, r.id, r.reason); err != nil {
			s.Log.Warn("revoke", "pairing", r.id, "err", err)
		}
	}
	for _, w := range warns {
		err := s.Store.Update(func(st *State) error {
			if p := st.Pairings[w.id]; p != nil {
				p.Warned = true
			}
			return nil
		})
		if err != nil {
			continue
		}
		s.notice(ctx, s.ownerDM(w.owner), fmt.Sprintf(
			"Your kwak key for %s (%s) expires on %s. Run kwak pair on that machine to renew it.", w.machine, w.id, w.expires))
	}
}

func (s *Server) retryCleanups(ctx context.Context) {
	var lps []string
	var devices [][2]string
	s.Store.View(func(st *State) {
		for lp, a := range st.Agents {
			if a.Closed != nil && !a.CleanupDone {
				lps = append(lps, lp)
			}
		}
		for id, p := range st.Pairings {
			if p.Revoked != nil && !p.LogoutDone {
				devices = append(devices, [2]string{p.Owner, id})
			}
		}
	})
	sort.Strings(lps)
	for _, lp := range lps {
		s.retryCleanup(ctx, lp)
	}
	for _, d := range devices {
		s.logoutStandingDevice(ctx, d[0], d[1])
	}
}

// retryCleanup runs an unfinished cleanup under the agent's lock, if the agent is still closed.
func (s *Server) retryCleanup(ctx context.Context, lp string) {
	pending := func() string {
		var lk string
		s.Store.View(func(st *State) {
			if a := st.Agents[lp]; a != nil && a.Closed != nil && !a.CleanupDone {
				lk = lockKey(a, lp)
			}
		})
		return lk
	}
	lk := pending()
	if lk == "" {
		return
	}
	unlock := s.lock(lk)
	defer unlock()
	if pending() != "" {
		s.cleanup(ctx, lp)
	}
}

func (s *Server) prune(now time.Time) {
	var old []string
	s.Store.View(func(st *State) {
		for lp, a := range st.Agents {
			if a.Closed != nil && a.CleanupDone && now.Sub(*a.Closed) > s.Cfg.PruneClosedAfter.D() {
				old = append(old, lp)
			}
		}
	})
	if len(old) > 0 {
		err := s.Store.Update(func(st *State) error {
			for _, lp := range old {
				delete(st.Agents, lp)
			}
			return nil
		})
		if err != nil {
			s.Log.Error("prune", "err", err)
		}
	}
	s.pmu.Lock()
	s.prunePending(now)
	s.pmu.Unlock()
	s.cmu.Lock()
	for k, v := range s.cache {
		if now.Sub(v.at) >= requestTTL {
			delete(s.cache, k)
		}
	}
	s.cmu.Unlock()
}
