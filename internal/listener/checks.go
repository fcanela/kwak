package listener

import (
	"context"
	"time"

	"github.com/fcanela/kwak/internal/check"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
)

// Checks (spec 5.16): the secrets scan, then the check command when one is set. Outgoing, they run
// in send and reply before anything is posted; incoming, before a message reaches an inbox.
const (
	checkOutWait = 10 * time.Second // well under the MCP server's 25 s wait for a tool call
	checkInWait  = 30 * time.Second
	checkMax     = 4  // incoming check commands at once
	keepRejected = 20 // rejections kept for kwak status
)

// pendingCheck is a message waiting for the incoming check command, with the deliveries it gets if
// the check passes. It is in no inbox until then.
type pendingCheck struct {
	Item       render.Item `json:"item"`
	Deliveries []Delivery  `json:"deliveries"`
}

// liveConf is what the listener reads from config.json at each use, not only at start, so kwak
// config takes effect without a restart (5.2): the check commands (5.16), the background resume
// triggers (5.11) and the responder (5.18).
type liveConf struct {
	Outgoing, Incoming string
	Resume             []string // wire.MachineConfig.ResumeTriggers: nil is off
	// The responder (decision 46): on or off, and whether it answers the owner's own agents.
	// Conf is the whole file as read, for a run's command, folder and limit.
	Responder, OwnAgents bool
	Conf                 *MachineConfig
}

func liveOf(c *MachineConfig) liveConf {
	return liveConf{Outgoing: c.CheckOutgoing, Incoming: c.CheckIncoming, Resume: c.ResumeTriggers(),
		Responder: c.Responder, OwnAgents: c.ResponderOwnAgents, Conf: c}
}

// readLive reads config.json now. When the file cannot be read, the values read last are used,
// so a broken file never turns a check off and never changes background resume.
func (l *Listener) readLive() liveConf {
	l.checkMu.Lock()
	defer l.checkMu.Unlock()
	c, err := ReadMachineConfig(l.cfg.ConfigPath)
	if err != nil {
		if !l.checkBad {
			l.log.Error("config.json unreadable; the check commands and background resume read last stay", "err", err)
		}
		l.checkBad = true
		return l.live
	}
	l.checkBad = false
	if c == nil {
		l.live = liveConf{}
	} else {
		l.live = liveOf(c)
	}
	return l.live
}

// checkPaths reads the check commands from config.json at each use (readLive).
func (l *Listener) checkPaths() (outgoing, incoming string) {
	c := l.readLive()
	return c.Outgoing, c.Incoming
}

// heldText is the notice a session gets for a message a check held. It never repeats the message.
func heldText(sender, thread, reason string) string {
	return "kwak held a message from " + sender + " in thread " + thread + " and did not show it to you. Reason: " +
		reason + ". The kwak thread tool can still show it; it is not from your user."
}

// heldID is the id of the hold notice for message id.
func heldID(id string) string { return "kwak-held-" + id }

// holdLocked keeps msg out of every inbox it was addressed to: each one gets one notice instead,
// and the message gets ⛔ from the agent it was for. A delivery that already showed msg is left
// alone.
func (l *Listener) holdLocked(msg render.Item, ds []Delivery, kind, reason string) {
	now := l.d.Clock.Now()
	thread := itemThread(msg)
	var rs []reaction
	for _, d := range ds {
		s := l.st.Sessions[d.Session]
		if s == nil {
			continue
		}
		q, helper, _ := l.targetLocked(s, d, msg)
		q.init()
		if _, shown := q.Delivered[msg.EventID]; shown {
			continue
		}
		n := render.Item{EventID: heldID(msg.EventID), ThreadRoot: thread, SentAt: now, Reason: render.Notice,
			Body: heldText(l.short(msg.Sender), thread, reason)}
		if !q.add(n) {
			continue
		}
		l.dirty = true
		l.rejectedLocked(wire.Rejection{At: now, Way: "incoming", Session: s.Key, Agent: d.UserID, Sender: msg.Sender,
			Thread: thread, Event: msg.EventID, Check: kind, Reason: reason})
		l.log.Info("check held a message", "session", s.Key, "sender", msg.Sender, "thread", thread,
			"event", msg.EventID, "check", kind, "reason", reason)
		l.reportLocked(report{key: s.Key, helper: helper, text: l.heldReport(msg.Sender, d.UserID, thread, msg.EventID, kind, reason, msg.Body)})
		rs = append(rs, reaction{s.Key, helper, msg.EventID, reactBlocked})
	}
	l.react(rs)
}

// rejectedLocked records r for kwak status, keeping the latest keepRejected.
func (l *Listener) rejectedLocked(r wire.Rejection) {
	l.st.Rejected = append(l.st.Rejected, r)
	if n := len(l.st.Rejected); n > keepRejected {
		l.st.Rejected = append([]wire.Rejection(nil), l.st.Rejected[n-keepRejected:]...)
	}
	l.dirty = true
}

