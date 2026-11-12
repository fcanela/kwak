package listener

import (
	"encoding/json"
	"sort"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
)

// Agent says which local session, and which of its helpers, an agent account belongs to.
type Agent struct {
	Session string `json:"session"`
	Helper  string `json:"helper,omitempty"`
}

// Thread is the latest event the listener saw in one thread.
type Thread struct {
	Latest string `json:"latest"`
	TS     int64  `json:"ts"`
}

// Index is what Route reads and records (spec 5.4).
type Index struct {
	Room       string
	Mine       map[string]Agent  // user id -> owner, every agent this listener created
	Roots      map[string]string // event id -> agent, for thread roots sent by Mine
	Threads    map[string]Thread // root -> latest event
	Volunteers map[string]string // session key -> its agent's user id
	Incomplete bool              // Roots may miss entries (state reset)
	Missing    []string          // thread roots Route could not attribute while Incomplete
	fetched    map[string]bool
}

func newIndex(st *state, room string) *Index {
	return &Index{Room: room, Mine: st.Mine, Roots: st.Roots, Threads: st.Threads,
		Volunteers: map[string]string{}, fetched: map[string]bool{}}
}

// Delivery is one message for one local agent.
type Delivery struct {
	UserID  string        `json:"user_id"`
	Session string        `json:"session"`
	Helper  string        `json:"helper,omitempty"`
	Reason  render.Reason `json:"reason"`
}

const (
	relThread  = "m.thread"
	relReplace = "m.replace"
)

type content struct {
	msgtype  string
	body     string
	mentions []string
	hasRel   bool
	relType  string
	relEvent string
}

// parseContent reads the fields Route needs. A field of the wrong type counts as absent; a body
// that is not a string drops the event.
func parseContent(raw json.RawMessage) (content, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return content{}, false
	}
	var c content
	json.Unmarshal(m["msgtype"], &c.msgtype)
	if json.Unmarshal(m["body"], &c.body) != nil {
		return content{}, false
	}
	var men struct {
		UserIDs []string `json:"user_ids"`
	}
	if json.Unmarshal(m["m.mentions"], &men) == nil {
		c.mentions = men.UserIDs
	}
	if r, ok := m["m.relates_to"]; ok && string(r) != "null" {
		var rel struct {
			RelType string `json:"rel_type"`
			EventID string `json:"event_id"`
		}
		if json.Unmarshal(r, &rel) == nil {
			c.hasRel = true
			c.relType = rel.RelType
			if wire.EventID.MatchString(rel.EventID) {
				c.relEvent = rel.EventID
			}
		}
	}
	return c, true
}

func prio(r render.Reason) int {
	switch r {
	case render.Mention:
		return 3
	case render.YourThread:
		return 2
	case render.OpenQuestion:
		return 1
	}
	return 0
}

// Route decides which local agents an event is addressed to. It does no I/O. It records thread
// roots sent by local agents and the latest event of each thread in idx.
func Route(ev matrix.Event, idx *Index) []Delivery {
	if ev.RoomID != idx.Room || ev.Type != "m.room.message" {
		return nil
	}
	c, ok := parseContent(ev.Content)
	if !ok || (c.msgtype != "m.text" && c.msgtype != "m.notice") || c.relType == relReplace {
		return nil
	}
	if _, mine := idx.Mine[ev.Sender]; mine && !c.hasRel {
		idx.Roots[ev.EventID] = ev.Sender
	}
	targets := map[string]render.Reason{}
	add := func(u string, r render.Reason) {
		if prev, ok := targets[u]; !ok || prio(r) > prio(prev) {
			targets[u] = r
		}
	}
	for _, u := range c.mentions {
		if _, ok := idx.Mine[u]; ok {
			add(u, render.Mention)
		}
	}
	switch {
	case c.relType == relThread && c.relEvent != "":
		if t, ok := idx.Threads[c.relEvent]; !ok || ev.TS >= t.TS {
			idx.Threads[c.relEvent] = Thread{Latest: ev.EventID, TS: ev.TS}
		}
		if a, ok := idx.Roots[c.relEvent]; ok {
			add(a, render.YourThread)
		} else if idx.Incomplete && !idx.fetched[c.relEvent] {
			if idx.fetched == nil {
				idx.fetched = map[string]bool{}
			}
			idx.fetched[c.relEvent] = true
			idx.Missing = append(idx.Missing, c.relEvent)
		}
	case !c.hasRel && len(c.mentions) == 0:
		for _, a := range idx.Volunteers {
			add(a, render.OpenQuestion)
		}
	}
	delete(targets, ev.Sender)
	out := make([]Delivery, 0, len(targets))
	for u, r := range targets {
		a := idx.Mine[u]
		out = append(out, Delivery{UserID: u, Session: a.Session, Helper: a.Helper, Reason: r})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out
}
