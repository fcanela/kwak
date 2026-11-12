package listener

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
)

// Status reactions (spec 5.14).
const (
	reactShown    = "👀"  // a hook printed the message to the session
	reactRun      = "⏳"  // a background run started for it
	reactAnswered = "💬"  // the agent posted in its thread afterwards
	reactPaused   = "⏸️" // the loop ceiling stopped a run for it
	reactBlocked  = "⛔"  // a check held it, or refused the agent's reply to it (5.16)
)

// reactWait bounds one reaction, a mint of a live session's agent included.
const reactWait = 30 * time.Second

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
		for _, r := range rs {
			l.reactOne(r)
		}
	})
}

// reactOne sends one reaction with the agent's own token. A live session's agent is minted if it
// has no token yet, or once more if the homeserver no longer knows the token, as for a tool call.
// The closed agent of an ended session is never reopened just to react, so it sends nothing.
// r.helper is the agent whose queue held the message: a finished helper's messages move to the
// session's agent (5.10), which then reacts, so a finished helper is never minted to react.
func (l *Listener) reactOne(r reaction) {
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
	ctx, cancel := context.WithTimeout(l.bg, reactWait)
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
// 👀 from it on each, the threads for the 💬 of its next post there, and the threads a running
// background run of s was shown (5.11). A finished helper's message printed to the session's
// agent counts for the session's agent.
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
		if run != nil && !containsString(run.printed, t) {
			run.printed = append(run.printed, t)
		}
	}
	l.react(rs)
}

// answered gives 💬 on the messages of thread root printed to this agent since its last post there.
func (l *Listener) answered(key, helper, root string) {
	fk := flightKey(key, helper)
	l.mu.Lock()
	ids := l.shown[fk][root]
	delete(l.shown[fk], root)
	l.mu.Unlock()
	rs := make([]reaction, 0, len(ids))
	for _, id := range ids {
		rs = append(rs, reaction{key, helper, id, reactAnswered})
	}
	l.react(rs)
}

func containsString(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
