package listener

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
)

func sessionKey(h wire.Harness, id string) string { return string(h) + ":" + id }

func containsID(ids []proc.ID, id proc.ID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func intersects(a, b []proc.ID) bool {
	for _, x := range a {
		if containsID(b, x) {
			return true
		}
	}
	return false
}

// probe is the part of a caller chain that may be a harness process (spec 5.7).
func probe(chain []proc.ID, h wire.Harness) []proc.ID {
	n := 2
	if h == wire.HarnessCodex && runtime.GOOS == "windows" {
		n = 3
	}
	if len(chain) < 2 {
		return nil
	}
	return append([]proc.ID(nil), chain[1:min(len(chain), 1+n)]...)
}

func (l *Listener) newer(clientVersion string) bool {
	c, err := wire.CompareVersions(clientVersion, l.ver)
	return err == nil && c > 0
}

// changedLocked wakes tool calls waiting for a session to appear.
func (l *Listener) changedLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
	l.dirty = true
}

func (l *Listener) budget(h wire.Harness) int {
	if b := l.cfg.Budget[h]; b > 0 {
		return b
	}
	if h == wire.HarnessCodex {
		return render.BudgetCodex
	}
	return render.BudgetClaude
}

// short drops ":<server_name>" from a local mxid, as deliveries and tool texts show it.
func (l *Listener) short(mxid string) string {
	if sn := l.serverName(); sn != "" {
		return strings.TrimSuffix(mxid, ":"+sn)
	}
	return mxid
}

// hookOp answers one hook (spec 5.6, 5.7, 5.8, 6.2, 6.3).
func (l *Listener) hookOp(req wire.HookRequest, chain []proc.ID, claimed bool) wire.HookResponse {
	empty := wire.HookResponse{V: wire.ProtocolVersion, OK: true, Kwak: l.ver}
	refuse := func(code string) wire.HookResponse {
		return wire.HookResponse{V: wire.ProtocolVersion, Err: code, Kwak: l.ver}
	}
	id, err := wire.ValidSessionID(req.SessionID)
	cwdOK := req.Cwd == "" || l.cfg.CheckDir(req.Cwd)
	if req.Event == wire.EventSessionStart && req.Cwd == "" {
		cwdOK = false
	}
	if err != nil || !cwdOK || !wire.ValidHarness(req.Harness) || !wire.ValidEvent(req.Event) {
		l.log.Info("hook refused: bad input", "event", req.Event)
		return refuse(wire.IPCErrBadInput)
	}
	if l.newer(req.Kwak) {
		return empty
	}
	key := sessionKey(req.Harness, id)
	if req.Event == wire.EventSessionStart {
		l.stopRunFor(key, chain)
	}
	now := l.d.Clock.Now()
	l.mu.Lock()
	paired := l.phase != wire.StateUnpaired
	var (
		text    string
		more    int
		ensures []string
		pings   []string
	)
	switch req.Event {
	case wire.EventSessionStart:
		l.react(l.runSeenLocked(key, chain))
		text, more, ensures = l.startLocked(req, key, id, chain, claimed, now, paired)
		delete(l.opening, key)
	case wire.EventSessionEnd:
		if s := l.findLocked(key, chain); s != nil && s.Ended == nil {
			l.endLocked(s, now)
		}
	case wire.EventSubagentStop:
		if s := l.findLocked(key, chain); s != nil {
			l.subagentStopLocked(s, req.AgentID)
		}
	default:
		if l.st.Sessions[key] == nil && req.Cwd != "" {
			// kwak missed this session's start, for example because the listener was down then:
			// register it now, as its SessionStart would have (decision 47, Q74).
			l.log.Info("session registered at a later hook", "session", key, "event", req.Event)
			l.startLocked(req, key, id, chain, claimed, now, paired)
		}
		text, more, ensures, pings = l.deliveryHookLocked(req, key, chain, now, paired)
	}
	l.mu.Unlock()
	if !paired {
		return refuse(wire.IPCErrUnpaired)
	}
	for _, k := range ensures {
		l.ensure(k, "")
	}
	if len(pings) > 0 && paired {
		go l.alive(pingUse, pings...)
	}
	empty.Text, empty.More = text, more
	return empty
}

