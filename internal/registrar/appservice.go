package registrar

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
)

// MaxTransaction is the largest transaction body the registrar reads.
const MaxTransaction = 32 << 20

// Appservice is the handler tuwunel calls, /_matrix/app/v1/... (spec 3.3).
func (s *Server) Appservice() http.Handler {
	s.init()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /_matrix/app/v1/transactions/{txn}", s.handleTxn)
	mux.HandleFunc("POST /_matrix/app/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, struct{}{})
	})
	notFound := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]string{"errcode": matrix.ErrNotFound})
	}
	mux.HandleFunc("GET /_matrix/app/v1/users/{id}", notFound)
	mux.HandleFunc("GET /_matrix/app/v1/rooms/{alias}", notFound)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]string{"errcode": "M_UNRECOGNIZED"})
	})
	return s.hsAuth(mux)
}

// hsAuth checks the hs_token in the Bearer header, ?access_token=, or both; when both are sent,
// both must match.
func (s *Server) hsAuth(next http.Handler) http.Handler {
	want := []byte(s.Cfg.HSToken)
	match := func(v string) bool { return len(want) > 0 && subtle.ConstantTimeCompare([]byte(v), want) == 1 }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, hasH := bearer(r)
		q := r.URL.Query()
		_, hasQ := q["access_token"]
		ok := hasH || hasQ
		if hasH && !match(h) {
			ok = false
		}
		if hasQ && !match(q.Get("access_token")) {
			ok = false
		}
		if !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"errcode": matrix.ErrForbidden})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// seenTxn records txn and reports whether it was already seen.
func (s *Server) seenTxn(txn string) bool {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	if s.txns[txn] {
		return true
	}
	s.txns[txn] = true
	s.txnOrder = append(s.txnOrder, txn)
	if len(s.txnOrder) > maxTxns {
		delete(s.txns, s.txnOrder[0])
		s.txnOrder = s.txnOrder[1:]
	}
	return false
}

