package listener

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fcanela/kwak/internal/atomicfile"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
)

const (
	stateVersion = 1
	stateFile    = "state.json"
	seenFile     = "state.seen" // marks that a state.json existed here, so a lost one shows as a reset
)

// state is state.json (spec 5.2). It never holds a token or a key (I1).
type state struct {
	Version     int                  `json:"version"`
	NextBatch   string               `json:"next_batch"`
	LastEventID string               `json:"last_event_id"`
	LastEventTS int64                `json:"last_event_ts"`
	Settings    *wire.Settings       `json:"settings,omitempty"`
	KeyExpires  time.Time            `json:"key_expires_at,omitzero"`
	MeAt        time.Time            `json:"me_at,omitzero"`
	LastPing    time.Time            `json:"last_ping,omitzero"`
	Sessions    map[string]*session  `json:"sessions"`
	Mine        map[string]Agent     `json:"mine"`
	Roots       map[string]string    `json:"roots"`
	Threads     map[string]Thread    `json:"threads"`
	Questions   map[string]*question `json:"questions"`
	Backfill    *backfill            `json:"backfill,omitempty"`
	ResumeRuns  int                  `json:"resume_runs"`
}

// session is one harness session (spec 5.5).
type session struct {
	Key         string             `json:"key"`
	Harness     wire.Harness       `json:"harness"`
	SessionID   string             `json:"session_id"`
	Cwd         string             `json:"cwd"`
	Candidates  []proc.ID          `json:"candidates"`
	HarnessProc *proc.ID           `json:"harness_proc"`
	Tools       bool               `json:"tools_connected"`
	Claimed     bool               `json:"claimed,omitempty"`
	Agent       string             `json:"agent"`
	AgentClosed bool               `json:"agent_closed,omitempty"`
	NoAgent     string             `json:"no_agent,omitempty"` // a registrar refusal that is never retried
	Helpers     map[string]*helper `json:"helpers"`
	Volunteer   bool               `json:"volunteer"`
	Started     time.Time          `json:"started"`
	Ended       *time.Time         `json:"ended"`
	LastHook    time.Time          `json:"last_hook,omitzero"`
	LastUse     time.Time          `json:"last_use,omitzero"`
	Active      bool               `json:"active_since_ping"`
	Inbox       queue              `json:"inbox"`
	HandedOver  []handover         `json:"handed_over,omitempty"`
	CloseDue    *time.Time         `json:"close_due,omitempty"`
	ResumeDue   *time.Time         `json:"resume_due,omitempty"`
}

type helper struct {
	UserID   string `json:"user_id"`
	Bound    string `json:"bound_agent_id"` // Claude agent_id or Codex thread id
	Finished bool   `json:"finished,omitempty"`
	Inbox    queue  `json:"inbox"`
}

// queue is one agent's inbox: waiting items and the ids already delivered.
type queue struct {
	Items     []render.Item        `json:"items"`
	Delivered map[string]time.Time `json:"delivered"`
}

type handover struct {
	Count     int       `json:"count"`
	ToSession string    `json:"to_session"`
	At        time.Time `json:"at"`
	Shown     bool      `json:"shown,omitempty"`
}

type question struct {
	Asker   string    `json:"asker"`
	Session string    `json:"session"`
	Helper  string    `json:"helper,omitempty"`
	AskedAt time.Time `json:"asked_at"`
	Due     time.Time `json:"due"`
}

// backfill is the progress of a gap fill (spec 5.3).
type backfill struct {
	From      string `json:"from"`
	NextBatch string `json:"next_batch"`
	Done      bool   `json:"done,omitempty"`
	Missed    bool   `json:"missed,omitempty"`
}

func newState() *state {
	s := &state{Version: stateVersion}
	s.init()
	return s
}

func (s *state) init() {
	if s.Sessions == nil {
		s.Sessions = map[string]*session{}
	}
	if s.Mine == nil {
		s.Mine = map[string]Agent{}
	}
	if s.Roots == nil {
		s.Roots = map[string]string{}
	}
	if s.Threads == nil {
		s.Threads = map[string]Thread{}
	}
	if s.Questions == nil {
		s.Questions = map[string]*question{}
	}
	for _, se := range s.Sessions {
		if se.Helpers == nil {
			se.Helpers = map[string]*helper{}
		}
		se.Inbox.init()
		for _, h := range se.Helpers {
			h.Inbox.init()
		}
	}
}

func (q *queue) init() {
	if q.Delivered == nil {
		q.Delivered = map[string]time.Time{}
	}
}

// loadState reads state.json. A file that does not parse is kept as state.json.bad and the
// listener starts fresh; reset is then true, as it is when a state.json existed and is gone.
func loadState(dir string) (s *state, reset bool, err error) {
	path := filepath.Join(dir, stateFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		_, serr := os.Stat(filepath.Join(dir, seenFile))
		return newState(), serr == nil, nil
	}
	if err != nil {
		return nil, false, err
	}
	s = &state{}
	if err := json.Unmarshal(data, s); err != nil {
		if rerr := os.Rename(path, path+".bad"); rerr != nil {
			return nil, false, rerr
		}
		return newState(), true, nil
	}
	if s.Version > stateVersion {
		return nil, false, fmt.Errorf("listener: state.json: %w", ErrConfigNewer)
	}
	s.Version = stateVersion
	s.init()
	return s, false, nil
}

// save writes state.json now. Callers must not hold l.mu.
func (l *Listener) save() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.saveLocked()
}

// saveLocked writes state.json with l.mu held: a temp file, fsync, then rename.
func (l *Listener) saveLocked() error {
	data, err := json.Marshal(l.st)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(l.dir, stateFile), data); err != nil {
		l.dirty = true
		l.log.Error("state.json write failed", "err", err)
		return err
	}
	l.dirty = false
	seen := filepath.Join(l.dir, seenFile)
	if _, err := os.Stat(seen); err != nil {
		os.WriteFile(seen, nil, 0o600)
	}
	return nil
}

func (l *Listener) flushDirty() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dirty {
		l.saveLocked()
	}
}

func writeAtomic(path string, data []byte) error { return atomicfile.Write(path, data, 0o600) }
