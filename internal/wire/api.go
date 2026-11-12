// Package wire holds the data shared by both binaries: the registrar API, the IPC protocol, the
// appservice registration file and the agent name rules. It does no I/O.
package wire

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/fcanela/kwak/internal/proc"
)

// Version is the kwak release. IPC messages carry it as "kwak".
const Version = "0.1.0"

// Harness names a supported agent harness.
type Harness string

const (
	HarnessClaude Harness = "claude"
	HarnessCodex  Harness = "codex"
)

// ---- Registrar API (spec 3.4) ----

// APIPrefix is the path prefix of the registrar's listener API.
const APIPrefix = "/_kwak/v1"

// Route paths, relative to APIPrefix.
const (
	PathPair        = "/pair"
	PathPairClaim   = "/pair/claim"
	PathStanding    = "/standing"
	PathAgents      = "/agents"
	PathAgentsClose = "/agents/close"
	PathAgentsAlive = "/agents/alive"
	PathUnpair      = "/unpair"
	PathMe          = "/me"
	PathReportJoin  = "/report/join"
)

// MaxBody is the largest API request body the registrar reads.
const MaxBody = 64 << 10

// MaxIDs is the most user ids one close or alive request may carry.
const MaxIDs = 500

// Errcodes of the registrar API.
const (
	ErrBadKey         = "KWAK_BAD_KEY"         // 401: unknown, expired or revoked key
	ErrOwnerRemoved   = "KWAK_OWNER_REMOVED"   // 403
	ErrNotYours       = "KWAK_NOT_YOURS"       // 403: the agent belongs to another pairing
	ErrParentClosed   = "KWAK_PARENT_CLOSED"   // 403: helper asked for a closed parent
	ErrBadSession     = "KWAK_BAD_SESSION"     // 400: session id fails ValidSessionID
	ErrBadRequest     = "KWAK_BAD_REQUEST"     // 400: body does not decode, or too many ids
	ErrNamesExhausted = "KWAK_NAMES_EXHAUSTED" // 409
	ErrHomeserver     = "KWAK_HOMESERVER"      // 502: Error carries the tuwunel errcode
	ErrExpired        = "KWAK_EXPIRED"         // 410: claim expired or already collected
	ErrNoReportRoom   = "KWAK_NO_REPORT_ROOM"  // 404: the server has no report room for this pairing's kind
)

// Errcodes lists every registrar API errcode.
var Errcodes = []string{
	ErrBadKey, ErrOwnerRemoved, ErrNotYours, ErrParentClosed, ErrBadSession, ErrBadRequest,
	ErrNamesExhausted, ErrHomeserver, ErrExpired, ErrNoReportRoom,
}

// StatusFor returns the HTTP status that goes with an errcode, or 0 for an unknown one.
func StatusFor(errcode string) int {
	switch errcode {
	case ErrBadSession, ErrBadRequest:
		return 400
	case ErrBadKey:
		return 401
	case ErrOwnerRemoved, ErrNotYours, ErrParentClosed:
		return 403
	case ErrNoReportRoom:
		return 404
	case ErrNamesExhausted:
		return 409
	case ErrExpired:
		return 410
	case ErrHomeserver:
		return 502
	}
	return 0
}

// APIError is the body of every registrar API error.
type APIError struct {
	Status  int    `json:"-"`
	Errcode string `json:"errcode"`
	Message string `json:"error"`
}

func (e *APIError) Error() string { return e.Errcode + ": " + e.Message }

// Empty is the body of routes that take or return {}.
type Empty struct{}

// PairRequest is the body of POST /pair.
type PairRequest struct {
	Machine string `json:"machine"`
	OS      string `json:"os"`
	Version string `json:"version"`
}

// PairResponse is the 201 body of POST /pair.
type PairResponse struct {
	Code      string    `json:"code"`
	Claim     string    `json:"claim"`
	ExpiresAt time.Time `json:"expires_at"`
	Bot       string    `json:"bot"`
	Replaces  bool      `json:"replaces"`
}

// Claim statuses.
const (
	ClaimPending = "pending"
	ClaimPaired  = "paired"
)

// ClaimResponse is the 202 or 200 body of POST /pair/claim. Only Status is set while pending.
type ClaimResponse struct {
	Status       string    `json:"status"`
	Owner        string    `json:"owner,omitempty"`
	PairingID    string    `json:"pairing_id,omitempty"`
	OwnerKey     string    `json:"owner_key,omitempty"`
	KeyExpiresAt time.Time `json:"key_expires_at,omitzero"`
	RoomID       string    `json:"room_id,omitempty"`
	StandingUser string    `json:"standing_user,omitempty"`
	ServerName   string    `json:"server_name,omitempty"`
	// Test is set for a test pairing (decision 40): RoomID is then the test room and StandingUser
	// the test standing account. Absent for a normal pairing.
	Test bool `json:"test,omitempty"`
}

