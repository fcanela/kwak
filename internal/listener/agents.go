package listener

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
)

type flight struct {
	done chan struct{}
	err  error
}

var errNoSession = errors.New("listener: no such session")

func flightKey(key, helper string) string { return key + "/" + helper }

// ensure mints the agent of a session, or of one of its helpers, through POST /agents. It runs
// once at a time per agent; callers asking together share the result (spec 5.8).
func (l *Listener) ensure(key, helper string) *flight {
	return l.ensureN(key, helper, 0)
}

func (l *Listener) ensureN(key, helper string, attempt int) *flight {
	fk := flightKey(key, helper)
	l.mu.Lock()
	if f := l.flights[fk]; f != nil {
		l.mu.Unlock()
		return f
	}
	f := &flight{done: make(chan struct{})}
	l.flights[fk] = f
	l.mu.Unlock()
	go func() {
		f.err = l.mint(key, helper)
		l.mu.Lock()
		delete(l.flights, fk)
		l.mu.Unlock()
		close(f.done)
		if f.err != nil && retryable(f.err) {
			l.mu.Lock()
			first := !l.retrying[fk]
			l.retrying[fk] = true
			l.mu.Unlock()
			if first {
				go l.retryEnsure(key, helper, attempt)
			}
		}
	}()
	return f
}

func retryable(err error) bool {
	if errors.Is(err, errNoSession) {
		return false
	}
	var up *errUnpaired
	if errors.As(err, &up) {
		return false
	}
	st, code, ok := apiStatus(err)
	if !ok {
		return true
	}
	return st >= 500 || code == wire.ErrHomeserver
}

func (l *Listener) retryEnsure(key, helper string, attempt int) {
	ok := l.sleep(l.backoff(attempt))
	l.mu.Lock()
	delete(l.retrying, flightKey(key, helper))
	if !ok {
		l.mu.Unlock()
		return
	}
	s := l.st.Sessions[key]
	need := s != nil && s.Ended == nil && l.phase != wire.StateUnpaired && l.tokens[l.agentOf(s, helper)] == ""
	l.mu.Unlock()
	if need {
		l.ensureN(key, helper, attempt+1)
	}
}

func (l *Listener) agentOf(s *session, helper string) string {
	if helper == "" {
		return s.Agent
	}
	if h := s.Helpers[helper]; h != nil {
		return h.UserID
	}
	return ""
}

