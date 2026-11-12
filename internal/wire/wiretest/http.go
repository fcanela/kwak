// Package wiretest holds the two contract tables every side of kwak tests against (plan P0): the
// HTTP transcript table of the registrar's /_kwak/v1 API and the IPC golden table of the local
// socket. Both are plain data. Each side sets up its own state for a row; Name says what it is.
package wiretest

import (
	"fmt"
	"strings"

	"github.com/fcanela/kwak/internal/wire"
)

// Fixture values shared by both tables. None is a real secret.
const (
	ServerName     = "kwak.test"
	Owner          = "fcanela"
	PairingID      = "3f9a1c2e"
	OldPairingID   = "77c0de11"
	Machine        = "laptop"
	RoomID         = "!a1B2c3D4"
	Bot            = "@registrar:" + ServerName
	PairCode       = "7H3Q-9XRM-K2PD"
	Claim          = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	OwnerKey       = "kwak_ok_ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8"
	OldOwnerKey    = "kwak_ok_QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8"
	BadKey         = "kwak_ok_YGFiY2RlZmdoaWprbG1ub3BxcnN0dXZ3eHl6e3x9fn8"
	StandingToken  = "tok-standing-0001"
	AgentToken     = "tok-agent-0001"
	ClaudeSession  = "4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d"                  // UUIDv4
	CodexSession   = "01a198dd-ad17-79f0-8261-713321e78fea"                  // UUIDv7
	StandingUser   = "@agent.fcanela.brave-heron.listener:" + ServerName     // the pairing's listener account (decision 42)
	TestStanding   = "@agent.fcanela.calm-otter-test.listener:" + ServerName // a test pairing's listener account
	LegacyStanding = "@agent.fcanela.listener:" + ServerName                 // the per-owner account of builds before decision 42
	ReportRoomID   = "!r3P0rtR00m"
	ClaudeAgent    = "@agent.fcanela.4f2a9c1e7b034d5a:" + ServerName // Candidates(...)[0]
	CodexAgent     = "@agent.fcanela.8261713321e78fea:" + ServerName // Candidates(...)[0]
	HelperAgent    = "@agent.fcanela.4f2a9c1e7b034d5a.tests:" + ServerName
	OtherAgent     = "@agent.bob.0718a1b2c3d4e5f6:" + ServerName
	RequestID      = "00112233445566778899aabbccddeeff"
)

// LegacyStandingLocalpart is the per-owner standing account of the builds before decision 42:
// "agent.<owner>.listener", or "agent.<owner>.listener-test" for test pairings. The registrar
// never makes one any more and its job pass closes the ones left (decision 43); tests use this to
// seed state as those builds left it.
func LegacyStandingLocalpart(owner string, test bool) string {
	if test {
		return "agent." + owner + ".listener-test"
	}
	return "agent." + owner + ".listener"
}

// Exchange is one request to the registrar's listener API and its answer.
type Exchange struct {
	Name       string
	Method     string
	Path       string // full path, wire.APIPrefix included
	Auth       string // Authorization header value; "" sends none
	Request    string // body; "" sends none
	BadBody    bool   // Request must fail wire.DecodeAPI
	Status     int
	Errcode    string // set on every error answered by the registrar
	Response   string // body; "" when there is none
	RetryAfter string // Retry-After header, the proxy's 429 only
	Edge       bool   // answered by the proxy (nginx), not by the registrar
}

func bearer(s string) string { return "Bearer " + s }

func apiErr(code, msg string) string {
	return fmt.Sprintf(`{"errcode":%q,"error":%q}`, code, msg)
}

// TooManyIDs is a user_ids list of wire.MaxIDs+1 entries.
var TooManyIDs = func() string {
	ids := make([]string, wire.MaxIDs+1)
	for i := range ids {
		ids[i] = fmt.Sprintf(`"@agent.fcanela.%016x:%s"`, i, ServerName)
	}
	return "[" + strings.Join(ids, ",") + "]"
}()