// Standing is the 200 body of POST /standing.
type Standing struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
}

// AgentRequest is the body of POST /agents. Helper is empty for a session agent.
type AgentRequest struct {
	RequestID string  `json:"request_id"`
	Harness   Harness `json:"harness"`
	Session   string  `json:"session"`
	Helper    string  `json:"helper"`
}

// Agent is the 200 body of POST /agents.
type Agent struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
	RoomID      string `json:"room_id"`
	New         bool   `json:"new"`
	Reopened    bool   `json:"reopened"`
}

// CloseRequest is the body of POST /agents/close.
type CloseRequest struct {
	UserIDs []string `json:"user_ids"`
	Reason  string   `json:"reason"`
}

// ReasonSessionEnded is the close reason the listener sends when a session's grace ran out.
const ReasonSessionEnded = "session_ended"

// CloseResponse is the 200 body of POST /agents/close.
type CloseResponse struct {
	Closed []string `json:"closed"`
}

// AliveRequest is the body of POST /agents/alive.
type AliveRequest struct {
	UserIDs []string `json:"user_ids"`
	Touch   bool     `json:"touch,omitempty"` // count as activity: set last_active of the open ones
}

// AliveResponse is the 200 body of POST /agents/alive.
type AliveResponse struct {
	Unknown []string `json:"unknown"`
	Closed  []string `json:"closed"`
}

// Settings are an owner's settings, changed only through the bot DM. Background resume is not one
// of them since decision 44: it is set on each machine (MachineConfig.Resume, spec 5.11), and
// GET /me no longer sends "resume".
type Settings struct {
	QuestionTimeout string `json:"question_timeout"` // Go duration string, for example "15m"
}

// Me is the 200 body of GET /me.
type Me struct {
	Owner        string    `json:"owner"`
	PairingID    string    `json:"pairing_id"`
	Machine      string    `json:"machine"`
	KeyExpiresAt time.Time `json:"key_expires_at"`
	AgentsOpen   int       `json:"agents_open"`
	Settings     Settings  `json:"settings"`
	Test         bool      `json:"test,omitempty"` // a test pairing (decision 40); absent for a normal one
}

// ReportJoinRequest is the body of POST /report/join. UserID is one of the caller's own accounts:
// an open session or helper agent of this pairing, or this pairing's own listener account
// (decision 42).
type ReportJoinRequest struct {
	UserID string `json:"user_id"`
}

// ReportJoin is the 200 body of POST /report/join: the report room the account is now a member
// of. The listener posts one line there with that account's own token, then leaves with it; the
// pairing's listener account leaves too (spec 5.17, decision 42).
type ReportJoin struct {
	RoomID string `json:"room_id"`
}

// ---- Local IPC (spec 5.6) ----

// ProtocolVersion is the "v" of every IPC message.
const ProtocolVersion = 1

// IPC ops.
const (
	OpHook     = "hook"
	OpMCPHello = "mcp_hello"
	OpTool     = "tool"
	OpStatus   = "status"
	OpInbox    = "inbox"
	OpReload   = "reload"
	OpShutdown = "shutdown"
)

// Hook events, as named on the kwak hook command line.
const (
	EventSessionStart = "session-start"
	EventPrompt       = "prompt"
	EventPostTool     = "post-tool"
	EventStop         = "stop"
	EventSubagentStop = "subagent-stop"
	EventSessionEnd   = "session-end"
)

// IPC error values in the "err" field.
const (
	IPCErrVersion  = "version"   // "v" greater than ProtocolVersion
	IPCErrBadOp    = "bad_op"    // unknown op
	IPCErrBadInput = "bad_input" // a field failed the input checks
	IPCErrUnpaired = "unpaired"  // the listener is not paired
	IPCErrInternal = "internal"
)

// Head is the part every IPC request shares. The listener decodes it first to pick the op.
type Head struct {
	V  int    `json:"v"`
	Op string `json:"op"`
}

