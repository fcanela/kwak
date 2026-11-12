package listener

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
)

// Status reactions (spec 5.14).
const (
	reactShown      = "👀"    // a hook printed the message to the session
	reactRun        = "⏳"    // a background run started for it
	reactAnswered   = "💬"    // the agent posted in its thread afterwards
	reactPaused     = "⏸️"   // the loop ceiling stopped a run for it
	reactBlocked    = "⛔"    // a check held it, or refused the agent's reply to it (5.16)
	reactUnknown    = "🤷‍♂️" // the agent called reply with dont_know: it does not know (decision 45)
	reactGotIt      = "👍"    // a clean background run printed it and did not answer it (decision 45)
	reactUnfinished = "⚠️"   // a background run that got it did not end cleanly (decision 45)
)

// reactWait bounds one reaction, a mint of a live session's agent included.
const reactWait = 30 * time.Second

// reactStopWait bounds the reactions sent while the listener stops, such as the ⚠️ of a run it
// stopped, so they do not hold the shutdown long (5.14).
const reactStopWait = 5 * time.Second

// reaction is one status reaction, sent by the agent of a session, or by one of its helpers, on a
// message addressed to it.
type reaction struct {
	key, helper, event, emoji string
}

// react sends reactions in the background. It never blocks. A token the homeserver no longer
// knows is re-minted once (reactOne); any other failure is logged and nothing else changes.
// Shutdown waits for reactions on the way, so a mint they started cannot race the last save.
func (l *Listener) react(rs []reaction) {
	if len(rs) == 0 {
		return
	}
	l.spawn(func() {
		parent := l.bg
		if parent.Err() != nil {
			// The listener is stopping: a run it stopped still gets its ⚠️, within a short bound.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(l.bg), reactStopWait)
			defer cancel()
			parent = ctx
		}
		for _, r := range rs {
			l.reactOne(parent, r)
		}
	})
}

// reactOne sends one reaction with the agent's own token. A live session's agent is minted if it
// has no token yet, or once more if the homeserver no longer knows the token, as for a tool call.
// The closed agent of an ended session is never reopened just to react, so it sends nothing.
// r.helper is the agent whose queue held the message: a finished helper's messages move to the
// session's agent (5.10), which then reacts, so a finished helper is never minted to react.
func (l *Listener) reactOne(parent context.Context, r reaction) {
	if !wire.EventID.MatchString(r.event) {
		return
	}
	l.mu.Lock()
	s := l.st.Sessions[r.key]
	if s == nil || l.phase == wire.StateUnpaired {
		l.mu.Unlock()
		return
	}
	tok := l.tokens[l.agentOf(s, r.helper)]
	usable := tok != "" && !s.AgentClosed
	live := s.Ended == nil
	room := l.roomID()
	l.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, reactWait)
	defer cancel()
	if !usable {
		if !live {
			l.log.Info("reaction not sent: the agent is closed", "session", r.key, "reaction", r.emoji)
			return
		}
		_, t, errText := l.token(ctx, r.key, r.helper)
		if errText != "" || t == "" {
			l.log.Info("reaction not sent: no agent", "session", r.key, "reaction", r.emoji)
			return
		}
		tok = t
	}
	_, err := l.d.Matrix(tok).React(ctx, room, "kwak-"+l.randomHex(8), r.event, r.emoji)
	if err != nil && matrix.IsCode(err, matrix.ErrUnknownToken) && live {
		// A resume re-mints the agent's token, often while its first reactions are on the way.
		if _, t, errText := l.remint(ctx, r.key, r.helper, tok); errText == "" && t != "" {
			_, err = l.d.Matrix(t).React(ctx, room, "kwak-"+l.randomHex(8), r.event, r.emoji)
		}
	}
	if err != nil {
		l.log.Info("reaction failed", "session", r.key, "reaction", r.emoji, "err", reactErr(err))
		return
	}
	l.log.Info("reaction sent", "session", r.key, "reaction", r.emoji, "event", r.event)
}

