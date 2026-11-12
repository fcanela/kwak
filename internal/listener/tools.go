package listener

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
)

const (
	maxText    = 16000
	maxTo      = 20
	maxReplies = 50
	toolBudget = time.Minute
)

type result struct {
	text     string
	isErr    bool
	internal bool
}

func fail(text string) result { return result{text: text, isErr: true} }

// toolOp answers one tools/call (spec 6.4).
func (l *Listener) toolOp(c *client, req wire.ToolRequest) wire.ToolResponse {
	refuse := func(code string) wire.ToolResponse {
		return wire.ToolResponse{V: wire.ProtocolVersion, ID: req.ID, Err: code}
	}
	if req.MetaSession != "" {
		id, err := wire.ValidSessionID(req.MetaSession)
		if err != nil {
			l.log.Info("tool refused: bad meta_session")
			return refuse(wire.IPCErrBadInput)
		}
		req.MetaSession = id
	}
	if id, err := wire.ValidSessionID(req.MetaThread); err == nil {
		req.MetaThread = id
	} else {
		req.MetaThread = ""
	}
	var r result
	if l.newer(c.kwak) {
		r = fail(textOlder)
	} else {
		r = l.tool(c, req)
	}
	if r.internal {
		return refuse(wire.IPCErrInternal)
	}
	return wire.ToolResponse{V: wire.ProtocolVersion, ID: req.ID, OK: true, Text: r.text, IsError: r.isErr}
}

func (l *Listener) tool(c *client, req wire.ToolRequest) result {
	l.mu.Lock()
	phase, detail := l.phase, l.detail
	l.mu.Unlock()
	switch phase {
	case wire.StateUnpaired:
		return fail(detail)
	case wire.StateWaitingKeyring:
		return fail(textKeyring)
	}
	known := false
	for _, t := range wire.Tools {
		known = known || t == req.Name
	}
	if !known {
		return fail("kwak has no tool named " + req.Name)
	}
	s := l.resolve(c, req.MetaSession)
	if s == nil {
		return fail(textNotSeen)
	}
	ctx, cancel := context.WithTimeout(l.bg, toolBudget)
	defer cancel()
	args := req.Args
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	switch req.Name {
	case wire.ToolSend:
		return l.toolSend(ctx, s, req, args)
	case wire.ToolReply:
		return l.toolReply(ctx, s, req, args)
	case wire.ToolThread:
		return l.toolThread(ctx, s, req, args)
	case wire.ToolHelper:
		return l.toolHelper(ctx, s, req, args)
	case wire.ToolVolunteer:
		return l.toolVolunteer(s, args)
	}
	return l.toolStatus(s)
}

// resolve finds the session of a tool call (spec 5.7), waiting up to 2 s for it to appear.
func (l *Listener) resolve(c *client, meta string) *session {
	var deadline <-chan time.Time
	for {
		l.mu.Lock()
		s := l.resolveLocked(c, meta)
		ch := l.changed
		if s != nil {
			s.LastUse, s.Active = l.d.Clock.Now(), true
			l.dirty = true
		}
		l.mu.Unlock()
		if s != nil {
			return s
		}
		if deadline == nil {
			deadline = l.d.Clock.After(sessionWait)
		}
		select {
		case <-ch:
		case <-deadline:
			return nil
		case <-l.bg.Done():
			return nil
		}
	}
}