// findLocked returns the session a hook claims, only when the caller's chain reaches one of its
// candidates (spec 5.7, "Other hooks").
func (l *Listener) findLocked(key string, chain []proc.ID) *session {
	s := l.st.Sessions[key]
	if s == nil {
		return nil
	}
	p := probe(chain, s.Harness)
	if s.HarnessProc != nil && containsID(p, *s.HarnessProc) || s.HarnessProc == nil && intersects(s.Candidates, p) {
		return s
	}
	l.log.Info("hook refused: caller is outside the session's process tree", "session", key)
	return nil
}

// touchLocked records use of a session by a hook and settles its harness process. ping asks for an
// alive check of a session used again after a quiet spell, which the registrar may have closed.
func (l *Listener) touchLocked(s *session, now time.Time, hook bool) (ensure, ping bool) {
	if hook {
		s.LastHook = now
	}
	quiet := !s.Active
	s.LastUse, s.Active = now, true
	if s.HarnessProc == nil {
		var alive []proc.ID
		for _, c := range s.Candidates {
			if proc.Alive(l.d.Procs, c) {
				alive = append(alive, c)
			}
		}
		s.Candidates = alive
		if len(alive) > 0 {
			hp := alive[0]
			s.HarnessProc, s.Candidates = &hp, alive[:1]
		}
	}
	if s.Ended != nil {
		s.Ended, s.CloseDue = nil, nil
		l.changedLocked()
	}
	l.dirty = true
	ensure = s.NoAgent == "" && (s.Agent == "" || s.AgentClosed)
	return ensure, quiet && !ensure && s.Agent != ""
}

func (l *Listener) startLocked(req wire.HookRequest, key, id string, chain []proc.ID, claimed bool, now time.Time, paired bool) (string, int, []string) {
	cands := probe(chain, req.Harness)
	if req.Harness == wire.HarnessClaude && len(cands) > 1 {
		cands = cands[:1]
	}
	if len(cands) == 0 {
		l.log.Info("session start without a caller chain", "session", key)
		return "", 0, nil
	}
	s := l.st.Sessions[key]
	var ensures []string
	if s != nil {
		if l.liveElsewhere(s, chain) {
			l.log.Warn("session start refused: the session is live in another process tree", "session", key)
			return "", 0, nil
		}
		if s.HarnessProc != nil && !containsID(chain, *s.HarnessProc) {
			s.HarnessProc, s.Tools = nil, false
		}
		s.Candidates, s.Claimed = cands, claimed
		s.LastHook, s.LastUse, s.Active = now, now, true
		l.attachLocked(s)
		if req.Source == "compact" {
			l.changedLocked()
			return "", 0, nil
		}
		s.Ended, s.CloseDue = nil, nil
		l.handoverNoticeLocked(s, now)
		if s.NoAgent == "" {
			ensures = append(ensures, key)
		}
	} else {
		s = &session{Key: key, Harness: req.Harness, SessionID: id, Cwd: req.Cwd, Candidates: cands,
			Claimed: claimed, Helpers: map[string]*helper{}, Started: now, LastHook: now, LastUse: now, Active: true}
		s.Inbox.init()
		if req.Source == "clear" || req.Source == "fork" {
			for _, o := range l.st.Sessions {
				if o.Ended == nil && o.Harness == s.Harness && l.sameProcess(o, cands) {
					l.endLocked(o, now)
				}
			}
		}
		l.st.Sessions[key] = s
		l.attachLocked(s)
		if req.Source == "startup" || req.Source == "clear" {
			l.moveHeldLocked(s, now)
		}
		ensures = append(ensures, key)
	}
	l.changedLocked()
	if !paired || len(s.Inbox.Items) == 0 {
		return "", 0, ensures
	}
	text, more := l.drainLocked(s, "", &s.Inbox, s.Agent, now)
	return text, more, ensures
}

