package listener

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
)

// Reports (spec 5.17; decisions 33, 36, 38, 39, 41 and 42): short reports in the pairing's report
// room for a check that refused a post or held a message (5.16), and for the hourly limit on
// background runs when it first holds (5.11). A check's report holds the message text, rendered as
// kwak shows other agents' text, and a matrix.to link to the message or its thread (decision 41),
// except when the secrets scan stopped it: then the report never holds the text, which would
// publish the secret the scan stopped. There is no cap on reports (decision 41). To post, the
// listener asks the registrar to join one of its own accounts to the report room
// (POST /report/join), posts with that account's own token, and leaves again with the same token.
// This holds for the listener account too: since decision 42 it is the pairing's own, so its leave
// cannot cut another listener's post. A report that fails is logged and never blocks a delivery, a
// tool call or a run.
const (
	reportWait      = 30 * time.Second // the join and the post of one report together
	reportLeaveWait = 10 * time.Second // an agent's leave, on its own clock, also when the post used up reportWait or the listener stops
	reportReasonMax = 120              // characters of a check's reason in a report line
)

// report is one report for the report room. key and helper name the agent that posts it, as for
// a reaction; an empty key, or an agent that holds no token, posts with the pairing's listener
// account (the standing account).
type report struct {
	key, helper string
	text        string
}

// reportLocked queues r. Call it under l.mu.
func (l *Listener) reportLocked(r report) {
	l.spawn(func() { l.postReport(r) })
}

// reportHead starts every report line: whose machine it comes from.
func (l *Listener) reportHead() string {
	owner, machine := "", ""
	if c := l.conf.Load(); c != nil {
		owner, machine = c.Owner, c.Machine
	}
	return "kwak report from " + owner + "'s machine " + render.Sanitize(machine) + ": "
}

// postReport runs one report: join, post, leave. Reports run one at a time, so one report's leave
// never cuts another report's post short. The listener account leaves like an agent (spec 5.17,
// decision 42): it belongs to this pairing alone, and this listener is its only user.
func (l *Listener) postReport(r report) {
	l.repMu.Lock()
	defer l.repMu.Unlock()
	if l.bg.Err() != nil {
		return
	}
	l.mu.Lock()
	if l.phase == wire.StateUnpaired {
		l.mu.Unlock()
		return
	}
	if now := l.d.Clock.Now(); now.Before(l.repNone) {
		l.mu.Unlock()
		l.log.Info("report skipped: no report room until the next hour or a reload")
		return
	}
	user, tok := l.reporterLocked(r)
	standUser, standTok := l.standingUserLocked(), l.standing
	l.mu.Unlock()
	if tok == "" {
		l.log.Info("report not sent: no account to post with")
		return
	}
	ctx, cancel := context.WithTimeout(l.bg, reportWait)
	defer cancel()
	room, err := l.d.Registrar.ReportJoin(ctx, user)
	if _, code, _ := apiStatus(err); code == wire.ErrNotYours && user != standUser && standTok != "" {
		// The agent was closed meanwhile; the pairing's listener account posts instead.
		user, tok = standUser, standTok
		room, err = l.d.Registrar.ReportJoin(ctx, user)
	}
	if err != nil {
		if st, code, _ := apiStatus(err); code == wire.ErrNoReportRoom || st == 404 {
			// No report room for this pairing, or an older registrar without the route (404
			// M_UNRECOGNIZED). Asked again in the next clock hour or after a reload, not for
			// every report.
			l.mu.Lock()
			l.repNone = l.d.Clock.Now().UTC().Truncate(time.Hour).Add(time.Hour)
			l.mu.Unlock()
			l.log.Info("report not sent: the server has no report room for this pairing")
			return
		}
		l.regFailed(err)
		l.log.Info("report not sent: the registrar did not join the account", "user", user, "err", describe(err))
		return
	}
	m := l.d.Matrix(tok)
	_, serr := m.Send(ctx, room, "kwak-report-"+l.randomHex(8),
		message{MsgType: "m.notice", Body: r.text, Mentions: mentions{}})
	// Its own clock: a slow join or post, or a listener that is stopping, must not keep the
	// account in the report room.
	lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), reportLeaveWait)
	lerr := m.Leave(lctx, room)
	lcancel()
	switch {
	case serr != nil:
		l.log.Info("report not sent", "user", user, "room", room, "err", reactErr(serr))
	default:
		l.log.Info("report sent", "user", user, "room", room)
	}
	if lerr != nil {
		l.log.Info("report room not left", "user", user, "room", room, "err", reactErr(lerr))
	}
}

