package registrar

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
)

// Homeserver is what the registrar asks of tuwunel, as the appservice (spec 2.1). userID "" means
// the @registrar bot.
type Homeserver interface {
	Register(ctx context.Context, localpart, deviceID string) (matrix.Login, error)
	Login(ctx context.Context, userID, deviceID string) (matrix.Login, error)
	LogoutDevice(ctx context.Context, userID, deviceID string) error
	LogoutAll(ctx context.Context, userID string) error
	JoinAs(ctx context.Context, userID, room string) error
	LeaveAs(ctx context.Context, userID, roomID string) error
	SetDisplayName(ctx context.Context, userID, name string) error
	SetAvatarURL(ctx context.Context, userID, url string) error
	UserExists(ctx context.Context, userID string) (bool, error)
	CreateDM(ctx context.Context, human string) (roomID string, err error)
	Invite(ctx context.Context, roomID, userID string) error
	RoomInfo(ctx context.Context, roomID string) (members int, encrypted bool, err error)
	FetchEvent(ctx context.Context, roomID, eventID string) (*matrix.Event, error)
	Notice(ctx context.Context, roomID, txnID, text string) error
	ResolveAlias(ctx context.Context, alias string) (string, error)
}

// Clock gives the time.
type Clock interface{ Now() time.Time }

// SystemClock is the real clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// ForwardedFor is the X-Forwarded-For the registrar sends to tuwunel on every request (spec 3.8).
const ForwardedFor = "127.0.0.1"

// NewHomeserver returns the Homeserver over the client-server API with the as_token. It is the
// only production client that accepts an http:// base (spec 2.1, 3.8).
func NewHomeserver(base, asToken string, doer matrix.Doer) Homeserver {
	return &asClient{c: &matrix.Client{
		Base:         base,
		Token:        asToken,
		ForwardedFor: ForwardedFor,
		AllowHTTP:    strings.HasPrefix(base, "http://"),
		HTTP:         doer,
	}}
}

type asClient struct{ c *matrix.Client }

func (a *asClient) as(userID string) *matrix.Client {
	if userID == "" {
		return a.c
	}
	return a.c.As(userID)
}

func (a *asClient) Register(ctx context.Context, lp, dev string) (matrix.Login, error) {
	l, err := a.c.RegisterAppservice(ctx, lp, dev)
	return l, scrub(err)
}

func (a *asClient) Login(ctx context.Context, userID, dev string) (matrix.Login, error) {
	l, err := a.c.LoginAppservice(ctx, userID, dev)
	return l, scrub(err)
}

func (a *asClient) LogoutDevice(ctx context.Context, userID, dev string) error {
	c := a.c.As(userID)
	c.AsDevice = dev
	return scrub(c.Logout(ctx))
}

func (a *asClient) LogoutAll(ctx context.Context, userID string) error {
	return scrub(a.c.As(userID).LogoutAll(ctx))
}

func (a *asClient) JoinAs(ctx context.Context, userID, room string) error {
	_, err := a.as(userID).Join(ctx, room)
	return scrub(err)
}

func (a *asClient) LeaveAs(ctx context.Context, userID, roomID string) error {
	return scrub(a.as(userID).Leave(ctx, roomID))
}

func (a *asClient) SetDisplayName(ctx context.Context, userID, name string) error {
	return scrub(a.as(userID).SetDisplayName(ctx, userID, name))
}

func (a *asClient) SetAvatarURL(ctx context.Context, userID, url string) error {
	return scrub(a.as(userID).SetAvatarURL(ctx, userID, url))
}

func (a *asClient) UserExists(ctx context.Context, userID string) (bool, error) {
	ok, err := a.c.ProfileExists(ctx, userID)
	return ok, scrub(err)
}

func (a *asClient) CreateDM(ctx context.Context, human string) (string, error) {
	id, err := a.c.CreateRoom(ctx, DMBody(human))
	return id, scrub(err)
}

func (a *asClient) Invite(ctx context.Context, roomID, userID string) error {
	return scrub(a.c.Invite(ctx, roomID, userID))
}

func (a *asClient) RoomInfo(ctx context.Context, roomID string) (int, bool, error) {
	m, err := a.c.JoinedMembers(ctx, roomID)
	if err != nil {
		return 0, false, scrub(err)
	}
	enc, err := a.c.RoomEncrypted(ctx, roomID)
	return len(m), enc, scrub(err)
}

func (a *asClient) FetchEvent(ctx context.Context, roomID, eventID string) (*matrix.Event, error) {
	ev, err := a.c.Event(ctx, roomID, eventID)
	return ev, scrub(err)
}

func (a *asClient) Notice(ctx context.Context, roomID, txnID, text string) error {
	_, err := a.c.Send(ctx, roomID, txnID, map[string]string{"msgtype": "m.notice", "body": text})
	return scrub(err)
}

func (a *asClient) ResolveAlias(ctx context.Context, alias string) (string, error) {
	id, err := a.c.ResolveAlias(ctx, alias)
	return id, scrub(err)
}

// scrub drops the request URL from a transport or decode error, so a log line never holds a
// URL or its ?user_id= (I4). A matrix.Error carries no URL and is kept as it is.
func scrub(err error) error {
	var me *matrix.Error
	if err == nil || errors.As(err, &me) {
		return err
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return &hsError{msg: "matrix: " + ue.Op + ": " + scrub(ue.Err).Error(), err: ue.Err}
	}
	if ue != nil {
		return &hsError{msg: "matrix: " + ue.Op + " failed"}
	}
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return &hsError{msg: "matrix: decode: " + se.Error(), err: se}
	case errors.As(err, &te):
		return &hsError{msg: "matrix: decode: " + te.Error(), err: te}
	}
	return err
}

// ScrubError is scrub for callers outside the package, such as kwak-registrar create-room.
func ScrubError(err error) error { return scrub(err) }

type hsError struct {
	msg string
	err error
}

func (e *hsError) Error() string { return e.msg }
func (e *hsError) Unwrap() error { return e.err }

// DMBody is the createRoom body of a registrar DM (spec 3.5). It has no "users" key: tuwunel
// makes v12 rooms, whose auth refuses a creator in "users".
func DMBody(human string) map[string]any {
	return map[string]any{
		"preset":    "private_chat",
		"is_direct": true,
		"invite":    []string{human},
		"name":      "kwak registrar",
		"power_level_content_override": map[string]any{
			"state_default": 100,
			"invite":        100,
			"events":        map[string]any{"m.room.encryption": 100},
		},
	}
}

// RoomBody is the createRoom body of the agents room (spec 3.11). bot is the @registrar user id.
func RoomBody(bot string) map[string]any {
	return map[string]any{
		"preset":          "public_chat",
		"visibility":      "private",
		"name":            "agents",
		"room_alias_name": "agents",
		"topic":           "kwak agents room. A thread's first message is a question; replies in it are answers.",
		"power_level_content_override": map[string]any{
			"users":          map[string]any{bot: 50},
			"users_default":  0,
			"events_default": 0,
			"state_default":  100,
			"invite":         50,
			"kick":           50,
			"ban":            50,
			"redact":         50,
			"notifications":  map[string]any{"room": 100},
			"events": map[string]any{
				"m.room.encryption":         100,
				"m.room.power_levels":       100,
				"m.room.join_rules":         100,
				"m.room.history_visibility": 100,
				"m.room.name":               100,
				"m.room.topic":              100,
				"m.room.tombstone":          100,
			},
		},
		"initial_state": []any{map[string]any{
			"type": "m.room.history_visibility", "state_key": "",
			"content": map[string]any{"history_visibility": "shared"},
		}},
	}
}