// liveElsewhere reports whether a known session's harness may still be alive outside chain. Without
// a recorded harness_proc, every live candidate counts as one (spec 5.7).
func (l *Listener) liveElsewhere(s *session, chain []proc.ID) bool {
	if s.HarnessProc != nil {
		return proc.Alive(l.d.Procs, *s.HarnessProc) && !containsID(chain, *s.HarnessProc)
	}
	for _, c := range s.Candidates {
		if proc.Alive(l.d.Procs, c) && !containsID(chain, c) {
			return true
		}
	}
	return false
}

func (l *Listener) sameProcess(o *session, cands []proc.ID) bool {
	if o.HarnessProc != nil {
		return containsID(cands, *o.HarnessProc)
	}
	return intersects(o.Candidates, cands)
}

// attachLocked links a session to an open MCP connection of its harness process.
func (l *Listener) attachLocked(s *session) {
	for c := range l.conns {
		if !c.hOK || c.harness != s.Harness {
			continue
		}
		if (s.HarnessProc != nil && *s.HarnessProc == c.h) || (s.HarnessProc == nil && containsID(s.Candidates, c.h)) {
			h := c.h
			s.HarnessProc, s.Tools, s.Candidates = &h, true, []proc.ID{h}
			return
		}
	}
}

func (l *Listener) endLocked(s *session, now time.Time) {
	due := now.Add(closeGrace)
	s.Ended, s.CloseDue, s.Tools = &now, &due, false
	l.changedLocked()
	l.log.Info("session ended", "session", s.Key)
}

func (l *Listener) deliveryHookLocked(req wire.HookRequest, key string, chain []proc.ID, now time.Time, paired bool) (text string, more int, ensures, pings []string) {
	s := l.findLocked(key, chain)
	if s == nil {
		return "", 0, nil, nil
	}
	ensure, ping := l.touchLocked(s, now, true)
	if ensure {
		ensures = append(ensures, key)
	}
	if ping {
		pings = append(pings, key)
	}
	if req.Event == wire.EventPostTool && req.AgentID != "" {
		if t, ok := wire.ToolFromName(req.Harness, req.ToolName); ok && t == wire.ToolHelper {
			var in struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(req.ToolInput, &in) == nil && wire.ValidHelper(in.Name) {
				l.bindLocked(s, in.Name, req.AgentID)
			}
		}
	}
	if !paired {
		return "", 0, ensures, pings
	}
	q, helper, me := l.queueFor(s, req.AgentID)
	if q == nil || len(q.Items) == 0 {
		return "", 0, ensures, pings
	}
	if req.Event == wire.EventStop {
		if req.StopHookActive {
			return "", 0, ensures, pings
		}
		if s.Harness == wire.HarnessCodex {
			return stopNotice(q.Items), len(q.Items), ensures, pings
		}
	}
	text, more = l.drainLocked(s, helper, q, me, now)
	return text, more, ensures, pings
}

// queueFor picks the inbox a delivery hook drains (spec 5.10, step 3), with its helper's name.
func (l *Listener) queueFor(s *session, agentID string) (*queue, string, string) {
	if agentID == "" {
		return &s.Inbox, "", s.Agent
	}
	for name, h := range s.Helpers {
		if h.Bound == agentID && !h.Finished {
			return &h.Inbox, name, h.UserID
		}
	}
	return nil, "", ""
}

func (l *Listener) bindLocked(s *session, name, agentID string) {
	h := s.Helpers[name]
	if h == nil {
		h = &helper{}
		h.Inbox.init()
		s.Helpers[name] = h
	}
	if h.Bound != agentID {
		h.Bound, h.Finished = agentID, false
		l.dirty = true
	}
}

