package listener

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/wire"
)

// client is one IPC connection.
type client struct {
	c       net.Conn
	wmu     sync.Mutex
	pid     int
	pidOK   bool
	hello   bool
	harness wire.Harness
	kwak    string
	env     string
	h       proc.ID // the MCP server's parent: the harness process
	hOK     bool
}

func (c *client) write(v any) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	ipc.WriteLine(c.c, v)
}

// chain is the caller's ancestry: read by the listener from the peer pid (I3), or, where no peer
// pid exists (Windows), the chain the client sent, marked claimed.
func (l *Listener) chain(c *client, sent []proc.ID) ([]proc.ID, bool) {
	if c.pidOK {
		ch, err := proc.Chain(l.d.Procs, c.pid, chainDepth)
		if err != nil {
			return nil, false
		}
		return ch, false
	}
	return sent, true
}

func (l *Listener) serveConn(nc net.Conn) {
	c := &client{c: nc}
	pid, ok, err := l.d.PeerPID(nc)
	c.pid, c.pidOK = pid, ok && err == nil
	stop := context.AfterFunc(l.bg, func() { nc.Close() })
	var tools sync.WaitGroup
	defer func() {
		stop()
		nc.Close()
		tools.Wait()
		if c.hello {
			l.mcpClosed(c)
		}
	}()
	r := ipc.NewLineReader(nc)
	for {
		line, err := r.Next()
		if err != nil {
			if errors.Is(err, ipc.ErrLineTooLong) {
				c.write(wire.Refuse(wire.IPCErrBadInput))
			}
			return
		}
		h, err := wire.ReadHead(line)
		switch {
		case errors.Is(err, wire.ErrVersion):
			c.write(wire.Refuse(wire.IPCErrVersion))
			continue
		case err != nil:
			c.write(wire.Refuse(wire.IPCErrBadInput))
			continue
		}
		switch h.Op {
		case wire.OpHook:
			var req wire.HookRequest
			if wire.Decode(line, &req) != nil {
				c.write(wire.HookResponse{V: wire.ProtocolVersion, Err: wire.IPCErrBadInput, Kwak: l.ver})
				return
			}
			ch, claimed := l.chain(c, req.Chain)
			c.write(l.hookOp(req, ch, claimed))
			return
		case wire.OpMCPHello:
			var m wire.MCPHello
			if wire.Decode(line, &m) != nil || !wire.ValidHarness(m.Harness) {
				c.write(wire.Refuse(wire.IPCErrBadInput))
				continue
			}
			l.hello(c, m)
			c.write(wire.Ack{V: wire.ProtocolVersion, OK: true, Kwak: l.ver})
		case wire.OpTool:
			var req wire.ToolRequest
			if wire.Decode(line, &req) != nil {
				c.write(wire.ToolResponse{V: wire.ProtocolVersion, Err: wire.IPCErrBadInput})
				continue
			}
			if !c.hello {
				c.write(wire.ToolResponse{V: wire.ProtocolVersion, ID: req.ID, Err: wire.IPCErrBadOp})
				continue
			}
			tools.Add(1)
			go func() {
				defer tools.Done()
				c.write(l.toolOp(c, req))
			}()
		case wire.OpStatus:
			c.write(l.status(c))
		case wire.OpInbox:
			c.write(l.inbox())
		case wire.OpReload:
			c.write(l.reload())
		case wire.OpShutdown:
			c.write(wire.Ack{V: wire.ProtocolVersion, OK: true})
			l.stop()
			return
		default:
			c.write(wire.Refuse(wire.IPCErrBadOp))
		}
	}
}

// hello registers an MCP connection and re-attaches the sessions of its harness process (5.8).
func (l *Listener) hello(c *client, m wire.MCPHello) {
	ch, _ := l.chain(c, m.Chain)
	l.mu.Lock()
	defer l.mu.Unlock()
	if c.hello {
		delete(l.conns, c)
	}
	c.hello, c.harness, c.kwak = true, m.Harness, m.Kwak
	if id, err := wire.ValidSessionID(m.EnvSession); err == nil {
		c.env = id
	}
	c.hOK = len(ch) >= 2
	if c.hOK {
		c.h = ch[1]
	}
	l.conns[c] = struct{}{}
	if !c.hOK {
		return
	}
	now := l.d.Clock.Now()
	for _, s := range l.st.Sessions {
		if s.Harness != c.harness {
			continue
		}
		switch {
		case s.HarnessProc != nil && *s.HarnessProc == c.h:
			if s.Ended != nil && (s.CloseDue == nil || now.After(*s.CloseDue) || l.replacedLocked(s, c.h)) {
				continue
			}
			s.Tools, s.Ended, s.CloseDue = true, nil, nil
		case s.HarnessProc == nil && s.Ended == nil && containsID(s.Candidates, c.h):
			h := c.h
			s.HarnessProc, s.Tools, s.Candidates = &h, true, []proc.ID{h}
		default:
			continue
		}
		l.changedLocked()
	}
}