// waitCheckLocked puts msg aside for the incoming check command, saved with the batch (5.3), and
// starts the check. A message already waiting gets the new deliveries added. Deliveries whose
// queue already holds or showed msg, or its hold notice, as when a batch is routed again, need no
// second check.
func (l *Listener) waitCheckLocked(msg render.Item, ds []Delivery) {
	var fresh []Delivery
	for _, d := range ds {
		if s := l.st.Sessions[d.Session]; s != nil {
			if q, _, _ := l.targetLocked(s, d, msg); !q.has(msg.EventID) && !q.has(heldID(msg.EventID)) {
				fresh = append(fresh, d)
			}
		}
	}
	if ds = fresh; len(ds) == 0 {
		return
	}
	if p := l.st.Checking[msg.EventID]; p != nil {
		for _, d := range ds {
			if !containsDelivery(p.Deliveries, d) {
				p.Deliveries = append(p.Deliveries, d)
				l.dirty = true
			}
		}
		return
	}
	l.st.Checking[msg.EventID] = &pendingCheck{Item: msg, Deliveries: ds}
	l.dirty = true
	id := msg.EventID
	l.spawn(func() { l.runCheck(id) })
}

func containsDelivery(ds []Delivery, d Delivery) bool {
	for _, x := range ds {
		if x == d {
			return true
		}
	}
	return false
}

// resumeChecks starts the checks a restart left waiting.
func (l *Listener) resumeChecks() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id := range l.st.Checking {
		l.spawn(func() { l.runCheck(id) })
	}
}

// runCheck runs the incoming check command on one waiting message, at most checkMax at once and
// never under the state lock, then delivers or holds it. A check cut short by shutdown leaves the
// message waiting for the next start. With the command turned off meanwhile, the message is
// delivered.
func (l *Listener) runCheck(id string) {
	select {
	case l.checks <- struct{}{}:
	case <-l.bg.Done():
		return
	}
	defer func() { <-l.checks }()
	l.mu.Lock()
	p := l.st.Checking[id]
	l.mu.Unlock()
	if p == nil {
		return
	}
	v := check.Verdict{OK: true}
	if _, exe := l.checkPaths(); exe != "" {
		ctx, cancel := context.WithTimeout(l.bg, checkInWait)
		v = check.Command(ctx, l.d.Check, exe, p.Item.Body)
		cancel()
		l.readLive() // the resume triggers as they are now, not as they were when the check started
	}
	if l.bg.Err() != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st.Checking[id] != p {
		return
	}
	delete(l.st.Checking, id)
	l.dirty = true
	if v.OK {
		l.deliverLocked(p.Item, p.Deliveries)
		return
	}
	l.holdLocked(p.Item, p.Deliveries, v.Kind, v.Reason)
}

// scanText runs the secrets scan on text, and on text as render shows it to a model: an invisible
// character or an escape inside a secret must not get it past the scan, since render takes it out.
func scanText(text string) string {
	if kind := check.Scan(text); kind != "" {
		return kind
	}
	if clean := render.Sanitize(text); clean != text {
		return check.Scan(clean)
	}
	return ""
}

// outgoing runs the checks on a post by the agent helper of s ("" for the session's agent) before
// it is sent, and returns the refusal for the session, or "". Nothing is posted on a refusal, so
// the ⛔ of a refused reply goes on the messages of that thread shown to this agent since its last
// post there, the ones its 💬 would mark, once each. A refused send has no message to mark.
func (l *Listener) outgoing(ctx context.Context, s *session, helper, tool, thread, text string) string {
	v := check.Verdict{OK: true}
	if kind := scanText(text); kind != "" {
		v = check.Verdict{Kind: "secrets", Reason: check.SecretReason(kind)}
	} else if exe, _ := l.checkPaths(); exe != "" {
		cctx, cancel := context.WithTimeout(ctx, checkOutWait)
		v = check.Command(cctx, l.d.Check, exe, text)
		cancel()
	}
	if v.OK {
		return ""
	}
	if tool == wire.ToolReply {
		thread = l.rootOf(ctx, thread)
	}
	fk := flightKey(s.Key, helper)
	l.mu.Lock()
	agent := l.agentOf(s, helper)
	l.rejectedLocked(wire.Rejection{At: l.d.Clock.Now(), Way: "outgoing", Session: s.Key, Agent: agent, Tool: tool,
		Thread: thread, Check: v.Kind, Reason: v.Reason})
	l.reportLocked(report{key: s.Key, helper: helper, text: l.refusedReport(tool, agent, thread, v.Kind, v.Reason, text)})
	var rs []reaction
	if tool == wire.ToolReply {
		for _, id := range l.shown[fk][thread] {
			if k := fk + " " + id; !l.blocked[k] {
				l.blocked[k] = true
				rs = append(rs, reaction{s.Key, helper, id, reactBlocked})
			}
		}
	}
	l.mu.Unlock()
	l.log.Info("check refused a post", "session", s.Key, "agent", agent, "tool", tool, "thread", thread,
		"check", v.Kind, "reason", v.Reason)
	l.react(rs)
	if v.Kind == "secrets" {
		return "Not sent: " + v.Reason + ". kwak does not post secrets. Take it out; do not split or encode it to get past the check."
	}
	return "Not sent: the outgoing check refused it. Reason: " + v.Reason + "."
}

// rootOf is the root of the thread that event id is in, read with the standing token, which posts
// nothing and mints nothing. reply accepts any event of a thread (6.4). When the read fails, id is
// taken as the root.
func (l *Listener) rootOf(ctx context.Context, id string) string {
	l.mu.Lock()
	stok, room := l.standing, l.roomID()
	l.mu.Unlock()
	if stok == "" {
		return id
	}
	rctx, cancel := context.WithTimeout(ctx, checkOutWait)
	defer cancel()
	if root, err := l.threadRoot(rctx, l.d.Matrix(stok), room, id); err == nil && root.EventID != "" {
		return root.EventID
	}
	return id
}