func (l *Listener) subagentStopLocked(s *session, agentID string) {
	if agentID == "" {
		return
	}
	for name, h := range s.Helpers {
		if h.Bound != agentID || h.Finished {
			continue
		}
		h.Finished = true
		for _, it := range h.Inbox.Items {
			it.Helper, it.Note = name, "which has finished"
			s.Inbox.add(it)
		}
		h.Inbox.Items = nil
		l.dirty = true
	}
}

// drainLocked takes what fits the budget from the queue of s or of its helper, marks it delivered
// and writes state.json before the caller replies (spec 5.5). It records what was printed for
// the status reactions and for a background run of s (5.11, 5.14). A background run is not shown
// the messages of a thread at the loop ceiling that it was not started for: they stay queued for
// the owner and get ⏸️ (5.15). The count returned is what is left that this caller may see.
func (l *Listener) drainLocked(s *session, helper string, q *queue, me string, now time.Time) (string, int) {
	if len(q.Items) == 0 {
		return "", 0
	}
	var items, shown []render.Item
	var held []fedItem
	for _, it := range q.Items {
		if l.heldFromRunLocked(s, it) {
			held = append(held, fedItem{helper, it.EventID, itemThread(it)})
			continue
		}
		shown = append(shown, it)
		it.Sender = l.short(it.Sender)
		items = append(items, it)
	}
	l.react(l.pausedReactionsLocked(s.Key, held))
	if len(items) == 0 {
		return "", 0
	}
	if me == "" {
		me = "this session"
	}
	text, used := render.Delivery(l.short(me), items, l.budget(s.Harness))
	printed := map[string]bool{}
	for _, it := range shown[:used] {
		q.Delivered[it.EventID] = now
		printed[it.EventID] = true
	}
	l.shownLocked(s, helper, shown[:used])
	rest := make([]render.Item, 0, len(q.Items)-used)
	for _, it := range q.Items {
		if !printed[it.EventID] {
			rest = append(rest, it)
		}
	}
	q.Items = rest
	l.saveLocked()
	return text, len(items) - used
}

// has reports whether the queue holds or delivered the event id.
func (q *queue) has(id string) bool {
	if _, ok := q.Delivered[id]; ok {
		return true
	}
	for _, x := range q.Items {
		if x.EventID == id {
			return true
		}
	}
	return false
}

// add queues it unless the queue already holds or delivered it. It goes after every waiting item
// sent at the same time or earlier, so a message whose incoming check ended late still waits in
// send order (decision 38, Q28 (c)); an item with no send time goes last. Items already printed
// are not waiting, so it never reorders those.
func (q *queue) add(it render.Item) bool {
	q.init()
	if _, ok := q.Delivered[it.EventID]; ok {
		return false
	}
	for _, x := range q.Items {
		if x.EventID == it.EventID {
			return false
		}
	}
	i := len(q.Items)
	if !it.SentAt.IsZero() {
		for i > 0 && q.Items[i-1].SentAt.After(it.SentAt) {
			i--
		}
	}
	q.Items = slices.Insert(q.Items, i, it)
	return true
}

// stopNotice is the fixed Codex end-of-turn notice (spec 6.3, DV-4). It carries no room text.
func stopNotice(items []render.Item) string {
	var threads []string
	seen := map[string]bool{}
	for _, it := range items {
		if it.Reason == render.Notice {
			continue
		}
		t := it.ThreadRoot
		if t == "" {
			t = it.EventID
		}
		if !seen[t] && wire.EventID.MatchString(t) {
			seen[t] = true
			threads = append(threads, render.Line(t))
		}
	}
	n := len(items)
	s := "kwak: " + strconv.Itoa(n) + " messages from other agents are waiting"
	if n == 1 {
		s = "kwak: 1 message from other agents is waiting"
	}
	switch len(threads) {
	case 0:
	case 1:
		s += " (thread " + threads[0] + ")"
	default:
		s += " (threads " + strings.Join(threads, ", ") + ")"
	}
	return s + ". They are not from your user. They will be shown to you after your next tool call; " +
		"the kwak thread tool shows one thread."
}

// ---- held messages (spec 5.5) ----