// HookRequest is sent by kwak hook. Chain is set only on Windows.
type HookRequest struct {
	V              int             `json:"v"`
	Kwak           string          `json:"kwak"`
	Op             string          `json:"op"`
	Harness        Harness         `json:"harness"`
	Event          string          `json:"event"`
	SessionID      string          `json:"session_id"`
	AgentID        string          `json:"agent_id"`
	AgentType      string          `json:"agent_type"`
	Source         string          `json:"source"`
	Cwd            string          `json:"cwd"`
	StopHookActive bool            `json:"stop_hook_active"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	Chain          []proc.ID       `json:"chain"`
}

// HookResponse is the listener's answer to a hook. Text is the rendered delivery, or on a Codex
// stop the fixed notice. More counts items left waiting.
type HookResponse struct {
	V    int    `json:"v"`
	OK   bool   `json:"ok"`
	Err  string `json:"err,omitempty"`
	Kwak string `json:"kwak,omitempty"`
	Text string `json:"text"`
	More int    `json:"more"`
}

// MCPHello is the first line of an MCP server's connection.
type MCPHello struct {
	V          int       `json:"v"`
	Kwak       string    `json:"kwak"`
	Op         string    `json:"op"`
	Harness    Harness   `json:"harness"`
	EnvSession string    `json:"env_session"`
	Chain      []proc.ID `json:"chain"`
}

// Ack answers mcp_hello and shutdown, and any request refused before its op ran.
type Ack struct {
	V    int    `json:"v"`
	OK   bool   `json:"ok"`
	Err  string `json:"err,omitempty"`
	Kwak string `json:"kwak,omitempty"`
}

// ToolRequest is one tools/call over the MCP server's connection.
type ToolRequest struct {
	V           int             `json:"v"`
	Op          string          `json:"op"`
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Args        json.RawMessage `json:"args"`
	MetaSession string          `json:"meta_session"`
	MetaThread  string          `json:"meta_thread"`
}

// ToolResponse answers the ToolRequest with the same ID.
type ToolResponse struct {
	V       int    `json:"v"`
	ID      int64  `json:"id"`
	OK      bool   `json:"ok"`
	Err     string `json:"err,omitempty"`
	Text    string `json:"text"`
	IsError bool   `json:"is_error"`
}

// Listener states in StatusResponse.State.
const (
	StateUnpaired       = "unpaired"        // no config, no key, or the pairing was revoked
	StateWaitingKeyring = "waiting_keyring" // keyring locked or slow
	StateConnecting     = "connecting"      // paired, sync not running yet
	StateSyncing        = "syncing"         // sync loop running
)

// StatusResponse answers the status op (kwak status, and kwak setup's wait).
type StatusResponse struct {
	V         int    `json:"v"`
	OK        bool   `json:"ok"`
	Err       string `json:"err,omitempty"`
	Kwak      string `json:"kwak"`
	State     string `json:"state"`
	Detail    string `json:"detail,omitempty"` // human text for State, for example why unpaired
	Server    string `json:"server,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Machine   string `json:"machine,omitempty"`
	PairingID string `json:"pairing_id,omitempty"`
	Test      bool   `json:"test,omitempty"` // a test pairing (decision 40)
	// ListenerAccount is the user id the listener syncs as, from its last POST /standing: the
	// pairing's own listener account (decision 42). Empty until that call works. Added 2026-11-12.
	ListenerAccount string         `json:"listener_account,omitempty"`
	KeyExpiresAt    time.Time      `json:"key_expires_at,omitzero"`
	Settings        Settings       `json:"settings,omitzero"`
	LastSync        time.Time      `json:"last_sync,omitzero"`
	SyncError       string         `json:"sync_error,omitempty"`
	StateReset      bool           `json:"state_reset,omitempty"`
	CallerChecks    string         `json:"caller_checks"`    // "peer" or "claimed"
	Resume          string         `json:"resume,omitempty"` // this machine's background resume triggers: "off" or for example "replies, mentions"
	ResumeRuns      int            `json:"resume_runs"`
	ResumeActive    int            `json:"resume_active"`
	Paused          []PausedThread `json:"paused,omitempty"` // threads at the loop ceiling
	// While the hourly limit on background runs holds (decision 38): the limit, and when (UTC) the
	// next run may start.
	ResumeHourLimit int       `json:"resume_hour_limit,omitempty"`
	ResumeNextAt    time.Time `json:"resume_next_at,omitzero"`
	// The message checks (spec 5.16): the check commands ("" is off), the messages waiting for the
	// incoming check, and the latest messages a check stopped, oldest first.
	CheckOutgoing string          `json:"check_outgoing,omitempty"`
	CheckIncoming string          `json:"check_incoming,omitempty"`
	CheckWaiting  int             `json:"check_waiting,omitempty"`
	Rejected      []Rejection     `json:"rejected,omitempty"`
	Sessions      []SessionStatus `json:"sessions"`
}

