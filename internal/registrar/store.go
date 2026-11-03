package registrar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/fcanela/kwak/internal/atomicfile"
	"github.com/fcanela/kwak/internal/wire"
)

// StateVersion is the state.json version this registrar reads and writes.
const StateVersion = 1

// maxDoneEvents is how many bot-command event ids state.json keeps (spec 3.2).
const maxDoneEvents = 512

// State is the whole state.json (spec 3.2).
type State struct {
	Version    int                 `json:"version"`
	RoomID     string              `json:"room_id"`
	Owners     map[string]*Owner   `json:"owners"`
	DMs        map[string]string   `json:"dms"`
	DMInvited  map[string]bool     `json:"dm_invited_back"`
	Pairings   map[string]*Pairing `json:"pairings"`
	Agents     map[string]*Agent   `json:"agents"`
	DoneEvents []string            `json:"done_events"`
	CommandTS  map[string]int64    `json:"command_ts"` // DM room -> origin_server_ts of its newest state-changing command

	sessions map[sessionKey]string // session map, rebuilt from Agents
	keys     map[string]string     // key_sha256 -> pairing id
}

// Owner is one owner, keyed by owner name.
type Owner struct {
	Human    string        `json:"human"`
	Alias    bool          `json:"alias"`
	Added    time.Time     `json:"added"`
	AddedBy  string        `json:"added_by"`
	Removed  *time.Time    `json:"removed"`
	Settings wire.Settings `json:"settings"`
}

// Pairing is one paired machine of an owner. KeySHA256 is empty until the key is claimed.
type Pairing struct {
	Owner        string               `json:"owner"`
	Machine      string               `json:"machine"`
	OS           string               `json:"os"`
	IP           string               `json:"ip"`
	KeySHA256    string               `json:"key_sha256"`
	Created      time.Time            `json:"created"`
	Expires      time.Time            `json:"expires"`
	Confirmed    *time.Time           `json:"confirmed"`
	Warned       bool                 `json:"warned"`
	Revoked      *time.Time           `json:"revoked"`
	RevokeReason string               `json:"revoke_reason"`
	LogoutDone   bool                 `json:"logout_done"`
	Replaces     string               `json:"replaces"`
	IPs          map[string]time.Time `json:"ips"`
	LastIP       string               `json:"last_ip"`
	LastUsed     time.Time            `json:"last_used,omitzero"`
}

// Agent is one agent account, keyed by localpart. A foreign clash is a closed record with
// CloseReason "clash" and no session, so the ladder skips that name.
type Agent struct {
	Owner        string       `json:"owner"`
	Kind         wire.Kind    `json:"kind"`
	Pairing      string       `json:"pairing,omitempty"`
	Harness      wire.Harness `json:"harness,omitempty"`
	Session      string       `json:"session,omitempty"` // 32 hex
	Helper       string       `json:"helper,omitempty"`
	Parent       string       `json:"parent,omitempty"`
	Created      time.Time    `json:"created"`
	LastActive   time.Time    `json:"last_active"`
	PendingSince *time.Time   `json:"pending_since"`
	MintedIP     string       `json:"minted_ip,omitempty"`
	MintedAt     time.Time    `json:"minted_at,omitzero"`
	Closed       *time.Time   `json:"closed"`
	CloseReason  string       `json:"close_reason"`
	CleanupDone  bool         `json:"cleanup_done"`
}

// Open reports whether the agent is neither closed nor pending.
func (a *Agent) Open() bool { return a.Closed == nil && a.PendingSince == nil }

type sessionKey struct {
	owner   string
	harness wire.Harness
	hex     string
	helper  string
}

func (k sessionKey) String() string {
	return k.owner + "|" + string(k.harness) + "|" + k.hex + "|" + k.helper
}

func newState() *State {
	st := &State{Version: StateVersion}
	st.fill()
	return st
}

func (st *State) fill() {
	if st.Owners == nil {
		st.Owners = map[string]*Owner{}
	}
	if st.DMs == nil {
		st.DMs = map[string]string{}
	}
	if st.DMInvited == nil {
		st.DMInvited = map[string]bool{}
	}
	if st.CommandTS == nil {
		st.CommandTS = map[string]int64{}
	}
	if st.Pairings == nil {
		st.Pairings = map[string]*Pairing{}
	}
	if st.Agents == nil {
		st.Agents = map[string]*Agent{}
	}
	if st.DoneEvents == nil {
		st.DoneEvents = []string{}
	}
}

func (st *State) reindex() {
	st.sessions = make(map[sessionKey]string, len(st.Agents))
	for lp, a := range st.Agents {
		if a.Kind == wire.KindStanding || a.Session == "" {
			continue
		}
		st.sessions[sessionKey{a.Owner, a.Harness, a.Session, a.Helper}] = lp
	}
	st.keys = make(map[string]string, len(st.Pairings))
	for id, p := range st.Pairings {
		if p.KeySHA256 != "" {
			st.keys[p.KeySHA256] = id
		}
	}
}