func (l *Listener) mint(key, name string) error {
	l.mu.Lock()
	s := l.st.Sessions[key]
	if s == nil {
		l.mu.Unlock()
		return errNoSession
	}
	if l.phase == wire.StateUnpaired {
		d := l.detail
		l.mu.Unlock()
		return &errUnpaired{d, d}
	}
	if s.Responder {
		l.mu.Unlock()
		return l.mintResponder(key)
	}
	h, sid := s.Harness, s.SessionID
	parentOK := s.Agent != "" && !s.AgentClosed && l.tokens[s.Agent] != ""
	l.mu.Unlock()
	if name != "" && !parentOK {
		if pf := l.ensure(key, ""); waitDone(l.bg, pf.done) && pf.err != nil {
			return pf.err
		}
	}
	a, err := l.agentCall(h, sid, name)
	if err != nil && name != "" {
		if _, code, _ := apiStatus(err); code == wire.ErrParentClosed {
			pf := l.ensure(key, "")
			if waitDone(l.bg, pf.done) && pf.err == nil {
				a, err = l.agentCall(h, sid, name)
			}
		}
	}
	if err != nil {
		l.regFailed(err)
		_, code, _ := apiStatus(err)
		l.log.Warn("agent request failed", "session", key, "name", name, "err", describe(err))
		if name == "" && (code == wire.ErrNotYours || code == wire.ErrBadSession || code == wire.ErrNamesExhausted) {
			l.mu.Lock()
			s.NoAgent = agentErrText(err)
			l.dirty = true
			l.mu.Unlock()
		}
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens[a.UserID] = a.AccessToken
	l.st.Mine[a.UserID] = Agent{Session: key, Helper: name}
	if name == "" {
		s.Agent, s.AgentClosed, s.NoAgent = a.UserID, false, ""
	} else {
		hp := s.Helpers[name]
		if hp == nil {
			hp = &helper{}
			hp.Inbox.init()
			s.Helpers[name] = hp
		}
		hp.UserID = a.UserID
	}
	l.changedLocked()
	l.log.Info("agent ready", "session", key, "agent", a.UserID, "new", a.New, "reopened", a.Reopened)
	return nil
}

func (l *Listener) agentCall(h wire.Harness, sid, helper string) (wire.Agent, error) {
	ctx, cancel := context.WithTimeout(l.bg, 30*time.Second)
	defer cancel()
	return l.d.Registrar.Agent(ctx, wire.AgentRequest{
		RequestID: l.randomHex(16), Harness: h, Session: sid, Helper: helper,
	})
}

func waitDone(ctx context.Context, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// agentErrText is the tool text for a failed POST /agents (spec 5.1 table).
func agentErrText(err error) string {
	var up *errUnpaired
	if errors.As(err, &up) {
		return up.status
	}
	st, code, _ := apiStatus(err)
	switch {
	case st == 401:
		return textUnpaired
	case code == wire.ErrOwnerRemoved:
		return textRemoved
	case code == wire.ErrNotYours:
		return textNotYours
	case code == wire.ErrParentClosed:
		return textParentClosed
	case code == wire.ErrBadSession:
		return textBadSession
	case code == wire.ErrNamesExhausted:
		return textNames
	}
	return fmt.Sprintf("kwak has no agent for this session yet (%s); retrying", describe(err))
}

// token gives the user id and token of a session's agent or helper, minting when needed and
// waiting up to 10 s for a pending mint (spec 5.8).
func (l *Listener) token(ctx context.Context, key, helper string) (uid, tok, errText string) {
	l.mu.Lock()
	s := l.st.Sessions[key]
	if s == nil {
		l.mu.Unlock()
		return "", "", textNotSeen
	}
	uid = l.agentOf(s, helper)
	tok = l.tokens[uid]
	closed := s.AgentClosed
	noAgent := s.NoAgent
	l.mu.Unlock()
	if noAgent != "" {
		return "", "", noAgent
	}
	if uid != "" && tok != "" && !closed {
		return uid, tok, ""
	}
	f := l.ensure(key, helper)
	select {
	case <-f.done:
	case <-l.d.Clock.After(ensureWait):
		return "", "", "kwak has no agent for this session yet (the registrar is slow); retrying"
	case <-ctx.Done():
		return "", "", "kwak has no agent for this session yet; retrying"
	}
	if f.err != nil {
		return "", "", agentErrText(f.err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	uid = l.agentOf(s, helper)
	return uid, l.tokens[uid], ""
}

// remint replaces a token the homeserver no longer knows, once per agent: it is skipped when the
// token already changed since the failed call (spec 5.8).
func (l *Listener) remint(ctx context.Context, key, helper, failed string) (string, string, string) {
	l.mu.Lock()
	s := l.st.Sessions[key]
	if s == nil {
		l.mu.Unlock()
		return "", "", textNotSeen
	}
	uid := l.agentOf(s, helper)
	if cur := l.tokens[uid]; cur != "" && cur != failed {
		l.mu.Unlock()
		return uid, cur, ""
	}
	if l.tokens[uid] == failed {
		delete(l.tokens, uid)
	}
	l.mu.Unlock()
	return l.token(ctx, key, helper)
}

// reensure runs after a reload (spec 5.1). A re-pair keeps the agents and their tokens (4 step 6).
// The ping is read-only; sessions that never got an agent are minted now, the others at their next
// hook or tool call.
func (l *Listener) reensure() {
	l.alive(pingReconcile)
	l.mu.Lock()
	var ws []string
	for k, s := range l.st.Sessions {
		if s.Ended == nil && s.NoAgent == "" && s.Agent == "" {
			ws = append(ws, k)
		}
	}
	l.mu.Unlock()
	for _, k := range ws {
		l.ensure(k, "")
	}
}

// closeAgents closes a session's agents once its grace ran out (spec 5.8). Before the listener is
// connected, or on a failure, the deadline stays, so a later tick retries. A responder record
// closes nothing: its account is shared by every responder run (5.18).
func (l *Listener) closeAgents(key string) {
	l.mu.Lock()
	s := l.st.Sessions[key]
	if s == nil || s.Ended == nil || s.CloseDue == nil || l.standing == "" {
		l.mu.Unlock()
		return
	}
	var ids []string
	if s.Agent != "" && !s.AgentClosed && !s.Responder {
		ids = append(ids, s.Agent)
	}
	for _, h := range s.Helpers {
		if h.UserID != "" && !s.AgentClosed {
			ids = append(ids, h.UserID)
		}
	}
	l.mu.Unlock()
	for i := 0; i < len(ids); i += aliveChunk {
		ctx, cancel := context.WithTimeout(l.bg, 30*time.Second)
		err := l.d.Registrar.Close(ctx, ids[i:min(len(ids), i+aliveChunk)], wire.ReasonSessionEnded)
		cancel()
		if err != nil {
			l.regFailed(err)
			l.log.Warn("close failed", "session", key, "err", describe(err))
			return
		}
	}
	l.mu.Lock()
	revived := s.Ended == nil
	if !revived {
		s.CloseDue = nil
	}
	if len(ids) > 0 {
		s.AgentClosed = true
	}
	for _, id := range ids {
		delete(l.tokens, id)
	}
	l.changedLocked()
	l.mu.Unlock()
	l.log.Info("agents closed", "session", key, "count", len(ids))
	if revived && len(ids) > 0 {
		l.ensure(key, "")
	}
}

type pingKind int

const (
	pingReconcile pingKind = iota // startup, reload, clock jump: read-only, closed agents reopen at their next use
	pingDaily                     // sessions used since the last ping
	pingUse                       // the given sessions, used again after a quiet spell
)

// alive sends POST /agents/alive in chunks of 500 (spec 5.8). Only daily and use pings count as
// activity at the registrar. What the registrar lists as unknown or closed loses its token; daily
// and use pings re-mint it at once, a reconcile ping leaves that to the session's next hook.
// Responder records are left out: the registrar does not track their shared account this way.
func (l *Listener) alive(kind pingKind, keys ...string) {
	l.mu.Lock()
	if l.phase == wire.StateUnpaired || l.standing == "" {
		l.mu.Unlock()
		return
	}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	owner := map[string]Agent{}
	sentTok := map[string]string{}
	var ids, picked []string
	for k, s := range l.st.Sessions {
		switch {
		case s.Responder, kind == pingDaily && !s.Active, kind == pingUse && !want[k], kind == pingReconcile && s.Ended != nil:
			continue
		}
		picked = append(picked, k)
		if s.Agent == "" {
			continue
		}
		owner[s.Agent] = Agent{Session: k}
		ids = append(ids, s.Agent)
		for n, h := range s.Helpers {
			if h.UserID != "" {
				owner[h.UserID] = Agent{Session: k, Helper: n}
				ids = append(ids, h.UserID)
			}
		}
	}
	for _, id := range ids {
		sentTok[id] = l.tokens[id]
	}
	l.mu.Unlock()
	if len(ids) == 0 && kind != pingDaily {
		return
	}
	var redo []string
	ok := true
	for i := 0; i < len(ids); i += aliveChunk {
		ctx, cancel := context.WithTimeout(l.bg, 30*time.Second)
		unknown, closed, err := l.d.Registrar.Alive(ctx, ids[i:min(len(ids), i+aliveChunk)], kind != pingReconcile)
		cancel()
		if err != nil {
			l.regFailed(err)
			ok = false
			break
		}
		redo = append(append(redo, unknown...), closed...)
	}
	l.mu.Lock()
	if ok && kind == pingDaily {
		for _, k := range picked {
			if s := l.st.Sessions[k]; s != nil {
				s.Active = false
			}
		}
		l.st.LastPing = l.d.Clock.Now()
		l.dirty = true
	}
	var again []Agent
	for _, id := range redo {
		a, known := owner[id]
		if l.tokens[id] != sentTok[id] {
			continue // minted while the ping was out: the answer is stale
		}
		if s := l.st.Sessions[a.Session]; known && s != nil && s.Ended == nil {
			if a.Helper == "" {
				s.AgentClosed = true
			}
			delete(l.tokens, id)
			l.dirty = true
			if kind != pingReconcile {
				again = append(again, a)
			}
		}
	}
	l.mu.Unlock()
	for _, a := range again {
		l.ensure(a.Session, a.Helper)
	}
}

// isNetErr reports a failure that never reached the homeserver.
func isNetErr(err error) bool {
	var me *matrix.Error
	return err != nil && !errors.As(err, &me)
}

func isNetwork(err error) bool {
	var ne net.Error
	return errors.As(err, &ne)
}