// replacedLocked reports whether a later live session runs in harness process h, as after /clear or
// a fork. Then s ended for good and a reconnect must not revive it.
func (l *Listener) replacedLocked(s *session, h proc.ID) bool {
	for _, o := range l.st.Sessions {
		if o != s && o.Ended == nil && o.Harness == s.Harness && !o.Started.Before(s.Started) &&
			(o.HarnessProc != nil && *o.HarnessProc == h || o.HarnessProc == nil && containsID(o.Candidates, h)) {
			return true
		}
	}
	return false
}

// mcpClosed ends the sessions of a gone harness, and only marks the others "no tools" (5.8).
func (l *Listener) mcpClosed(c *client) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.conns, c)
	if !c.hOK {
		return
	}
	for o := range l.conns {
		if o.hOK && o.h == c.h {
			return
		}
	}
	alive := proc.Alive(l.d.Procs, c.h)
	now := l.d.Clock.Now()
	for _, s := range l.st.Sessions {
		if s.Ended != nil || s.HarnessProc == nil || *s.HarnessProc != c.h {
			continue
		}
		if alive {
			s.Tools = false
			l.dirty = true
		} else {
			l.endLocked(s, now)
		}
	}
}

func (l *Listener) status(c *client) wire.StatusResponse {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := wire.StatusResponse{
		V: wire.ProtocolVersion, OK: true, Kwak: l.ver, State: l.phase, Detail: l.detail,
		StateReset: l.stateReset, CallerChecks: "peer", ResumeRuns: l.st.ResumeRuns,
		ResumeActive: len(l.running), Sessions: []wire.SessionStatus{},
	}
	if !c.pidOK {
		r.CallerChecks = "claimed"
	}
	if conf := l.conf.Load(); l.phase != wire.StateUnpaired && conf != nil {
		r.Server, r.Owner, r.Machine, r.PairingID = conf.Server, conf.Owner, conf.Machine, conf.PairingID
		r.SyncError = l.syncErr
		if l.st.Settings != nil {
			r.Settings, r.KeyExpiresAt, r.Resume = *l.st.Settings, l.st.KeyExpires, textResumeClaude
		}
		if l.phase == wire.StateSyncing {
			r.LastSync = l.lastSync
		}
	}
	for _, s := range l.sortedSessions() {
		if s.Ended != nil && s.waiting() == 0 {
			continue
		}
		ss := wire.SessionStatus{
			Key: s.Key, Harness: s.Harness, SessionID: s.SessionID, Cwd: s.Cwd, Agent: s.Agent,
			Volunteer: s.Volunteer, Tools: s.Tools, Started: s.Started, LastHook: s.LastHook, Inbox: s.waiting(),
		}
		if s.Ended != nil {
			ss.Ended = *s.Ended
		}
		var names []string
		for n, h := range s.Helpers {
			if h.UserID != "" {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			ss.Helpers = append(ss.Helpers, s.Helpers[n].UserID)
		}
		switch {
		case s.NoAgent != "":
			ss.Note = s.NoAgent
		case s.LastHook.IsZero():
			ss.Note = "no hook seen for session " + s.Key
		}
		r.Sessions = append(r.Sessions, ss)
	}
	return r
}

func (l *Listener) sortedSessions() []*session {
	out := make([]*session, 0, len(l.st.Sessions))
	for _, s := range l.st.Sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Started.Equal(out[j].Started) {
			return out[i].Started.Before(out[j].Started)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func (l *Listener) inbox() wire.InboxResponse {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := wire.InboxResponse{V: wire.ProtocolVersion, OK: true, Sessions: []wire.InboxSession{}}
	for _, s := range l.sortedSessions() {
		n := s.waiting()
		if n == 0 {
			continue
		}
		cmd := "claude --resume " + s.SessionID
		if s.Harness == wire.HarnessCodex {
			cmd = "codex resume " + s.SessionID
		}
		if s.Cwd != "" {
			cmd += " (run it in " + s.Cwd + ")"
		}
		r.Sessions = append(r.Sessions, wire.InboxSession{
			Key: s.Key, Harness: s.Harness, SessionID: s.SessionID, Cwd: s.Cwd, Agent: s.Agent,
			Ended: s.Ended != nil, Items: n, Resume: cmd,
		})
	}
	return r
}