func (l *Listener) resolveLocked(c *client, meta string) *session {
	var cands []*session
	if c.hOK {
		for _, s := range l.st.Sessions {
			if s.Ended == nil && s.Harness == c.harness &&
				((s.HarnessProc != nil && *s.HarnessProc == c.h) || containsID(s.Candidates, c.h)) {
				cands = append(cands, s)
			}
		}
	}
	if c.harness == wire.HarnessCodex && meta != "" {
		for _, s := range cands {
			if s.SessionID == meta {
				return s
			}
		}
		key := sessionKey(wire.HarnessCodex, meta)
		if _, exists := l.st.Sessions[key]; !exists && c.hOK {
			now := l.d.Clock.Now()
			h := c.h
			s := &session{Key: key, Harness: wire.HarnessCodex, SessionID: meta, HarnessProc: &h, Tools: true,
				Helpers: map[string]*helper{}, Started: now, LastUse: now, Active: true}
			s.Inbox.init()
			l.st.Sessions[key] = s
			l.changedLocked()
			l.log.Info("session seen first through a tool call", "session", key)
			return s
		}
	}
	if len(cands) > 0 {
		sort.Slice(cands, func(i, j int) bool { return cands[i].Started.After(cands[j].Started) })
		if meta != "" && cands[0].SessionID != meta {
			l.log.Info("meta session does not match ancestry", "session", cands[0].Key)
		}
		return cands[0]
	}
	if c.env != "" {
		if s := l.st.Sessions[sessionKey(c.harness, c.env)]; s != nil && s.Ended == nil {
			return s
		}
	}
	return nil
}

// identity picks the helper a call acts as: as=..., or on Codex the helper bound to the thread id.
func (l *Listener) identity(s *session, as, metaThread string) (string, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if as != "" {
		if h := s.Helpers[as]; h != nil && h.UserID != "" {
			return as, ""
		}
		return "", "you have no helper named " + as + "; create it with the helper tool"
	}
	if s.Harness == wire.HarnessCodex && metaThread != "" && metaThread != s.SessionID {
		for n, h := range s.Helpers {
			if h.Bound == metaThread && h.UserID != "" {
				return n, ""
			}
		}
	}
	return "", ""
}

type mentions struct {
	UserIDs []string `json:"user_ids,omitempty"`
}

type eventRef struct {
	EventID string `json:"event_id"`
}

type relatesTo struct {
	RelType       string    `json:"rel_type"`
	EventID       string    `json:"event_id"`
	IsFallingBack bool      `json:"is_falling_back"`
	InReplyTo     *eventRef `json:"m.in_reply_to,omitempty"`
}

// message is what kwak sends (spec 8): body only, m.mentions always present.
type message struct {
	MsgType   string     `json:"msgtype"`
	Body      string     `json:"body"`
	Mentions  mentions   `json:"m.mentions"`
	RelatesTo *relatesTo `json:"m.relates_to,omitempty"`
}

func checkText(text string, to []string, as string) string {
	switch n := utf8.RuneCountInString(text); {
	case strings.TrimSpace(text) == "":
		return "the text is empty"
	case n > maxText:
		return fmt.Sprintf("the text has %d characters; the limit is %d", n, maxText)
	case len(to) > maxTo:
		return fmt.Sprintf("'to' has %d entries; the limit is %d", len(to), maxTo)
	case as != "" && !wire.ValidHelper(as):
		return "'as' must be a helper name (not \"listener\", which is reserved)"
	}
	return ""
}

var localpart = regexp.MustCompile(`^[a-z0-9._=/+-]+$`)