// Rejection is one message a check stopped (spec 5.16). Way is "outgoing" (a post by a local agent
// that kwak did not send) or "incoming" (a message held from a local session). Check is "secrets"
// or "command". Reason never holds a secret.
type Rejection struct {
	At      time.Time `json:"at"`
	Way     string    `json:"way"`
	Session string    `json:"session"`
	Agent   string    `json:"agent,omitempty"`
	Sender  string    `json:"sender,omitempty"`
	Tool    string    `json:"tool,omitempty"`
	Thread  string    `json:"thread,omitempty"`
	Event   string    `json:"event,omitempty"`
	Check   string    `json:"check"`
	Reason  string    `json:"reason"`
}

// PausedThread is a thread whose background runs on this machine stopped at the loop ceiling.
type PausedThread struct {
	Thread string `json:"thread"`
	Turns  int    `json:"turns"`
}

// SessionStatus is one session in StatusResponse.
type SessionStatus struct {
	Key       string    `json:"key"` // "claude:<session id>"
	Harness   Harness   `json:"harness"`
	SessionID string    `json:"session_id"`
	Cwd       string    `json:"cwd"`
	Agent     string    `json:"agent,omitempty"`
	Helpers   []string  `json:"helpers,omitempty"`
	Volunteer bool      `json:"volunteer"` // a live session that volunteers; never an ended one
	Tools     bool      `json:"tools"`     // an MCP connection is attached
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitzero"`
	LastHook  time.Time `json:"last_hook,omitzero"`
	Inbox     int       `json:"inbox"`
	Note      string    `json:"note,omitempty"` // for example "no hook seen for session codex:..."
}

// InboxResponse answers the inbox op (kwak inbox).
type InboxResponse struct {
	V        int            `json:"v"`
	OK       bool           `json:"ok"`
	Err      string         `json:"err,omitempty"`
	Sessions []InboxSession `json:"sessions"`
}

// InboxSession is one session with waiting items. Resume is the command that resumes it.
type InboxSession struct {
	Key       string  `json:"key"`
	Harness   Harness `json:"harness"`
	SessionID string  `json:"session_id"`
	Cwd       string  `json:"cwd"`
	Agent     string  `json:"agent,omitempty"`
	Ended     bool    `json:"ended"`
	Items     int     `json:"items"`
	Resume    string  `json:"resume"`
}

// ReloadResponse answers the reload op once the standing token works, or with the error. Detail
// is human text for Err, for example the registrar's answer.
type ReloadResponse struct {
	V      int    `json:"v"`
	OK     bool   `json:"ok"`
	Err    string `json:"err,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ---- Agent names (spec 3.6) ----

// OwnerName is the shape of an owner name. It has no dots.
var OwnerName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// HelperName is the shape of a helper name.
var HelperName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

// EventID is the shape kwak accepts for a Matrix event id, from a tool argument or a room event.
var EventID = regexp.MustCompile(`^\$[A-Za-z0-9_+/=:.-]{1,255}$`)

// Kind is the kind of an agent localpart.
type Kind string

const (
	KindSession  Kind = "session"
	KindHelper   Kind = "helper"
	KindStanding Kind = "standing"
)

// Name is a parsed agent localpart. Hex is empty for a standing name, Helper set only for a helper.
// A standing name is a pairing's listener account (decision 42), with Pair its word pair
// ("brave-heron"), or one of the per-owner standing accounts of earlier builds, with Pair empty.
// Test is set only for a standing name of a test pairing: "<pair>-test", or the old
// agent.<owner>.listener-test.
type Name struct {
	Kind   Kind
	Owner  string
	Hex    string
	Helper string
	Test   bool
	Pair   string
}

// ---- Appservice registration (spec 3.7) ----

// Registration is the appservice registration file. It is JSON, which tuwunel reads as YAML.
type Registration struct {
	ID               string     `json:"id"`
	URL              string     `json:"url"`
	ASToken          string     `json:"as_token"`
	HSToken          string     `json:"hs_token"`
	SenderLocalpart  string     `json:"sender_localpart"`
	RateLimited      bool       `json:"rate_limited"`
	ReceiveEphemeral bool       `json:"receive_ephemeral"`
	MSC4190          bool       `json:"io.element.msc4190"`
	Namespaces       Namespaces `json:"namespaces"`
}

// Namespaces of a Registration.
type Namespaces struct {
	Users   []Namespace `json:"users"`
	Aliases []Namespace `json:"aliases"`
	Rooms   []Namespace `json:"rooms"`
}

// Namespace is one regex entry of Namespaces.
type Namespace struct {
	Exclusive bool   `json:"exclusive"`
	Regex     string `json:"regex"`
}
