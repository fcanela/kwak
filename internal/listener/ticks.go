package listener

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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
	for root, q := range l.st.Questions {
		if now.Before(q.Due) {
			continue
		}
		delete(l.st.Questions, root)
		s := l.st.Sessions[q.Session]
		if s == nil {
			continue
		}
		dest := &s.Inbox
		if h := s.Helpers[q.Helper]; q.Helper != "" && h != nil && h.Bound != "" && !h.Finished {
			dest = &h.Inbox
		}
		dest.add(render.Item{EventID: "kwak-timeout-" + root, ThreadRoot: root, SentAt: now, Reason: render.Notice,
			Body: "No reply in thread " + root + " after " + humanDuration(q.Due.Sub(q.AskedAt)) +
				". Assume the agents you asked are shut down. Carry on without the answer, or ask someone else."})
		l.dirty = true
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
	for root, q := range l.st.Questions {
		if q.Session == key {
			delete(l.st.Questions, root)
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

// resumeArgv is the Claude Code argv of 5.11: the prompt right after -p, then only flags.
func resumeArgv(exe, sessionID string) []string {
	return []string{exe, "-p", resumePrompt, "--resume", sessionID, "--tools", "",
		"--allowedTools", "mcp__plugin_kwak_kwak__send,mcp__plugin_kwak_kwak__reply,mcp__plugin_kwak_kwak__thread"}
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
	max := 2
	if c := l.conf.Load(); c != nil {
		exe, max = c.ResumeExe[wire.HarnessClaude], c.ResumeMax
	}
	hex, err := wire.SessionHex(s.SessionID)
	switch {
	case s.Ended == nil || s.Tools || s.Harness != wire.HarnessClaude || s.waiting() == 0 || err != nil:
		l.mu.Unlock()
		return
	case l.d.Runner == nil || exe == "" || !filepath.IsAbs(exe) || s.Cwd == "":
		l.mu.Unlock()
		l.log.Info("background resume skipped: no resume-exe or cwd", "session", key)
		return
	case l.running[key] || len(l.running) >= max:
		due := l.d.Clock.Now()
		s.ResumeDue = &due
		l.mu.Unlock()
		return
	}
	l.running[key] = true
	l.st.ResumeRuns++
	cwd, sid := s.Cwd, s.SessionID
	l.mu.Unlock()
	done := func() {
		l.mu.Lock()
		delete(l.running, key)
		l.mu.Unlock()
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
	ctx, cancel := context.WithTimeout(l.bg, resumeKill)
	wait, err := l.d.Runner.Start(ctx, cwd, resumeArgv(exe, sid), logf)
	if err != nil {
		cancel()
		logf.Close()
		done()
		l.log.Warn("background resume failed to start", "session", key, "err", err)
		return
	}
	l.log.Info("background resume started", "session", key)
	go func() {
		err := wait()
		cancel()
		logf.Close()
		done()
		l.log.Info("background resume ended", "session", key, "err", err)
	}()
}