// normalizeTo turns user ids and localparts into local mxids (spec 6.4). Any local account may be
// named; only agents are mentioned (splitAgents, decision 38).
func (l *Listener) normalizeTo(to []string) ([]string, string) {
	sn := l.serverName()
	var out []string
	seen := map[string]bool{}
	for _, raw := range to {
		t := strings.TrimPrefix(strings.TrimSpace(raw), "@")
		lp, server, hasServer := strings.Cut(t, ":")
		if hasServer && server != sn {
			return nil, "kwak only knows accounts on " + sn
		}
		if !localpart.MatchString(lp) {
			return nil, "kwak cannot read the user id " + render.Sanitize(raw)
		}
		// The agent namespace is kwak's (spec 3.7): an id there that has none of the shapes of 3.6 is
		// a typo or a cut-off name, not a person, so it is refused instead of being sent unmentioned.
		if _, err := wire.ParseLocalpart(lp); err != nil && wire.InAgentNamespace(lp) {
			return nil, render.Sanitize(raw) + " is not a valid agent id: an agent id looks like agent.<owner>.<hex>, with 16 to 32 hex digits"
		}
		id := "@" + lp + ":" + sn
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, ""
}

// isAgent reports whether a user id is an agent account: its localpart has one of the agent
// shapes of spec 3.6. Any other account is a person (or the registrar bot).
func isAgent(id string) bool {
	lp, _, _ := strings.Cut(strings.TrimPrefix(id, "@"), ":")
	_, err := wire.ParseLocalpart(lp)
	return err == nil
}

// splitAgents splits ids into agents, whom kwak mentions, and persons, whom it never pings
// (decision 38, spec 6.4).
func splitAgents(ids []string) (agents, persons []string) {
	for _, id := range ids {
		if isAgent(id) {
			agents = append(agents, id)
		} else {
			persons = append(persons, id)
		}
	}
	return agents, persons
}

// personLines says, one line each, that kwak did not mention the persons named in 'to'.
func (l *Listener) personLines(persons []string) string {
	var b strings.Builder
	for _, id := range persons {
		b.WriteString(l.short(id) + " is a person: kwak does not ping people.\n")
	}
	return b.String()
}

func sendErrText(sn string, err error) string {
	var me *matrix.Error
	if errors.As(err, &me) {
		return "Not sent: the homeserver answered " + strings.TrimPrefix(me.Error(), "matrix: ") + "."
	}
	return "Not sent: " + sn + " is unreachable (last error: " + err.Error() + "). Try again later."
}

// send posts content as a session's agent or helper. The txnID is reused on a network retry with
// the same token, and replaced after a re-mint (spec 8).
func (l *Listener) send(ctx context.Context, key, helper string, content message) (eventID, uid, errText string) {
	uid, tok, et := l.token(ctx, key, helper)
	if et != "" {
		return "", "", et
	}
	room := l.roomIDLocked()
	txn := "kwak-" + l.randomHex(8)
	for attempt := 0; ; attempt++ {
		ev, err := l.d.Matrix(tok).Send(ctx, room, txn, content)
		if err == nil {
			return ev, uid, ""
		}
		if attempt > 0 || ctx.Err() != nil {
			return "", uid, sendErrText(l.serverName(), err)
		}
		if matrix.IsCode(err, matrix.ErrUnknownToken) {
			if uid, tok, et = l.remint(ctx, key, helper, tok); et != "" {
				return "", "", et
			}
			txn = "kwak-" + l.randomHex(8)
			continue
		}
		if !isNetErr(err) {
			return "", uid, sendErrText(l.serverName(), err)
		}
	}
}

func (l *Listener) roomIDLocked() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.roomID()
}

// joined is the agents room's member list, cached 60 s (spec 5.9). nil when unknown. cached says
// the list came from the cache; force skips the cache.
func (l *Listener) joined(ctx context.Context, force bool) (in map[string]bool, cached bool) {
	l.mu.Lock()
	now := l.d.Clock.Now()
	if !force && l.members != nil && now.Sub(l.membAt) < membersTTL {
		m := l.members
		l.mu.Unlock()
		return toSet(m), true
	}
	tok, room := l.standing, l.roomID()
	l.mu.Unlock()
	if tok == "" {
		return nil, false
	}
	ids, err := l.d.Matrix(tok).JoinedMembers(ctx, room)
	if err != nil {
		return nil, false
	}
	l.mu.Lock()
	l.members, l.membAt, l.membGone = ids, now, nil
	l.mu.Unlock()
	return toSet(ids), false
}

func toSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// unchecked says some id is missing from in and no fresh list has confirmed it gone yet.
func (l *Listener) unchecked(in map[string]bool, ids []string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		if !in[id] && !l.membGone[id] {
			return true
		}
	}
	return false
}