// reporterLocked is the account that posts r: the session's agent (or its helper) while it holds
// a token, as for a reaction (DV-17); otherwise the pairing's listener account. No agent is
// minted or reopened just to report.
func (l *Listener) reporterLocked(r report) (user, tok string) {
	if s := l.st.Sessions[r.key]; s != nil && r.key != "" {
		agent := l.agentOf(s, r.helper)
		if t := l.tokens[agent]; agent != "" && t != "" && !s.AgentClosed {
			return agent, t
		}
	}
	return l.standingUserLocked(), l.standing
}

// standingUserLocked is the account of the standing token, as POST /standing named it: the
// pairing's own listener account (decision 42). It is never config.json's standing_user, which is
// what the claim named and may be out of date (a pairing made before decision 42 that the
// registrar moved, or a name the registrar had to change). It is "" while the listener has no
// standing token.
func (l *Listener) standingUserLocked() string {
	return l.standUser
}

// reportReason is a check's reason as a report line shows it: one line, no control or invisible
// characters, at most reportReasonMax characters, and hidden when the secrets scan stops it. The
// message text, when a report may show it, comes after the line on its own (reportText).
func reportReason(reason string) string {
	if scanText(reason) != "" {
		return "(hidden: the reason itself looks like it holds a secret)"
	}
	s := strings.Join(strings.Fields(render.Sanitize(reason)), " ")
	if utf8.RuneCountInString(s) > reportReasonMax {
		s = string([]rune(s)[:reportReasonMax]) + "..."
	}
	if s == "" {
		s = "none given"
	}
	return s
}

// reportText is what a report shows of the message a check stopped (decision 41): the text as
// kwak shows other agents' text (sanitized, tag-like text escaped, cut to render's caps), except
// after the secrets scan, whose report never holds the text: it would publish the secret.
func reportText(kind, text string) string {
	if kind == "secrets" {
		return "\nThe text is not shown: the secrets scan stopped it, and showing it here would publish the secret."
	}
	return "\nText:\n" + strings.Join(render.Quote(text, render.MaxChars, render.MaxLines), "\n")
}

// matrixTo is the matrix.to link to event in the pairing's agents room (the main or the test
// room), or "" when there is no event or no config.
func (l *Listener) matrixTo(event string) string {
	c := l.conf.Load()
	if c == nil || event == "" || c.RoomID == "" {
		return ""
	}
	return "\nLink: https://matrix.to/#/" + linkPart(c.RoomID) + "/" + linkPart(event) + "?via=" + url.QueryEscape(c.ServerName)
}

// linkPart percent-encodes s as one part of a matrix.to link: it keeps the characters a URL path
// segment allows as they are (so "!room:server" and "$event" stay readable) and encodes the rest,
// "/", "?", "#" and "%" among them, so an id cannot change the link's shape.
func linkPart(s string) string {
	const keep = "-._~!$&'()*+,;=:@"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte(keep, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// heldReport is the report of a message an incoming check held (5.16), linked to that message.
func (l *Listener) heldReport(sender, agent, thread, event, kind, reason, text string) string {
	return l.reportHead() + "the incoming check held a message from " + l.short(sender) + " for " + l.short(agent) +
		" in thread " + thread + " (event " + event + "). Check: " + kind + ". Reason: " + reportReason(reason) + "." +
		l.matrixTo(event) + reportText(kind, text)
}

// refusedReport is the report of a post an outgoing check refused (5.16). A refused reply links
// its thread; a refused send has nothing in the room to link.
func (l *Listener) refusedReport(tool, agent, thread, kind, reason, text string) string {
	where := "in thread " + thread
	if thread == "" {
		where = "as a new thread"
	}
	who := l.short(agent)
	if agent == "" {
		who = "a session with no agent yet"
	}
	return l.reportHead() + "the outgoing check refused a " + tool + " by " + who + " " + where +
		" (not posted). Check: " + kind + ". Reason: " + reportReason(reason) + "." + l.matrixTo(thread) + reportText(kind, text)
}

// hourLimitReportLocked reports the hourly limit on background runs, once in any 60 minutes
// (5.11, decision 38). Call it under l.mu while the limit holds a run.
func (l *Listener) hourLimitReportLocked(limit int, next time.Time) {
	now := l.d.Clock.Now()
	if !l.repLimit.IsZero() && now.Before(l.repLimit.Add(time.Hour)) {
		return
	}
	l.repLimit = now
	l.reportLocked(report{text: l.reportHead() + fmt.Sprintf("background runs reached the hourly limit (%d started in the last 60 minutes). "+
		"More runs wait until %s UTC; no message is dropped.", limit, next.UTC().Format("15:04"))})
}