func reactErr(err error) string {
	var me *matrix.Error
	if errors.As(err, &me) {
		return strings.TrimPrefix(me.Error(), "matrix: ")
	}
	return "homeserver unreachable"
}

// shownLocked records messages a hook printed to the agent helper ("" for the session's agent):
// 👀 from it on each, the threads for the 💬 of its next post there, and what a running
// background run of s was shown (5.11): the threads, and the replies in the session's own threads,
// which get 👍 at the run's clean end unless answered (decision 45). A finished helper's message
// printed to the session's agent counts for the session's agent.
func (l *Listener) shownLocked(s *session, helper string, items []render.Item) {
	var rs []reaction
	run := l.running[s.Key]
	fk := flightKey(s.Key, helper)
	for _, it := range items {
		if it.Reason == render.Notice || !wire.EventID.MatchString(it.EventID) {
			continue
		}
		t := itemThread(it)
		rs = append(rs, reaction{s.Key, helper, it.EventID, reactShown})
		if l.shown[fk] == nil {
			l.shown[fk] = map[string][]string{}
		}
		l.shown[fk][t] = append(l.shown[fk][t], it.EventID)
		if run == nil {
			continue
		}
		if !containsString(run.printed, t) {
			run.printed = append(run.printed, t)
		}
		if t != it.EventID && l.rootedByLocked(t) == s.Key {
			run.replies = append(run.replies, fedItem{helper, it.EventID, t})
		}
	}
	l.react(rs)
}

// rootedByLocked is the key of the session whose agent or helper started thread root, or "".
func (l *Listener) rootedByLocked(root string) string {
	if a := l.st.Roots[root]; a != "" {
		return l.st.Mine[a].Session
	}
	return ""
}

// answered gives emoji (💬, or 🤷‍♂️ for a dont_know reply) on the messages of thread root printed
// to this agent since its last post or mark there. A running background run of the session
// remembers them as handled, so they get no 👍 or ⚠️ at its end (decision 45). It returns how many
// messages it marks.
func (l *Listener) answered(key, helper, root, emoji string) int {
	fk := flightKey(key, helper)
	l.mu.Lock()
	ids := l.shown[fk][root]
	delete(l.shown[fk], root)
	if r := l.running[key]; r != nil {
		for _, id := range ids {
			r.handled[fk+" "+id] = true
		}
	}
	l.mu.Unlock()
	rs := make([]reaction, 0, len(ids))
	for _, id := range ids {
		rs = append(rs, reaction{key, helper, id, emoji})
	}
	l.react(rs)
	return len(ids)
}

// runEndLocked gives the reactions of a background run that ends (decision 45). A clean end (exit
// 0, not stopped, not timed out) gives 👍 on each reply in the session's own threads that the run
// printed. Any other end gives ⚠️ on each message that got ⏳ from it. In both cases a message the
// agent answered (💬) or marked (🤷‍♂️) during the run gets nothing. A message with 👍 is done: it no
// longer waits for the agent's next post or mark in its thread, so it gets no 💬 or 🤷‍♂️ later.
func (l *Listener) runEndLocked(key string, r *resumeRun, clean bool) []reaction {
	var rs []reaction
	add := func(items []fedItem, emoji string) {
		for _, f := range items {
			if !r.handled[flightKey(key, f.helper)+" "+f.event] {
				rs = append(rs, reaction{key, f.helper, f.event, emoji})
			}
		}
	}
	switch {
	case clean:
		add(r.replies, reactGotIt)
		for _, f := range r.replies {
			if m := l.shown[flightKey(key, f.helper)]; m != nil {
				m[f.thread] = slices.DeleteFunc(m[f.thread], func(id string) bool { return id == f.event })
			}
		}
	case r.seen:
		// ⏳ is sent at the run's own SessionStart: a run stopped before it gave none.
		add(r.fed, reactUnfinished)
	}
	return rs
}

func containsString(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