// markGone records the ids a freshly fetched list lacks.
func (l *Listener) markGone(in map[string]bool, ids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		if !in[id] {
			if l.membGone == nil {
				l.membGone = map[string]bool{}
			}
			l.membGone[id] = true
		}
	}
}

func (l *Listener) memberLines(ctx context.Context, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	in, cached := l.joined(ctx, false)
	if in == nil {
		return ""
	}
	// An agent created after the cached list would show as gone (PoC 2026-11-11), so a miss
	// fetches the list once more. An id a fresh list already lacked does not fetch again until
	// the cache expires (spec 5.9).
	if cached && l.unchecked(in, ids) {
		if fresh, _ := l.joined(ctx, true); fresh != nil {
			in, cached = fresh, false
		}
	}
	if !cached {
		l.markGone(in, ids)
	}
	var b strings.Builder
	for _, id := range ids {
		if !in[id] {
			b.WriteString(l.short(id) + " is not in the room: its session has ended. The message waits for its owner's next session.\n")
		}
	}
	return b.String()
}

func (l *Listener) toolSend(ctx context.Context, s *session, req wire.ToolRequest, raw json.RawMessage) result {
	var a struct {
		Text string   `json:"text"`
		To   []string `json:"to"`
		As   string   `json:"as"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return fail("kwak could not read the arguments of send")
	}
	if e := checkText(a.Text, a.To, a.As); e != "" {
		return fail(e)
	}
	to, e := l.normalizeTo(a.To)
	if e != "" {
		return fail(e)
	}
	helper, e := l.identity(s, a.As, req.MetaThread)
	if e != "" {
		return fail(e)
	}
	if e := l.outgoing(ctx, s, helper, wire.ToolSend, "", a.Text); e != "" {
		return fail(e)
	}
	agents, persons := splitAgents(to)
	ev, uid, e := l.send(ctx, s.Key, helper, message{MsgType: "m.text", Body: a.Text, Mentions: mentions{agents}})
	if e != "" {
		return fail(e)
	}
	now := l.d.Clock.Now()
	l.mu.Lock()
	l.st.Roots[ev] = uid
	// Open questions have their own map, so an older kwak restored over state.json does not give
	// them the text for questions with mentions. A send that names only persons mentions nobody,
	// so the room reads it as an open question, and so does the no-reply notice.
	qs := l.st.Questions
	if len(agents) == 0 {
		qs = l.st.OpenQs
	}
	qs[ev] = &question{Asker: uid, Session: s.Key, Helper: helper, AskedAt: now, Due: now.Add(l.questionTimeoutLocked())}
	l.saveLocked()
	l.mu.Unlock()
	text := "Sent as " + l.short(uid) + " in thread " + ev + ".\n" + l.personLines(persons)
	if len(persons) > 0 && len(agents) == 0 {
		text += textOpenNoAgent
	}
	return result{text: text + l.memberLines(ctx, agents) + textNoPoll}
}

// textOpenNoAgent ends a send whose 'to' named only persons: it mentions nobody.
const textOpenNoAgent = "No agent is mentioned, so it is an open question for volunteering agents.\n"

func (l *Listener) questionTimeoutLocked() time.Duration {
	if l.st.Settings != nil {
		if d, err := time.ParseDuration(l.st.Settings.QuestionTimeout); err == nil && d > 0 {
			return d
		}
	}
	return 15 * time.Minute
}

func (l *Listener) toolReply(ctx context.Context, s *session, req wire.ToolRequest, raw json.RawMessage) result {
	var a replyArgs
	if json.Unmarshal(raw, &a) != nil {
		return fail("kwak could not read the arguments of reply")
	}
	if !wire.EventID.MatchString(a.Thread) {
		return fail("'thread' must be an event id such as $abc")
	}
	switch {
	case a.DontKnow:
		return l.dontKnow(ctx, s, req, a)
	case a.Text == "":
		return fail("reply needs 'text' with your answer, or dont_know true when you do not know it")
	}
	if e := checkText(a.Text, a.To, a.As); e != "" {
		return fail(e)
	}
	to, e := l.normalizeTo(a.To)
	if e != "" {
		return fail(e)
	}
	helper, e := l.identity(s, a.As, req.MetaThread)
	if e != "" {
		return fail(e)
	}
	if e := l.outgoing(ctx, s, helper, wire.ToolReply, a.Thread, a.Text); e != "" {
		return fail(e)
	}
	uid, _, e := l.token(ctx, s.Key, helper)
	if e != "" {
		return fail(e)
	}
	l.mu.Lock()
	stok, room := l.standing, l.roomID()
	l.mu.Unlock()
	if stok == "" {
		return fail("kwak is not connected yet; try again in a moment")
	}
	m := l.d.Matrix(stok)
	root, err := l.threadRoot(ctx, m, room, a.Thread)
	if err != nil {
		return l.readErr(a.Thread, err)
	}
	latest := root.EventID
	if replies, err := m.ThreadReplies(ctx, room, root.EventID, maxReplies); err == nil && len(replies) > 0 {
		latest = replies[0].EventID
	}
	// Address on purpose (spec 6.4): the thread's starter and 'to' only. Earlier writers are not
	// mentioned, so a "thanks" that names nobody wakes nobody but the starter. Only agents are
	// mentioned (decision 38): a person who started the thread is not pinged, and a person in
	// 'to' gets a line in the result instead.
	agents, persons := splitAgents(to)
	var ment []string
	seen := map[string]bool{uid: true}
	for _, id := range append([]string{root.Sender}, agents...) {
		if id != "" && !seen[id] && isAgent(id) {
			seen[id] = true
			ment = append(ment, id)
		}
	}
	msg := message{MsgType: "m.text", Body: a.Text, Mentions: mentions{ment},
		RelatesTo: &relatesTo{RelType: relThread, EventID: root.EventID, IsFallingBack: true, InReplyTo: &eventRef{latest}}}
	ev, uid, e := l.send(ctx, s.Key, helper, msg)
	if e != "" {
		return fail(e)
	}
	l.mu.Lock()
	l.st.Threads[root.EventID] = Thread{Latest: ev, TS: l.d.Clock.Now().UnixMilli()}
	l.dirty = true
	l.mu.Unlock()
	l.answered(s.Key, helper, root.EventID, reactAnswered)
	text := "Replied as " + l.short(uid) + " in thread " + root.EventID
	if len(ment) > 0 {
		shorts := make([]string, len(ment))
		for i, id := range ment {
			shorts[i] = l.short(id)
		}
		text += ", mentioning " + strings.Join(shorts, ", ")
	}
	return result{text: text + ".\n" + l.personLines(persons) + l.memberLines(ctx, ment) + textNoPoll}
}

type replyArgs struct {
	Thread   string   `json:"thread"`
	Text     string   `json:"text"`
	DontKnow bool     `json:"dont_know"`
	To       []string `json:"to"`
	As       string   `json:"as"`
}

// dontKnow is reply with dont_know true (decision 45): it posts nothing, and marks 🤷‍♂️ on the
// messages that a 💬 from a reply in that thread would mark. 'as' picks the agent, as for a reply;
// 'to' is refused, because nothing is posted, so nobody is addressed. No check runs: no text
// leaves the machine.
func (l *Listener) dontKnow(ctx context.Context, s *session, req wire.ToolRequest, a replyArgs) result {
	switch {
	case a.Text != "":
		return fail("give either 'text' or dont_know true, not both")
	case len(a.To) > 0:
		return fail("'to' cannot go with dont_know: nothing is posted, so nobody is addressed")
	}
	helper, e := l.identity(s, a.As, req.MetaThread)
	if e != "" {
		return fail(e)
	}
	l.mu.Lock()
	stok, room := l.standing, l.roomID()
	l.mu.Unlock()
	if stok == "" {
		return fail("kwak is not connected yet; try again in a moment")
	}
	root, err := l.threadRoot(ctx, l.d.Matrix(stok), room, a.Thread)
	if err != nil {
		return l.readErr(a.Thread, err)
	}
	n := l.answered(s.Key, helper, root.EventID, reactUnknown)
	l.log.Info("reply dont_know", "session", s.Key, "thread", root.EventID, "marked", n)
	text := "Nothing was posted in thread " + root.EventID + ". "
	switch n {
	case 0:
		return result{text: text + "No message of it waited for an answer from you, so kwak added no " + reactUnknown + ".\n"}
	case 1:
		text += "kwak added " + reactUnknown + " to the message of it you were shown since your last post there.\n"
	default:
		text += fmt.Sprintf("kwak added %s to the %d messages of it you were shown since your last post there.\n", reactUnknown, n)
	}
	return result{text: text}
}

// threadRoot fetches id, and its thread root when id is itself in a thread, so kwak never starts a
// thread from a threaded event.
func (l *Listener) threadRoot(ctx context.Context, m Matrix, room, id string) (*matrix.Event, error) {
	ev, err := m.Event(ctx, room, id)
	if err != nil {
		return nil, err
	}
	if c, ok := parseContent(ev.Content); ok && c.relType == relThread && c.relEvent != "" && c.relEvent != ev.EventID {
		return m.Event(ctx, room, c.relEvent)
	}
	return ev, nil
}

func (l *Listener) readErr(thread string, err error) result {
	var me *matrix.Error
	switch {
	case errors.As(err, &me) && me.Status == 404:
		return fail("kwak found no message " + thread + " in the agents room")
	case errors.As(err, &me):
		return fail("kwak could not read thread " + thread + ": " + strings.TrimPrefix(me.Error(), "matrix: "))
	case isNetwork(err):
		return fail("kwak could not read thread " + thread + ": " + l.serverName() + " is unreachable")
	}
	l.log.Error("thread read failed", "thread", thread, "err", err)
	return result{internal: true}
}

func (l *Listener) toolThread(ctx context.Context, s *session, req wire.ToolRequest, raw json.RawMessage) result {
	var a struct {
		Thread string `json:"thread"`
		Limit  int    `json:"limit"`
	}
	if json.Unmarshal(raw, &a) != nil || !wire.EventID.MatchString(a.Thread) {
		return fail("'thread' must be an event id such as $abc")
	}
	if a.Limit <= 0 || a.Limit > maxReplies {
		a.Limit = maxReplies
	}
	l.mu.Lock()
	tok, room := l.standing, l.roomID()
	l.mu.Unlock()
	if tok == "" {
		return fail("kwak is not connected yet; try again in a moment")
	}
	m := l.d.Matrix(tok)
	root, err := l.threadRoot(ctx, m, room, a.Thread)
	if err != nil {
		return l.readErr(a.Thread, err)
	}
	replies, err := m.ThreadReplies(ctx, room, root.EventID, a.Limit)
	if err != nil {
		return l.readErr(a.Thread, err)
	}
	now := l.d.Clock.Now()
	var shown []matrix.Event
	for i := len(replies) - 1; i >= 0; i-- {
		if c, ok := parseContent(replies[i].Content); ok && c.relType == relThread {
			shown = append(shown, replies[i])
		}
	}
	helper, _ := l.identity(s, "", req.MetaThread)
	l.mu.Lock()
	q := &s.Inbox
	if h := s.Helpers[helper]; helper != "" && h != nil {
		q = &h.Inbox
	}
	ids := map[string]bool{root.EventID: true}
	for _, r := range shown {
		ids[r.EventID] = true
	}
	var keep []render.Item
	for _, it := range q.Items {
		if ids[it.EventID] {
			q.Delivered[it.EventID] = now
			continue
		}
		keep = append(keep, it)
	}
	if len(keep) != len(q.Items) {
		q.Items = keep
		l.saveLocked()
	}
	l.mu.Unlock()
	return result{text: l.threadText(root, shown, now)}
}

func (l *Listener) threadText(root *matrix.Event, replies []matrix.Event, now time.Time) string {
	var b strings.Builder
	b.WriteString("kwak: thread " + render.Line(root.EventID) + ", ")
	switch n := len(replies); n {
	case 0:
		b.WriteString("no replies yet.")
	case 1:
		b.WriteString("1 reply, the last " + ago(now, replies[0].TS) + ".")
	default:
		b.WriteString(fmt.Sprintf("%d replies, the last %s.", n, ago(now, replies[n-1].TS)))
	}
	b.WriteString(" " + render.Provenance)
	head := func(label string, ev matrix.Event) {
		c, _ := parseContent(ev.Content)
		b.WriteString("\n" + label + " from " + render.Line(l.short(ev.Sender)) + ", " +
			time.UnixMilli(ev.TS).UTC().Format("2006-01-02 15:04 UTC"))
		for _, line := range render.Body(c.body, root.EventID, maxText, 0) {
			b.WriteString("\n" + line)
		}
	}
	head("[question]", *root)
	for i, r := range replies {
		head(fmt.Sprintf("[reply %d/%d]", i+1, len(replies)), r)
	}
	return b.String()
}

func ago(now time.Time, ts int64) string {
	d := now.Sub(time.UnixMilli(ts))
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return fmt.Sprintf("%d %ss ago", n, unit)
	}
	switch {
	case d < time.Minute:
		return "less than a minute ago"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour")
	}
	return plural(int(d/(24*time.Hour)), "day")
}

func (l *Listener) toolHelper(ctx context.Context, s *session, req wire.ToolRequest, raw json.RawMessage) result {
	var a struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &a) != nil || !wire.ValidHelper(a.Name) {
		return fail("'name' must match ^[a-z][a-z0-9-]{0,23}$ and must not be \"listener\", which is reserved")
	}
	if _, _, e := l.token(ctx, s.Key, ""); e != "" {
		return fail(e)
	}
	uid, _, e := l.token(ctx, s.Key, a.Name)
	if e != "" {
		return fail(e)
	}
	if s.Harness == wire.HarnessCodex && req.MetaThread != "" && req.MetaThread != s.SessionID {
		l.mu.Lock()
		l.bindLocked(s, a.Name, req.MetaThread)
		l.mu.Unlock()
	}
	return result{text: fmt.Sprintf("Created %s. If a subagent will use it, call this tool from that subagent, "+
		"and pass as=%q to send and reply.", l.short(uid), a.Name)}
}

func (l *Listener) toolVolunteer(s *session, raw json.RawMessage) result {
	var a struct {
		On *bool `json:"on"`
	}
	if json.Unmarshal(raw, &a) != nil || a.On == nil {
		return fail("'on' must be true or false")
	}
	l.mu.Lock()
	s.Volunteer = *a.On
	l.changedLocked()
	l.mu.Unlock()
	if *a.On {
		return result{text: "You now volunteer: open questions in the agents room are shown to you. This ends with the session."}
	}
	return result{text: "You no longer volunteer: open questions are not shown to you."}
}

func (l *Listener) toolStatus(s *session) result {
	l.mu.Lock()
	defer l.mu.Unlock()
	agent := "none yet"
	if s.Agent != "" {
		agent = l.short(s.Agent)
	}
	var names []string
	for n, h := range s.Helpers {
		if h.UserID != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	helpers := "none"
	if len(names) > 0 {
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = n + " (" + l.short(s.Helpers[n].UserID) + ")"
		}
		helpers = strings.Join(parts, ", ")
	}
	vol := "off"
	if s.Volunteer {
		vol = "on"
	}
	return result{text: "Agent: " + agent + "\nHelpers: " + helpers + "\nVolunteer: " + vol + "\nConnection: " + l.phase}
}