// Lookup returns the localpart the session map holds for k.
func (st *State) lookup(k sessionKey) (string, *Agent) {
	lp, ok := st.sessions[k]
	if !ok {
		return "", nil
	}
	return lp, st.Agents[lp]
}

func (st *State) putAgent(lp string, a *Agent) {
	st.Agents[lp] = a
	if a.Kind != wire.KindStanding && a.Session != "" {
		st.sessions[sessionKey{a.Owner, a.Harness, a.Session, a.Helper}] = lp
	}
}

func (st *State) pairingByKey(sha string) (string, *Pairing) {
	id, ok := st.keys[sha]
	if !ok {
		return "", nil
	}
	return id, st.Pairings[id]
}

// done reports whether a bot command event already ran.
func (st *State) done(eventID string) bool {
	for _, id := range st.DoneEvents {
		if id == eventID {
			return true
		}
	}
	return false
}

func (st *State) markDone(eventID string) {
	st.DoneEvents = append(st.DoneEvents, eventID)
	if n := len(st.DoneEvents) - maxDoneEvents; n > 0 {
		st.DoneEvents = append([]string(nil), st.DoneEvents[n:]...)
	}
}

// ownerOf returns the owner name whose human is mxid.
func (st *State) ownerOf(mxid string) (string, *Owner) {
	for name, o := range st.Owners {
		if o.Human == mxid {
			return name, o
		}
	}
	return "", nil
}

// Store is state.json behind one mutex. Every Update is written before it returns.
type Store struct {
	path    string
	mu      sync.Mutex
	st      *State
	saved   []byte
	touched map[string]time.Time
	used    map[string]time.Time
}

// OpenStore loads path, or starts empty when it does not exist. A file that does not parse, or
// has a newer version, is an error: the registrar never resets its state.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, touched: map[string]time.Time{}, used: map[string]time.Time{}}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.st = newState()
		s.st.reindex()
		s.saved, err = json.Marshal(s.st)
		return s, err
	case err != nil:
		return nil, err
	}
	st, err := parseState(data)
	if err != nil {
		return nil, fmt.Errorf("registrar: %s: %w", path, err)
	}
	s.st, s.saved = st, data
	return s, nil
}

func parseState(data []byte) (*State, error) {
	st := &State{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(st); err != nil {
		return nil, fmt.Errorf("state does not parse: %w", err)
	}
	if st.Version != StateVersion {
		return nil, fmt.Errorf("state version %d, this registrar knows %d", st.Version, StateVersion)
	}
	st.fill()
	st.reindex()
	return st, nil
}

// Update runs fn on the state and writes the result before it returns. If fn or the write fails,
// the state is rolled back to the last saved file.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyTouches()
	if err := fn(s.st); err != nil {
		s.rollback()
		return err
	}
	if err := s.save(); err != nil {
		s.rollback()
		return err
	}
	return nil
}

// View runs fn on the state. fn must not keep pointers into it.
func (s *Store) View(fn func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.st)
}

// Touch moves an open agent's last_active forward, in memory only. The next Update or Flush saves it.
func (s *Store) Touch(localpart string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.st.Agents[localpart]
	if a == nil || a.Closed != nil || !t.After(a.LastActive) {
		return
	}
	a.LastActive = t
	s.touched[localpart] = t
}

// TouchPairing moves a pairing's last_used forward, in memory only.
func (s *Store) TouchPairing(id string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.st.Pairings[id]
	if p == nil || !t.After(p.LastUsed) {
		return
	}
	p.LastUsed = t
	s.used[id] = t
}

// Flush saves touched activity times.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.touched) == 0 && len(s.used) == 0 {
		return nil
	}
	if err := s.save(); err != nil {
		return err
	}
	return nil
}

func (s *Store) applyTouches() {
	for lp, t := range s.touched {
		if a := s.st.Agents[lp]; a != nil && t.After(a.LastActive) {
			a.LastActive = t
		}
	}
	for id, t := range s.used {
		if p := s.st.Pairings[id]; p != nil && t.After(p.LastUsed) {
			p.LastUsed = t
		}
	}
}

func (s *Store) rollback() {
	st, err := parseState(s.saved)
	if err != nil {
		panic("registrar: saved state does not parse: " + err.Error())
	}
	s.st = st
	s.applyTouches()
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.st, "", " ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeAtomic(s.path, data); err != nil {
		return err
	}
	s.saved = data
	clear(s.touched)
	clear(s.used)
	s.st.reindex()
	return nil
}

func writeAtomic(path string, data []byte) error { return atomicfile.Write(path, data, 0o600) }