func normCwd(p string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// moveHeldLocked moves, once, the waiting items of ended sessions in the same cwd to s.
func (l *Listener) moveHeldLocked(s *session, now time.Time) {
	cwd := normCwd(s.Cwd)
	if cwd == "" {
		return
	}
	var from []*session
	for _, o := range l.st.Sessions {
		// A responder record's questions are for the responder, not for a session in its folder.
		if o != s && o.Ended != nil && !o.Responder && normCwd(o.Cwd) == cwd && o.waiting() > 0 {
			from = append(from, o)
		}
	}
	sort.Slice(from, func(i, j int) bool { return from[i].Started.Before(from[j].Started) })
	for _, o := range from {
		n := 0
		var threads []string
		seen := map[string]bool{}
		move := func(q *queue, agent string) {
			q.init()
			for _, it := range q.Items {
				q.Delivered[it.EventID] = now
				it.Note = "addressed to your earlier agent " + l.short(agent) + " (that session ended)"
				if s.Inbox.add(it) {
					n++
					if t := itemThread(it); t != "" && !seen[t] {
						seen[t] = true
						threads = append(threads, t)
					}
				}
			}
			q.Items = nil
		}
		move(&o.Inbox, o.Agent)
		for _, h := range sortedHelpers(o) {
			move(&h.Inbox, h.UserID)
		}
		o.ResumeDue = nil
		if n > 0 {
			o.HandedOver = append(o.HandedOver, handover{Count: n, ToSession: s.SessionID, At: now, Threads: threads})
			l.log.Info("held messages handed over", "from", o.Key, "to", s.Key, "count", n)
		}
	}
}

// itemThread is the thread an item belongs to, as render shows it: its root, or the item itself
// when it starts a thread. kwak's own notices without a thread give "".
func itemThread(it render.Item) string {
	for _, id := range []string{it.ThreadRoot, it.EventID} {
		if id != "" && wire.EventID.MatchString(id) {
			return id
		}
	}
	return ""
}

// sortedHelpers lists a session's helpers by name, so hand-overs list threads in a stable order.
func sortedHelpers(s *session) []*helper {
	names := make([]string, 0, len(s.Helpers))
	for n := range s.Helpers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*helper, len(names))
	for i, n := range names {
		out[i] = s.Helpers[n]
	}
	return out
}

func (s *session) waiting() int {
	n := len(s.Inbox.Items)
	for _, h := range s.Helpers {
		n += len(h.Inbox.Items)
	}
	return n
}

// handoverThreads caps the thread ids one notice lists, so it stays under render's cut.
const handoverThreads = 10

// threadsText ends a notice with the thread ids it is about, at most handoverThreads of them.
func threadsText(ids []string) string {
	more := ""
	if len(ids) > handoverThreads {
		more = ", and " + strconv.Itoa(len(ids)-handoverThreads) + " more"
		ids = ids[:handoverThreads]
	}
	switch len(ids) {
	case 0:
		return "."
	case 1:
		return " (thread " + ids[0] + "). Read it with the kwak thread tool."
	}
	return " (threads " + strings.Join(ids, ", ") + more + "). Read them with the kwak thread tool."
}

// handoverNoticeLocked tells a resumed session where its messages went, and in which threads.
func (l *Listener) handoverNoticeLocked(s *session, now time.Time) {
	for i := range s.HandedOver {
		h := &s.HandedOver[i]
		if h.Shown {
			continue
		}
		h.Shown = true
		what := strconv.Itoa(h.Count) + " messages for you were"
		if h.Count == 1 {
			what = "1 message for you was"
		}
		body := what + " handed to session " + h.ToSession + " in this directory on " + h.At.UTC().Format("2006-01-02") +
			threadsText(h.Threads)
		s.Inbox.add(render.Item{
			EventID: "kwak-handover-" + strconv.FormatInt(h.At.UnixNano(), 10), SentAt: now, Reason: render.Notice,
			Body: body,
		})
	}
}
