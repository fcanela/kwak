package listener

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
)

// deadlines fires overdue close graces, question timeouts and resume debounces, and watches for
// the wall clock jumping ahead of the ticker (a wake from sleep, spec 5.8).
func (l *Listener) deadlines() {
	now := l.d.Clock.Now()
	l.mu.Lock()
	jump := !l.lastTick.IsZero() && now.Sub(l.lastTick) > deadlineEvery+jumpSlack
	l.lastTick = now
	var closes, resumes []string
	for k, s := range l.st.Sessions {
		if s.CloseDue != nil && !now.Before(*s.CloseDue) {
			closes = append(closes, k)
		}
		if s.ResumeDue != nil && !now.Before(*s.ResumeDue) {
			resumes = append(resumes, k)
		}
	}
	for i, qs := range []map[string]*question{l.st.Questions, l.st.OpenQs} {
		for root, q := range qs {
			if now.Before(q.Due) {
				continue
			}
			delete(qs, root)
			s := l.st.Sessions[q.Session]
			if s == nil {
				continue
			}
			dest := &s.Inbox
			if h := s.Helpers[q.Helper]; q.Helper != "" && h != nil && h.Bound != "" && !h.Finished {
				dest = &h.Inbox
			}
			dest.add(render.Item{EventID: "kwak-timeout-" + root, ThreadRoot: root, SentAt: now, Reason: render.Notice,
				Body: noReplyText(root, q, i == 1)})
			l.dirty = true
		}
	}
	l.mu.Unlock()
	for _, k := range closes {
		l.closeAgents(k)
	}
	for _, k := range resumes {
		l.startResume(k)
	}
	if jump {
		l.log.Info("wall clock jumped ahead; checking agents")
		go l.alive(pingReconcile)
	}
}

// noReplyText is the question timeout notice (spec 5.9). An open question asked nobody, so
// nobody is to be assumed shut down.
func noReplyText(root string, q *question, open bool) string {
	after := " after " + humanDuration(q.Due.Sub(q.AskedAt)) + "."
	if open {
		return "No reply in thread " + root + after + " Nobody answered your open question. " +
			"Carry on without the answer, or ask an agent directly."
	}
	return "No reply in thread " + root + after + " Assume the agents you asked are shut down. " +
		"Carry on without the answer, or ask someone else."
}