// HTTP is the transcript table of spec 3.4 and the errcode table of spec 5.1.
var HTTP = []Exchange{
	// POST /pair
	{
		Name: "pair: first pairing", Method: "POST", Path: wire.APIPrefix + wire.PathPair,
		Request: `{"machine":"laptop","os":"linux","version":"0.1.0"}`,
		Status:  201,
		Response: `{"code":"7H3Q-9XRM-K2PD","claim":"` + Claim + `","expires_at":"2026-11-02T10:15:00Z",` +
			`"bot":"` + Bot + `","replaces":false}`,
	},
	{
		Name: "pair: re-pair with a valid old key", Method: "POST", Path: wire.APIPrefix + wire.PathPair,
		Auth:    bearer(OldOwnerKey),
		Request: `{"machine":"laptop","os":"linux","version":"0.1.0"}`,
		Status:  201,
		Response: `{"code":"7H3Q-9XRM-K2PD","claim":"` + Claim + `","expires_at":"2026-11-02T10:15:00Z",` +
			`"bot":"` + Bot + `","replaces":true}`,
	},
	{
		Name: "pair: a bad, expired or revoked old key is not an error", Method: "POST", Path: wire.APIPrefix + wire.PathPair,
		Auth:    bearer(BadKey),
		Request: `{"machine":"laptop","os":"darwin","version":"0.1.0"}`,
		Status:  201,
		Response: `{"code":"7H3Q-9XRM-K2PD","claim":"` + Claim + `","expires_at":"2026-11-02T10:15:00Z",` +
			`"bot":"` + Bot + `","replaces":false}`,
	},
	{
		Name: "pair: unknown field", Method: "POST", Path: wire.APIPrefix + wire.PathPair,
		Request: `{"machine":"laptop","os":"linux","version":"0.1.0","owner":"fcanela"}`, BadBody: true,
		Status: 400, Errcode: wire.ErrBadRequest,
		Response: apiErr(wire.ErrBadRequest, `json: unknown field "owner"`),
	},
	{
		Name: "pair: the proxy rate limit", Method: "POST", Path: wire.APIPrefix + wire.PathPair,
		Request: `{"machine":"laptop","os":"linux","version":"0.1.0"}`,
		Status:  429, RetryAfter: "30", Edge: true,
	},

	// POST /pair/claim
	{
		Name: "claim: not sent yet", Method: "POST", Path: wire.APIPrefix + wire.PathPairClaim,
		Auth: bearer(Claim), Request: `{}`,
		Status: 202, Response: `{"status":"pending"}`,
	},
	{
		Name: "claim: paired", Method: "POST", Path: wire.APIPrefix + wire.PathPairClaim,
		Auth: bearer(Claim), Request: `{}`,
		Status: 200,
		Response: `{"status":"paired","owner":"fcanela","pairing_id":"3f9a1c2e","owner_key":"` + OwnerKey + `",` +
			`"key_expires_at":"2027-01-31T10:05:00Z","room_id":"!a1B2c3D4","standing_user":"` + StandingUser + `",` +
			`"server_name":"kwak.test"}`,
	},
	{
		Name: "claim: expired or already collected", Method: "POST", Path: wire.APIPrefix + wire.PathPairClaim,
		Auth: bearer(Claim), Request: `{}`,
		Status: 410, Errcode: wire.ErrExpired,
		Response: apiErr(wire.ErrExpired, "pairing expired; run kwak pair again"),
	},
	{
		Name: "claim: unknown claim", Method: "POST", Path: wire.APIPrefix + wire.PathPairClaim,
		Auth: bearer(BadKey), Request: `{}`,
		Status: 410, Errcode: wire.ErrExpired,
		Response: apiErr(wire.ErrExpired, "pairing expired; run kwak pair again"),
	},

	// POST /standing
	{
		Name: "standing: token", Method: "POST", Path: wire.APIPrefix + wire.PathStanding,
		Auth: bearer(OwnerKey), Request: `{}`,
		Status:   200,
		Response: `{"user_id":"` + StandingUser + `","access_token":"` + StandingToken + `","device_id":"KWAK-3f9a1c2e"}`,
	},
	{
		Name: "standing: unknown, expired or revoked key", Method: "POST", Path: wire.APIPrefix + wire.PathStanding,
		Auth: bearer(BadKey), Request: `{}`,
		Status: 401, Errcode: wire.ErrBadKey, Response: apiErr(wire.ErrBadKey, "unknown, expired or revoked key"),
	},
	{
		Name: "standing: no key", Method: "POST", Path: wire.APIPrefix + wire.PathStanding,
		Request: `{}`,
		Status:  401, Errcode: wire.ErrBadKey, Response: apiErr(wire.ErrBadKey, "missing key"),
	},
	{
		Name: "standing: owner removed", Method: "POST", Path: wire.APIPrefix + wire.PathStanding,
		Auth: bearer(OwnerKey), Request: `{}`,
		Status: 403, Errcode: wire.ErrOwnerRemoved, Response: apiErr(wire.ErrOwnerRemoved, "owner removed"),
	},

	// POST /agents
	{
		Name: "agents: new Claude session", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"` + ClaudeSession + `","helper":""}`,
		Status:  200,
		Response: `{"user_id":"` + ClaudeAgent + `","access_token":"` + AgentToken + `","device_id":"KWAK",` +
			`"room_id":"!a1B2c3D4","new":true,"reopened":false}`,
	},
	{
		Name: "agents: new Codex session", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"codex","session":"` + CodexSession + `","helper":""}`,
		Status:  200,
		Response: `{"user_id":"` + CodexAgent + `","access_token":"` + AgentToken + `","device_id":"KWAK",` +
			`"room_id":"!a1B2c3D4","new":true,"reopened":false}`,
	},
	{
		Name: "agents: upper-case session id of an open agent re-mints its token", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"ffeeddccbbaa99887766554433221100","harness":"claude","session":"4F2A9C1E-7B03-4D5A-8C0D-1E2F3A4B5C6D","helper":""}`,
		Status:  200,
		Response: `{"user_id":"` + ClaudeAgent + `","access_token":"` + AgentToken + `","device_id":"KWAK",` +
			`"room_id":"!a1B2c3D4","new":false,"reopened":false}`,
	},
	{
		Name: "agents: closed agent reopens under its name", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"` + ClaudeSession + `","helper":""}`,
		Status:  200,
		Response: `{"user_id":"` + ClaudeAgent + `","access_token":"` + AgentToken + `","device_id":"KWAK",` +
			`"room_id":"!a1B2c3D4","new":false,"reopened":true}`,
	},
	{
		Name: "agents: new helper of an open parent", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"` + ClaudeSession + `","helper":"tests"}`,
		Status:  200,
		Response: `{"user_id":"` + HelperAgent + `","access_token":"` + AgentToken + `","device_id":"KWAK",` +
			`"room_id":"!a1B2c3D4","new":true,"reopened":false}`,
	},
	{
		Name: "agents: session id is not a UUID", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"../4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b","helper":""}`,
		Status:  400, Errcode: wire.ErrBadSession, Response: apiErr(wire.ErrBadSession, "session id is not a UUID"),
	},
	{
		Name: "agents: unknown harness", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"gemini","session":"` + ClaudeSession + `","helper":""}`,
		Status:  400, Errcode: wire.ErrBadRequest, Response: apiErr(wire.ErrBadRequest, `unknown harness "gemini"`),
	},
	{
		Name: "agents: bad helper name", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"` + ClaudeSession + `","helper":"Tests.x"}`,
		Status:  400, Errcode: wire.ErrBadRequest, Response: apiErr(wire.ErrBadRequest, "bad helper name"),
	},
	{
		Name: "agents: bad request_id", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"x","harness":"claude","session":"` + ClaudeSession + `","helper":""}`,
		Status:  400, Errcode: wire.ErrBadRequest, Response: apiErr(wire.ErrBadRequest, "request_id must be 32 hex digits"),
	},
	{
		Name: "agents: unknown field", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"` + ClaudeSession + `","helper":"","name":"agent.fcanela.x"}`,
		BadBody: true,
		Status:  400, Errcode: wire.ErrBadRequest, Response: apiErr(wire.ErrBadRequest, `json: unknown field "name"`),
	},
	{
		Name: "agents: the session's agent belongs to another pairing", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"` + ClaudeSession + `","helper":""}`,
		Status:  403, Errcode: wire.ErrNotYours, Response: apiErr(wire.ErrNotYours, "agent belongs to another pairing"),
	},
	{
		Name: "agents: helper whose parent belongs to another pairing", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"` + ClaudeSession + `","helper":"tests"}`,
		Status:  403, Errcode: wire.ErrNotYours, Response: apiErr(wire.ErrNotYours, "parent agent belongs to another pairing"),
	},
	{
		Name: "agents: helper whose parent is closed", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"` + ClaudeSession + `","helper":"tests"}`,
		Status:  403, Errcode: wire.ErrParentClosed, Response: apiErr(wire.ErrParentClosed, "parent agent is closed"),
	},
	{
		Name: "agents: helper whose parent does not exist", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"codex","session":"` + CodexSession + `","helper":"tests"}`,
		Status:  403, Errcode: wire.ErrParentClosed, Response: apiErr(wire.ErrParentClosed, "parent agent does not exist"),
	},
	{
		Name: "agents: all five names taken", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"` + ClaudeSession + `","helper":""}`,
		Status:  409, Errcode: wire.ErrNamesExhausted, Response: apiErr(wire.ErrNamesExhausted, "all candidate names are taken"),
	},
	{
		Name: "agents: display name or join failed at tuwunel", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"` + ClaudeSession + `","helper":""}`,
		Status:  502, Errcode: wire.ErrHomeserver, Response: apiErr(wire.ErrHomeserver, "M_UNKNOWN"),
	},
	{
		Name: "agents: bad key", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(BadKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"` + ClaudeSession + `","helper":""}`,
		Status:  401, Errcode: wire.ErrBadKey, Response: apiErr(wire.ErrBadKey, "unknown, expired or revoked key"),
	},
	{
		Name: "agents: owner removed", Method: "POST", Path: wire.APIPrefix + wire.PathAgents,
		Auth:    bearer(OwnerKey),
		Request: `{"request_id":"` + RequestID + `","harness":"claude","session":"` + ClaudeSession + `","helper":""}`,
		Status:  403, Errcode: wire.ErrOwnerRemoved, Response: apiErr(wire.ErrOwnerRemoved, "owner removed"),
	},

	// POST /agents/close
	{
		Name: "close: session agent and its helper", Method: "POST", Path: wire.APIPrefix + wire.PathAgentsClose,
		Auth:     bearer(OwnerKey),
		Request:  `{"user_ids":["` + ClaudeAgent + `","` + HelperAgent + `"],"reason":"session_ended"}`,
		Status:   200,
		Response: `{"closed":["` + ClaudeAgent + `","` + HelperAgent + `"]}`,
	},
	{
		Name: "close: another pairing's agent", Method: "POST", Path: wire.APIPrefix + wire.PathAgentsClose,
		Auth:    bearer(OwnerKey),
		Request: `{"user_ids":["` + ClaudeAgent + `","` + OtherAgent + `"],"reason":"session_ended"}`,
		Status:  403, Errcode: wire.ErrNotYours, Response: apiErr(wire.ErrNotYours, "not this pairing's agent: "+OtherAgent),
	},
	{
		Name: "close: the standing account", Method: "POST", Path: wire.APIPrefix + wire.PathAgentsClose,
		Auth:    bearer(OwnerKey),
		Request: `{"user_ids":["` + StandingUser + `"],"reason":"session_ended"}`,
		Status:  403, Errcode: wire.ErrNotYours, Response: apiErr(wire.ErrNotYours, "not this pairing's agent: "+StandingUser),
	},
	{
		Name: "close: more than 500 ids", Method: "POST", Path: wire.APIPrefix + wire.PathAgentsClose,
		Auth:    bearer(OwnerKey),
		Request: `{"user_ids":` + TooManyIDs + `,"reason":"session_ended"}`,
		Status:  400, Errcode: wire.ErrBadRequest, Response: apiErr(wire.ErrBadRequest, "at most 500 user ids"),
	},
	{
		Name: "close: bad key", Method: "POST", Path: wire.APIPrefix + wire.PathAgentsClose,
		Auth:    bearer(BadKey),
		Request: `{"user_ids":["` + ClaudeAgent + `"],"reason":"session_ended"}`,
		Status:  401, Errcode: wire.ErrBadKey, Response: apiErr(wire.ErrBadKey, "unknown, expired or revoked key"),
	},

	// POST /agents/alive
	{
		Name: "alive: one open, one unknown, one closed", Method: "POST", Path: wire.APIPrefix + wire.PathAgentsAlive,
		Auth:     bearer(OwnerKey),
		Request:  `{"user_ids":["` + ClaudeAgent + `","` + CodexAgent + `","` + HelperAgent + `"]}`,
		Status:   200,
		Response: `{"unknown":["` + CodexAgent + `"],"closed":["` + HelperAgent + `"]}`,
	},
	{
		Name: "alive: all open", Method: "POST", Path: wire.APIPrefix + wire.PathAgentsAlive,
		Auth:     bearer(OwnerKey),
		Request:  `{"user_ids":["` + ClaudeAgent + `"]}`,
		Status:   200,
		Response: `{"unknown":[],"closed":[]}`,
	},
	{
		Name: "alive: more than 500 ids", Method: "POST", Path: wire.APIPrefix + wire.PathAgentsAlive,
		Auth:    bearer(OwnerKey),
		Request: `{"user_ids":` + TooManyIDs + `}`,
		Status:  400, Errcode: wire.ErrBadRequest, Response: apiErr(wire.ErrBadRequest, "at most 500 user ids"),
	},
	{
		Name: "alive: bad key", Method: "POST", Path: wire.APIPrefix + wire.PathAgentsAlive,
		Auth:    bearer(BadKey),
		Request: `{"user_ids":["` + ClaudeAgent + `"]}`,
		Status:  401, Errcode: wire.ErrBadKey, Response: apiErr(wire.ErrBadKey, "unknown, expired or revoked key"),
	},

	// POST /unpair
	{
		Name: "unpair: revokes this pairing", Method: "POST", Path: wire.APIPrefix + wire.PathUnpair,
		Auth: bearer(OwnerKey), Request: `{}`,
		Status: 200, Response: `{}`,
	},
	{
		Name: "unpair: bad key", Method: "POST", Path: wire.APIPrefix + wire.PathUnpair,
		Auth: bearer(BadKey), Request: `{}`,
		Status: 401, Errcode: wire.ErrBadKey, Response: apiErr(wire.ErrBadKey, "unknown, expired or revoked key"),
	},

	// GET /me
	{
		Name: "me: owner settings", Method: "GET", Path: wire.APIPrefix + wire.PathMe,
		Auth:   bearer(OwnerKey),
		Status: 200,
		Response: `{"owner":"fcanela","pairing_id":"3f9a1c2e","machine":"laptop","key_expires_at":"2027-01-31T10:05:00Z",` +
			`"agents_open":12,"settings":{"resume":"off","question_timeout":"15m"}}`,
	},
	{
		Name: "me: bad key", Method: "GET", Path: wire.APIPrefix + wire.PathMe,
		Auth:   bearer(BadKey),
		Status: 401, Errcode: wire.ErrBadKey, Response: apiErr(wire.ErrBadKey, "unknown, expired or revoked key"),
	},
	{
		Name: "me: owner removed", Method: "GET", Path: wire.APIPrefix + wire.PathMe,
		Auth:   bearer(OwnerKey),
		Status: 403, Errcode: wire.ErrOwnerRemoved, Response: apiErr(wire.ErrOwnerRemoved, "owner removed"),
	},

	// POST /report/join (decisions 39 and 40)
	{
		Name: "report: join the session's agent", Method: "POST", Path: wire.APIPrefix + wire.PathReportJoin,
		Auth: bearer(OwnerKey), Request: `{"user_id":"` + ClaudeAgent + `"}`,
		Status: 200, Response: `{"room_id":"` + ReportRoomID + `"}`,
	},
	{
		Name: "report: join the standing account", Method: "POST", Path: wire.APIPrefix + wire.PathReportJoin,
		Auth: bearer(OwnerKey), Request: `{"user_id":"` + StandingUser + `"}`,
		Status: 200, Response: `{"room_id":"` + ReportRoomID + `"}`,
	},
	{
		Name: "report: another pairing's agent", Method: "POST", Path: wire.APIPrefix + wire.PathReportJoin,
		Auth: bearer(OwnerKey), Request: `{"user_id":"` + OtherAgent + `"}`,
		Status: 403, Errcode: wire.ErrNotYours, Response: apiErr(wire.ErrNotYours, "not an open account of this pairing"),
	},
	{
		Name: "report: the standing account of the other kind", Method: "POST", Path: wire.APIPrefix + wire.PathReportJoin,
		Auth: bearer(OwnerKey), Request: `{"user_id":"` + TestStanding + `"}`,
		Status: 403, Errcode: wire.ErrNotYours, Response: apiErr(wire.ErrNotYours, "not an open account of this pairing"),
	},
	{
		Name: "report: no report room", Method: "POST", Path: wire.APIPrefix + wire.PathReportJoin,
		Auth: bearer(OwnerKey), Request: `{"user_id":"` + StandingUser + `"}`,
		Status: 404, Errcode: wire.ErrNoReportRoom, Response: apiErr(wire.ErrNoReportRoom, "this server has no report room for this pairing"),
	},
	{
		Name: "report: unknown field", Method: "POST", Path: wire.APIPrefix + wire.PathReportJoin,
		Auth: bearer(OwnerKey), Request: `{"user_id":"` + StandingUser + `","text":"hi"}`, BadBody: true,
		Status: 400, Errcode: wire.ErrBadRequest, Response: apiErr(wire.ErrBadRequest, `json: unknown field "text"`),
	},
	{
		Name: "report: bad key", Method: "POST", Path: wire.APIPrefix + wire.PathReportJoin,
		Auth: bearer(BadKey), Request: `{"user_id":"` + StandingUser + `"}`,
		Status: 401, Errcode: wire.ErrBadKey, Response: apiErr(wire.ErrBadKey, "unknown, expired or revoked key"),
	},
}

// NewRequest returns a pointer to the type the request body decodes into, or nil for no body.
func (e Exchange) NewRequest() any {
	if e.Request == "" {
		return nil
	}
	switch strings.TrimPrefix(e.Path, wire.APIPrefix) {
	case wire.PathPair:
		return new(wire.PairRequest)
	case wire.PathAgents:
		return new(wire.AgentRequest)
	case wire.PathAgentsClose:
		return new(wire.CloseRequest)
	case wire.PathAgentsAlive:
		return new(wire.AliveRequest)
	case wire.PathReportJoin:
		return new(wire.ReportJoinRequest)
	case wire.PathPairClaim, wire.PathStanding, wire.PathUnpair:
		return new(wire.Empty)
	}
	return nil
}

// NewResponse returns a pointer to the type the response body decodes into, or nil for no body.
func (e Exchange) NewResponse() any {
	if e.Response == "" {
		return nil
	}
	if e.Status >= 300 {
		return new(wire.APIError)
	}
	switch strings.TrimPrefix(e.Path, wire.APIPrefix) {
	case wire.PathPair:
		return new(wire.PairResponse)
	case wire.PathPairClaim:
		return new(wire.ClaimResponse)
	case wire.PathStanding:
		return new(wire.Standing)
	case wire.PathAgents:
		return new(wire.Agent)
	case wire.PathAgentsClose:
		return new(wire.CloseResponse)
	case wire.PathAgentsAlive:
		return new(wire.AliveResponse)
	case wire.PathUnpair:
		return new(wire.Empty)
	case wire.PathMe:
		return new(wire.Me)
	case wire.PathReportJoin:
		return new(wire.ReportJoin)
	}
	return nil
}

// fixedKeys are compared by value in Check; every other value only by shape.
var fixedKeys = []string{"errcode", "status", "new", "reopened", "replaces", "user_id"}

// Check compares a real answer with this row: the same status, the same errcode, and a body of the
// same shape (same keys, same JSON kinds, arrays never null where the row has one). Tokens, codes,
// times and error text may differ; the keys in fixedKeys may not.
func (e Exchange) Check(status int, body []byte) error {
	if status != e.Status {
		return fmt.Errorf("%s: status %d, want %d (body %s)", e.Name, status, e.Status, body)
	}
	if e.Response == "" {
		return nil
	}
	if err := SameShape([]byte(e.Response), body, fixedKeys...); err != nil {
		return fmt.Errorf("%s: %w", e.Name, err)
	}
	return nil
}