func (s *Server) handleTxn(w http.ResponseWriter, r *http.Request) {
	txn := r.PathValue("txn")
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxTransaction))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"errcode": "M_TOO_LARGE"})
		return
	}
	if s.seenTxn(txn) {
		writeJSON(w, 200, struct{}{})
		return
	}
	var body struct {
		Events []matrix.Event `json:"events"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		s.Log.Warn("transaction does not parse", "txn", txn, "err", err)
		writeJSON(w, 200, struct{}{})
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer cancel()
	for _, ev := range body.Events {
		s.handleEvent(ctx, ev)
	}
	writeJSON(w, 200, struct{}{})
}

type memberContent struct {
	Membership  string  `json:"membership"`
	Displayname *string `json:"displayname"`
	AvatarURL   string  `json:"avatar_url"`
}

type textContent struct {
	MsgType string `json:"msgtype"`
	Body    string `json:"body"`
}

func (s *Server) handleEvent(ctx context.Context, ev matrix.Event) {
	sender := ev.Sender
	lp, local := localpart(sender, s.Cfg.ServerName)
	if local && strings.HasPrefix(lp, "agent.") {
		s.agentEvent(ctx, lp, ev)
		return
	}
	if sender == s.Cfg.Bot() {
		return
	}
	switch ev.Type {
	case "m.room.member":
		var m memberContent
		if json.Unmarshal(ev.Content, &m) != nil || ev.StateKey == nil {
			return
		}
		switch {
		case *ev.StateKey == s.Cfg.Bot() && m.Membership == "invite":
			s.invited(ctx, sender, ev.RoomID)
		case *ev.StateKey == sender && m.Membership == "leave":
			s.leftDM(ctx, sender, ev.RoomID)
		}
	case "m.room.message":
		var c textContent
		if json.Unmarshal(ev.Content, &c) != nil || c.MsgType != "m.text" {
			return
		}
		s.command(ctx, ev, c.Body)
	}
}

// agentEvent keeps the idle clock and resets a changed display name or avatar (spec 3.3 step 1).
func (s *Server) agentEvent(ctx context.Context, lp string, ev matrix.Event) {
	open := false
	s.Store.View(func(st *State) {
		a := st.Agents[lp]
		open = a != nil && a.Closed == nil
	})
	if !open {
		return
	}
	if ev.TS > 0 {
		s.Store.Touch(lp, time.UnixMilli(ev.TS).UTC())
	}
	if ev.Type != "m.room.member" || ev.StateKey == nil || *ev.StateKey != ev.Sender {
		return
	}
	var m memberContent
	if json.Unmarshal(ev.Content, &m) != nil || m.Membership != "join" {
		return
	}
	if (m.Displayname == nil || *m.Displayname == lp) && m.AvatarURL == "" {
		return
	}
	s.Log.Info("agent profile reset", "agent", lp)
	if err := s.HS.SetDisplayName(ctx, ev.Sender, lp); err != nil {
		s.Log.Warn("display name reset", "agent", lp, "err", err)
	}
	if err := s.HS.SetAvatarURL(ctx, ev.Sender, ""); err != nil {
		s.Log.Warn("avatar reset", "agent", lp, "err", err)
	}
}

// role is what the registrar knows of a human.
type role struct {
	admin bool
	owner string // "" unless an owner that is not removed
	known bool   // admin, owner, or removed owner
	dm    string
}

func (s *Server) roleOf(mxid string) role {
	var r role
	for _, a := range s.Cfg.Admins {
		if a == mxid {
			r.admin = true
		}
	}
	s.Store.View(func(st *State) {
		name, o := st.ownerOf(mxid)
		if o != nil && o.Removed == nil {
			r.owner = name
		}
		r.known = r.admin || o != nil
		r.dm = st.DMs[mxid]
	})
	return r
}

// invited handles an invite of @registrar (spec 3.3 step 2). An invite to a configured side room
// (an invite-only report room, for example) is what its join was waiting for: it is joined as a
// side room at once and never taken for a DM.
func (s *Server) invited(ctx context.Context, inviter, room string) {
	r := s.roleOf(inviter)
	if !r.admin && r.owner == "" {
		return
	}
	s.joinSideRooms(ctx, true)
	if s.knownRoom(room) || r.dm != "" {
		return
	}
	if err := s.HS.JoinAs(ctx, "", room); err != nil {
		s.Log.Warn("join invited room", "room", room, "err", err)
		return
	}
	members, enc, err := s.HS.RoomInfo(ctx, room)
	if err != nil {
		s.Log.Warn("room info", "room", room, "err", err)
		return
	}
	if enc || members > 2 {
		s.notice(ctx, room, "I cannot read encrypted or group chats. I opened a plain chat with you instead.")
		if err := s.HS.LeaveAs(ctx, "", room); err != nil {
			s.Log.Warn("leave room", "room", room, "err", err)
		}
		if _, err := s.openDM(ctx, inviter, ""); err != nil {
			s.Log.Warn("open DM", "human", inviter, "err", err)
		}
		return
	}
	s.Store.Update(func(st *State) error {
		if _, ok := st.DMs[inviter]; !ok {
			st.DMs[inviter] = room
		}
		return nil
	})
}

// leftDM invites a human back to their DM once, then opens a new one (spec 3.3 step 2b).
func (s *Server) leftDM(ctx context.Context, human, room string) {
	var recorded, invited bool
	s.Store.View(func(st *State) {
		recorded = st.DMs[human] == room
		invited = st.DMInvited[human]
	})
	if !recorded || !s.roleOf(human).known {
		return
	}
	if !invited {
		err := s.HS.Invite(ctx, room, human)
		if err == nil {
			s.Store.Update(func(st *State) error { st.DMInvited[human] = true; return nil })
			return
		}
		s.Log.Warn("invite back", "human", human, "err", err)
	}
	if _, err := s.openDM(ctx, human, room); err != nil {
		s.Log.Warn("open DM", "human", human, "err", err)
	}
}

// maxNotice keeps one bot notice far below the homeserver's 64 KiB event limit.
const maxNotice = 24000

// reply posts a bot reply in room, split at line ends into notices of at most maxNotice bytes.
func (s *Server) reply(ctx context.Context, room, text string) {
	for _, part := range splitLines(text, maxNotice) {
		s.notice(ctx, room, part)
	}
}

func splitLines(text string, max int) []string {
	var out []string
	var b strings.Builder
	for _, l := range strings.Split(text, "\n") {
		if b.Len() > 0 && b.Len()+1+len(l) > max {
			out = append(out, b.String())
			b.Reset()
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	return append(out, b.String())
}

// fetchMatches re-reads an event from tuwunel: a command runs only if sender and body match, so
// a leaked hs_token cannot invent one (spec 3.3 step 3). It returns tuwunel's copy, nil on a
// mismatch, or the error of the re-read.
func (s *Server) fetchMatches(ctx context.Context, ev matrix.Event, body string) (*matrix.Event, error) {
	got, err := s.HS.FetchEvent(ctx, ev.RoomID, ev.EventID)
	if err != nil {
		s.Log.Warn("fetch event", "event", ev.EventID, "err", err)
		return nil, err
	}
	var c textContent
	if got.Sender != ev.Sender || json.Unmarshal(got.Content, &c) != nil || c.MsgType != "m.text" || c.Body != body {
		s.Log.Warn("event does not match tuwunel's copy; ignored", "event", ev.EventID, "sender", ev.Sender)
		return nil, nil
	}
	return got, nil
}