func humanDuration(d time.Duration) string {
	unit := func(n int64, u string) string {
		if n == 1 {
			return "1 " + u
		}
		return strconv.FormatInt(n, 10) + " " + u + "s"
	}
	switch {
	case d%time.Hour == 0:
		return unit(int64(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return unit(int64(d/time.Minute), "minute")
	}
	return d.String()
}

// liveness ends sessions whose harness is gone, or that saw no hook for 14 days without a known
// harness process, and prunes records 90 days after their session ended (spec 5.2, 5.8).
func (l *Listener) liveness() {
	now := l.d.Clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, s := range l.st.Sessions {
		switch {
		case s.Ended == nil && s.HarnessProc != nil && !proc.Alive(l.d.Procs, *s.HarnessProc):
			l.endLocked(s, now)
		case s.Ended == nil && s.HarnessProc == nil && now.Sub(latest(s.Started, s.LastHook)) > noHookEnd:
			l.endLocked(s, now)
		case s.Ended != nil && now.Sub(*s.Ended) > keepEnded && s.CloseDue == nil:
			l.pruneLocked(k, s)
		}
	}
	for root, t := range l.st.Threads {
		if now.Sub(time.UnixMilli(t.TS)) > keepEnded {
			delete(l.st.Threads, root)
		}
	}
	for root, t := range l.st.Turns {
		if now.Sub(t.Last) > keepEnded {
			delete(l.st.Turns, root)
		}
	}
}

func latest(ts ...time.Time) time.Time {
	var m time.Time
	for _, t := range ts {
		if t.After(m) {
			m = t
		}
	}
	return m
}

func (l *Listener) pruneLocked(key string, s *session) {
	gone := map[string]bool{}
	for id, a := range l.st.Mine {
		if a.Session == key {
			gone[id] = true
			delete(l.st.Mine, id)
		}
	}
	for root, a := range l.st.Roots {
		if gone[a] {
			delete(l.st.Roots, root)
		}
	}
	for _, qs := range []map[string]*question{l.st.Questions, l.st.OpenQs} {
		for root, q := range qs {
			if q.Session == key {
				delete(qs, root)
			}
		}
	}
	for fk := range l.shown {
		if strings.HasPrefix(fk, key+"/") {
			delete(l.shown, fk)
		}
	}
	for k := range l.blocked {
		if strings.HasPrefix(k, key+"/") {
			delete(l.blocked, k)
		}
	}
	delete(l.st.Sessions, key)
	l.changedLocked()
}

// hourly refreshes the owner's settings and sends the daily alive ping when due (spec 5.1, 5.8).
func (l *Listener) hourly() {
	l.mu.Lock()
	paired := l.phase != wire.StateUnpaired && l.standing != ""
	due := l.d.Clock.Now().Sub(l.st.LastPing) >= pingEvery
	l.mu.Unlock()
	if !paired {
		return
	}
	if due {
		l.alive(pingDaily)
	}
	l.refreshMe(l.bg)
}

// ---- background resume (spec 5.11) ----

const resumePrompt = "kwak resumed this session in the background because other agents wrote to it. " +
	"kwak shows you their messages. Answer them with the kwak tools if you can, using what you already " +
	"know from this conversation. Your user is not watching this run."

// resumeStrictMCP picks the run's tool fence (spec 5.11). true: only a kwak MCP server, whose tools
// are then named mcp__kwak__*. false is the fallback if a live run refuses those tools: the
// plugin's servers stay and dontAsk alone denies every tool not allowed.
const resumeStrictMCP = true

// resumeMCPConfig is the only MCP server of a strict run: the same command as the plugin's.
const resumeMCPConfig = `{"mcpServers":{"kwak":{"command":"kwak","args":["mcp","--harness","claude"]}}}`

// resumeArgv is the Claude Code argv of 5.11: the prompt right after -p, then only flags. Each
// variadic list (--tools, --allowedTools, --mcp-config) is followed by another flag.
func resumeArgv(exe, sessionID string, strict bool) []string {
	argv := []string{exe, "-p", resumePrompt, "--resume", sessionID, "--tools", ""}
	if strict {
		argv = append(argv, "--allowedTools", "mcp__kwak__send,mcp__kwak__reply,mcp__kwak__thread",
			"--strict-mcp-config", "--mcp-config", resumeMCPConfig)
	} else {
		argv = append(argv, "--allowedTools", "mcp__plugin_kwak_kwak__send,mcp__plugin_kwak_kwak__reply,mcp__plugin_kwak_kwak__thread")
	}
	return append(argv, "--permission-mode", "dontAsk")
}

// resumeTriggerLocked starts the 30 s debounce when an item that the owner's setting covers lands
// for an ended session with no tools connection. ownThread is a reply in a thread this session
// rooted, whatever Route ranked it. Codex sessions are skipped (DV-14).
func (l *Listener) resumeTriggerLocked(s *session, r render.Reason, ownThread bool) {
	if s.Ended == nil || s.Tools || s.Harness != wire.HarnessClaude || s.ResumeDue != nil || l.st.Settings == nil {
		return
	}
	switch l.st.Settings.Resume {
	case wire.ResumeReplies:
		if !ownThread && r != render.YourThread {
			return
		}
	case wire.ResumeAll:
		if !ownThread && r != render.YourThread && r != render.Mention {
			return
		}
	default:
		return
	}
	due := l.d.Clock.Now().Add(resumeDelay)
	s.ResumeDue = &due
}

// resumeRun is one background run (spec 5.11).
type resumeRun struct {
	cancel  context.CancelFunc
	done    chan struct{}   // closed once the run has exited and left l.running
	fed     []fedItem       // the waiting messages that started it
	held    []fedItem       // waiting messages of threads at the loop ceiling, kept from it (5.15)
	counted map[string]bool // the threads it was started for, each counted once
	printed []string        // threads of the messages its hooks printed
	seen    bool            // its own SessionStart arrived
}

// fedItem is a waiting message: the helper it is for ("" for the session's agent) and its thread.
type fedItem struct {
	helper, event, thread string
}

// waitingLocked lists the waiting messages of a session and their threads, in queue order, each
// with the agent whose queue holds it. kwak's own notices are left out.
func waitingLocked(s *session) (items []fedItem, threads []string) {
	seen := map[string]bool{}
	add := func(helper string, q *queue) {
		for _, it := range q.Items {
			if it.Reason == render.Notice || !wire.EventID.MatchString(it.EventID) {
				continue
			}
			t := itemThread(it)
			items = append(items, fedItem{helper, it.EventID, t})
			if !seen[t] {
				seen[t] = true
				threads = append(threads, t)
			}
		}
	}
	add("", &s.Inbox)
	names := make([]string, 0, len(s.Helpers))
	for n := range s.Helpers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		add(n, &s.Helpers[n].Inbox)
	}
	return items, threads
}

func (l *Listener) startResume(key string) {
	l.mu.Lock()
	s := l.st.Sessions[key]
	if s == nil || s.ResumeDue == nil {
		l.mu.Unlock()
		return
	}
	s.ResumeDue = nil
	l.dirty = true
	exe := ""
	max := wire.DefaultResumeMax
	if c := l.conf.Load(); c != nil {
		exe, max = c.ResumeExe[wire.HarnessClaude], c.ResumeMax
	}
	hex, err := wire.SessionHex(s.SessionID)
	waiting, threads := waitingLocked(s)
	retry := func() {
		due := l.d.Clock.Now()
		s.ResumeDue = &due
		l.mu.Unlock()
	}
	switch {
	case s.Ended == nil || s.Tools || s.Harness != wire.HarnessClaude || s.waiting() == 0 || err != nil:
		l.mu.Unlock()
		return
	case l.d.Runner == nil || exe == "" || !filepath.IsAbs(exe) || s.Cwd == "":
		l.mu.Unlock()
		l.log.Info("background resume skipped: no resume-exe or cwd", "session", key)
		return
	case l.running[key] != nil || l.opening[key]:
		// The session's run is still going, or its owner is opening it ("you win").
		retry()
		return
	}
	counted, fed, held := l.splitLocked(waiting, threads)
	if len(counted) == 0 {
		l.pauseLocked(s, threads, held)
		l.mu.Unlock()
		return
	}
	if len(l.running) >= max {
		retry()
		return
	}
	now := l.d.Clock.Now()
	if limit, n := l.resumePerHour(), len(l.hourRunsLocked(now)); n >= limit {
		// Like a full slot: the run waits for the next tick, nothing is dropped (decision 38).
		if !l.hourFull {
			l.hourFull = true
			l.log.Info("background resume waiting: hourly limit", "session", key, "limit", limit, "count", n,
				"next", l.nextHourRunLocked(limit).Format(time.RFC3339))
		}
		l.hourLimitReportLocked(limit, l.nextHourRunLocked(limit))
		retry()
		return
	}
	for _, t := range threads {
		if !counted[t] {
			l.log.Info("background resume paused: loop ceiling", "session", key, "thread", t, "turns", l.turnsLocked(t))
		}
	}
	ctx, cancel := context.WithTimeout(l.bg, resumeKill)
	r := &resumeRun{cancel: cancel, done: make(chan struct{}), fed: fed, held: held, counted: counted}
	l.running[key] = r
	l.st.ResumeRuns++
	l.hourRuns, l.hourFull = append(l.hourRuns, now), false
	cwd, sid := s.Cwd, s.SessionID
	l.mu.Unlock()
	done := func() {
		cancel()
		l.mu.Lock()
		delete(l.running, key)
		l.mu.Unlock()
		close(r.done)
	}
	dir := filepath.Join(l.dir, "resume")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		done()
		return
	}
	logf, err := os.OpenFile(filepath.Join(dir, hex+".log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		done()
		return
	}
	wait, err := l.d.Runner.Start(ctx, cwd, resumeArgv(exe, sid, resumeStrictMCP), logf)
	if err != nil {
		logf.Close()
		done()
		l.log.Warn("background resume failed to start", "session", key, "err", err)
		return
	}
	l.mu.Lock()
	now = l.d.Clock.Now()
	for t := range counted {
		c := l.st.Turns[t]
		if c == nil {
			c = &turns{}
			l.st.Turns[t] = c
		}
		c.N, c.Last = c.N+1, now
	}
	l.dirty = true
	l.mu.Unlock()
	l.log.Info("background resume started", "session", key, "threads", len(counted))
	// Run waits for this on shutdown, so a stopped run's group gets its SIGKILL (5.11).
	l.spawn(func() {
		err := wait()
		logf.Close()
		done()
		l.log.Info("background resume ended", "session", key, "err", err)
	})
}

// resumePerHour is the most background runs this machine starts in any 60 minutes (decision 38).
func (l *Listener) resumePerHour() int {
	if c := l.conf.Load(); c != nil && c.ResumePerHour > 0 {
		return c.ResumePerHour
	}
	return wire.DefaultResumePerHour
}

// hourRunsLocked drops the run starts older than an hour and returns the rest, oldest first. The
// list is in memory only, so a listener restart clears it (5.11).
func (l *Listener) hourRunsLocked(now time.Time) []time.Time {
	i := 0
	for i < len(l.hourRuns) && !now.Before(l.hourRuns[i].Add(time.Hour)) {
		i++
	}
	l.hourRuns = l.hourRuns[i:]
	return l.hourRuns
}

// nextHourRunLocked is when the hourly limit allows the next run (UTC), once enough of the runs
// counted by hourRunsLocked are an hour old. Call it only while the count is at the limit.
func (l *Listener) nextHourRunLocked(limit int) time.Time {
	return l.hourRuns[len(l.hourRuns)-limit].Add(time.Hour).UTC()
}

// ---- you win (spec 5.11) ----

// stopWait bounds how long a SessionStart waits for a stopped run to exit: the runner's grace of
// 5 s, then SIGKILL, inside the hook's 9 s (5.6).
const stopWait = 7 * time.Second

// ownChild reports whether chain passes through this listener: the caller is one of its runs.
func (l *Listener) ownChild(chain []proc.ID) bool {
	for _, id := range chain {
		if id.PID == l.self {
			return true
		}
	}
	return false
}

// stopRunFor stops the background run of a session that its owner opens ("you win"): it cancels
// the run and waits until it has exited, so the SessionStart hook is answered only then. The
// run's own SessionStart comes from a process this listener started, and never stops it. Until
// the hook links the session, l.opening keeps a new run from starting for it; hookOp clears it.
func (l *Listener) stopRunFor(key string, chain []proc.ID) {
	l.mu.Lock()
	r := l.running[key]
	if r == nil || l.ownChild(chain) {
		l.mu.Unlock()
		return
	}
	l.opening[key] = true
	l.mu.Unlock()
	l.log.Info("background resume stopped: its session was opened", "session", key)
	r.cancel()
	select {
	case <-r.done:
	case <-l.d.Clock.After(stopWait):
		l.log.Warn("background resume did not stop in time", "session", key)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.st.Sessions[key]; s != nil && len(r.printed) > 0 {
		now := l.d.Clock.Now()
		s.Inbox.add(render.Item{EventID: "kwak-stopped-" + strconv.FormatInt(now.UnixNano(), 10), SentAt: now,
			Reason: render.Notice, Body: stoppedText(r.printed)})
		l.dirty = true
	}
}

// stoppedText names the threads a stopped run was shown. The owner's session loads the run's
// prompt with no answer after it, so those messages look handled when they may not be.
func stoppedText(threads []string) string {
	return "kwak stopped this session's background run when you opened it. The run had been shown " +
		"messages that may look handled here but may have no answer" + threadsText(threads)
}

// runSeenLocked marks the SessionStart of a session's own run, and gives the ⏳ reactions for the
// messages that started it and the ⏸️ for those it is kept from (spec 5.14, 5.15). The run's
// agent is reopened at this point, so a closed agent can send them.
func (l *Listener) runSeenLocked(key string, chain []proc.ID) []reaction {
	r := l.running[key]
	if r == nil || r.seen || !l.ownChild(chain) {
		return nil
	}
	r.seen = true
	rs := make([]reaction, 0, len(r.fed)+len(r.held))
	for _, f := range r.fed {
		rs = append(rs, reaction{key, f.helper, f.event, reactRun})
	}
	return append(rs, l.pausedReactionsLocked(key, r.held)...)
}

// ---- loop ceiling (spec 5.15) ----

func (l *Listener) ceiling() int {
	if c := l.conf.Load(); c != nil && c.ThreadCeiling > 0 {
		return c.ThreadCeiling
	}
	return wire.DefaultThreadCeiling
}

// splitLocked splits the waiting messages of a session by the loop ceiling: the threads under it
// (a run is started for them and counts once for each), their messages, and the messages of
// threads at the ceiling, which no run is started for or shown.
func (l *Listener) splitLocked(waiting []fedItem, threads []string) (counted map[string]bool, fed, held []fedItem) {
	max := l.ceiling()
	counted = map[string]bool{}
	for _, t := range threads {
		if l.turnsLocked(t) < max {
			counted[t] = true
		}
	}
	for _, f := range waiting {
		if counted[f.thread] {
			fed = append(fed, f)
		} else {
			held = append(held, f)
		}
	}
	return counted, fed, held
}

// pauseLocked is a session whose every waiting thread reached the ceiling: no run starts, the
// messages stay held, each gets one ⏸️ and each thread a log line.
func (l *Listener) pauseLocked(s *session, threads []string, held []fedItem) {
	for _, t := range threads {
		l.log.Info("background resume paused: loop ceiling", "session", s.Key, "thread", t, "turns", l.turnsLocked(t))
	}
	l.react(l.pausedReactionsLocked(s.Key, held))
}

// pausedReactionsLocked gives ⏸️ once per message.
func (l *Listener) pausedReactionsLocked(key string, held []fedItem) []reaction {
	var rs []reaction
	for _, f := range held {
		if !l.pausedOn[f.event] {
			l.pausedOn[f.event] = true
			rs = append(rs, reaction{key, f.helper, f.event, reactPaused})
		}
	}
	return rs
}

// heldFromRunLocked reports whether a running background run of s must not be shown it: a message
// of a thread at the ceiling that the run was not started for, also one that came in during the
// run. It stays held for the session's owner (5.15).
func (l *Listener) heldFromRunLocked(s *session, it render.Item) bool {
	r := l.running[s.Key]
	if r == nil || it.Reason == render.Notice || !wire.EventID.MatchString(it.EventID) {
		return false
	}
	t := itemThread(it)
	return !r.counted[t] && l.turnsLocked(t) >= l.ceiling()
}

// resetTriggerLocked applies the resume trigger again to the messages held in thread root, after
// a person's message reset its count: a paused session may run again (5.15).
func (l *Listener) resetTriggerLocked(root string) {
	owner := ""
	if a := l.st.Roots[root]; a != "" {
		owner = l.st.Mine[a].Session
	}
	for _, s := range l.st.Sessions {
		if s.Ended == nil || s.ResumeDue != nil {
			continue
		}
		qs := []*queue{&s.Inbox}
		for _, h := range s.Helpers {
			qs = append(qs, &h.Inbox)
		}
		for _, q := range qs {
			for _, it := range q.Items {
				if it.Reason != render.Notice && itemThread(it) == root {
					l.resumeTriggerLocked(s, it.Reason, it.OwnThread || owner == s.Key)
				}
			}
		}
	}
}

func (l *Listener) turnsLocked(thread string) int {
	if c := l.st.Turns[thread]; c != nil {
		return c.N
	}
	return 0
}

// person reports whether sender is a human: not an agent account and not the registrar bot.
func (l *Listener) person(sender string) bool {
	lp, server, _ := strings.Cut(strings.TrimPrefix(sender, "@"), ":")
	if strings.HasPrefix(strings.ToLower(lp), "agent.") {
		return false
	}
	return lp != wire.BotLocalpart || server != l.serverName()
}
