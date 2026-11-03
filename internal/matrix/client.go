// Package matrix is a minimal client-server API client, shared by the listener and the registrar.
package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Doer sends one request. *http.Client satisfies it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client talks to one homeserver with one token. The zero value of the optional fields is off.
type Client struct {
	Base         string // "https://kwak.test" or "http://tuwunel:8008"
	Token        string // sent only as Authorization: Bearer, never in a URL
	AsUser       string // appservice identity assertion: adds ?user_id=
	AsDevice     string // with AsUser: adds ?device_id=
	ForwardedFor string // X-Forwarded-For value; required when talking to tuwunel directly (3.8)
	AllowHTTP    bool   // accept an http:// Base; production sets it only for the registrar's homeserver URL
	HTTP         Doer
}

// Response size limits (spec 2.1).
const (
	SmallLimit = 64 << 10
	LargeLimit = 8 << 20
)

// Deadlines for a context that has none. Sync always adds SyncSlack to its long-poll timeout.
const (
	DefaultTimeout = 30 * time.Second
	SyncSlack      = 30 * time.Second
)

// ErrTooLarge is returned when a response body passes its size limit.
var ErrTooLarge = errors.New("matrix: response too large")

// NewHTTPClient returns the *http.Client behind Doer in matrix and regclient: it never follows a
// redirect, so Authorization is never forwarded.
func NewHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Login is the result of a register or login.
type Login struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
}

// Error is a non-2xx answer from the homeserver.
type Error struct {
	Status     int
	Code, Msg  string          // errcode, error
	RetryAfter time.Duration   // from retry_after_ms or Retry-After
	Body       json.RawMessage // the raw body when it is JSON, for user-interactive auth
}

func (e *Error) Error() string {
	s := "matrix: HTTP " + strconv.Itoa(e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

// Matrix errcodes kwak acts on or answers with; tests script the others.
const (
	ErrUnknownToken = "M_UNKNOWN_TOKEN"
	ErrUserInUse    = "M_USER_IN_USE"
	ErrLimit        = "M_LIMIT_EXCEEDED"
	ErrForbidden    = "M_FORBIDDEN"
	ErrNotFound     = "M_NOT_FOUND"
	ErrExclusive    = "M_EXCLUSIVE"
)

// Event is a room event.
type Event struct {
	EventID  string          `json:"event_id"`
	RoomID   string          `json:"room_id,omitempty"`
	Sender   string          `json:"sender"`
	Type     string          `json:"type"`
	StateKey *string         `json:"state_key,omitempty"`
	TS       int64           `json:"origin_server_ts"`
	Content  json.RawMessage `json:"content"`
}

// SyncResponse is the part of a /sync answer kwak reads.
type SyncResponse struct {
	NextBatch string    `json:"next_batch"`
	Rooms     SyncRooms `json:"rooms"`
}

// SyncRooms holds the rooms of a SyncResponse by membership.
type SyncRooms struct {
	Join  map[string]JoinedRoom `json:"join,omitempty"`
	Leave map[string]LeftRoom   `json:"leave,omitempty"`
}

// JoinedRoom is one joined room of a SyncResponse.
type JoinedRoom struct {
	Timeline Timeline `json:"timeline"`
}

// LeftRoom is one left room of a SyncResponse.
type LeftRoom struct {
	Timeline Timeline `json:"timeline"`
}

// Timeline is a room timeline slice. Limited means events were left out before PrevBatch.
type Timeline struct {
	Events    []Event `json:"events"`
	Limited   bool    `json:"limited"`
	PrevBatch string  `json:"prev_batch"`
}

// MessagesResponse is a /messages page. End is empty when the server has no more history.
type MessagesResponse struct {
	Chunk []Event `json:"chunk"`
	Start string  `json:"start"`
	End   string  `json:"end,omitempty"`
}

const (
	v3 = "/_matrix/client/v3"
	v1 = "/_matrix/client/v1"
)

func esc(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(p))
	}
	return b.String()
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any, limit int64) error {
	base := strings.TrimRight(c.Base, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
	case strings.HasPrefix(base, "http://") && c.AllowHTTP:
	default:
		return errors.New("matrix: base must start with https://")
	}
	if c.HTTP == nil {
		return errors.New("matrix: no HTTP client")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	if c.AsUser != "" {
		if q == nil {
			q = url.Values{}
		}
		q.Set("user_id", c.AsUser)
		if c.AsDevice != "" {
			q.Set("device_id", c.AsDevice)
		}
	}
	u := base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.ForwardedFor != "" {
		req.Header.Set("X-Forwarded-For", c.ForwardedFor)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// *url.Error prints the request URL; spec I4 keeps URLs out of errors and logs.
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			return fmt.Errorf("matrix: %s: %w", ue.Op, ue.Err)
		}
		return fmt.Errorf("matrix: %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return readError(resp)
	}
	data, err := readLimited(resp.Body, limit)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("matrix: %s: decode: %w", method, err)
	}
	return nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}

func readError(resp *http.Response) error {
	e := &Error{Status: resp.StatusCode}
	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		e.Msg = "redirect refused"
		return e
	}
	data, _ := readLimited(resp.Body, SmallLimit)
	var m struct {
		Code       string `json:"errcode"`
		Msg        string `json:"error"`
		RetryAfter int64  `json:"retry_after_ms"`
	}
	if data != nil && json.Unmarshal(data, &m) == nil {
		e.Code, e.Msg = m.Code, m.Msg
		e.Body = data
		if m.RetryAfter > 0 {
			e.RetryAfter = time.Duration(m.RetryAfter) * time.Millisecond
		}
	}
	if e.RetryAfter == 0 {
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
			e.RetryAfter = time.Duration(s) * time.Second
		}
	}
	if e.Msg == "" && e.Code == "" {
		e.Msg = http.StatusText(resp.StatusCode)
	}
	return e
}

// IsCode reports whether err is an *Error with errcode code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

func isNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// As returns a copy that asserts userID (appservice ?user_id=), with no device.
func (c *Client) As(userID string) *Client {
	cp := *c
	cp.AsUser = userID
	cp.AsDevice = ""
	return &cp
}

// Register posts a /register body as is. RegisterAppservice and the matrixtest helpers use it.
func (c *Client) Register(ctx context.Context, body any) (Login, error) {
	var l Login
	err := c.do(ctx, http.MethodPost, v3+"/register", nil, body, &l, SmallLimit)
	return l, err
}

// RegisterAppservice registers localpart with the appservice login type, without a password.
func (c *Client) RegisterAppservice(ctx context.Context, localpart, deviceID string) (Login, error) {
	body := map[string]any{"type": "m.login.application_service", "username": localpart}
	if deviceID != "" {
		body["device_id"] = deviceID
	}
	return c.Register(ctx, body)
}

// LoginAppservice logs userID in on deviceID with the appservice login type.
func (c *Client) LoginAppservice(ctx context.Context, userID, deviceID string) (Login, error) {
	body := map[string]any{
		"type":       "m.login.application_service",
		"identifier": map[string]string{"type": "m.id.user", "user": userID},
	}
	if deviceID != "" {
		body["device_id"] = deviceID
	}
	return c.LoginWith(ctx, body)
}

// LoginWith posts a /login body as is. LoginAppservice and the matrixtest helpers use it.
func (c *Client) LoginWith(ctx context.Context, body any) (Login, error) {
	var l Login
	err := c.do(ctx, http.MethodPost, v3+"/login", nil, body, &l, SmallLimit)
	return l, err
}

// Logout logs out the token's device; with AsUser and AsDevice, that device only.
func (c *Client) Logout(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, v3+"/logout", nil, struct{}{}, nil, SmallLimit)
}

// LogoutAll logs out every device of the user.
func (c *Client) LogoutAll(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, v3+"/logout/all", nil, struct{}{}, nil, SmallLimit)
}

type roomIDBody struct {
	RoomID string `json:"room_id"`
}

// Join joins a room by id or alias.
func (c *Client) Join(ctx context.Context, roomIDOrAlias string) (roomID string, err error) {
	var r roomIDBody
	err = c.do(ctx, http.MethodPost, v3+"/join"+esc(roomIDOrAlias), nil, struct{}{}, &r, SmallLimit)
	return r.RoomID, err
}

// Leave leaves a room.
func (c *Client) Leave(ctx context.Context, roomID string) error {
	return c.do(ctx, http.MethodPost, v3+"/rooms"+esc(roomID, "leave"), nil, struct{}{}, nil, SmallLimit)
}

// Invite invites userID to roomID.
func (c *Client) Invite(ctx context.Context, roomID, userID string) error {
	body := map[string]string{"user_id": userID}
	return c.do(ctx, http.MethodPost, v3+"/rooms"+esc(roomID, "invite"), nil, body, nil, SmallLimit)
}

// ResolveAlias resolves a room alias to its id.
func (c *Client) ResolveAlias(ctx context.Context, alias string) (roomID string, err error) {
	var r roomIDBody
	err = c.do(ctx, http.MethodGet, v3+"/directory/room"+esc(alias), nil, nil, &r, SmallLimit)
	return r.RoomID, err
}

// CreateRoom creates a room from a createRoom body.
func (c *Client) CreateRoom(ctx context.Context, body any) (roomID string, err error) {
	var r roomIDBody
	err = c.do(ctx, http.MethodPost, v3+"/createRoom", nil, body, &r, SmallLimit)
	return r.RoomID, err
}

// SetDisplayName sets userID's display name.
func (c *Client) SetDisplayName(ctx context.Context, userID, name string) error {
	body := map[string]string{"displayname": name}
	return c.do(ctx, http.MethodPut, v3+"/profile"+esc(userID, "displayname"), nil, body, nil, SmallLimit)
}

// SetAvatarURL sets userID's avatar; "" clears it.
func (c *Client) SetAvatarURL(ctx context.Context, userID, url string) error {
	body := map[string]string{"avatar_url": url}
	return c.do(ctx, http.MethodPut, v3+"/profile"+esc(userID, "avatar_url"), nil, body, nil, SmallLimit)
}

// ProfileExists reports whether userID has a profile, that is, exists. A 404 is false, not an error.
func (c *Client) ProfileExists(ctx context.Context, userID string) (bool, error) {
	err := c.do(ctx, http.MethodGet, v3+"/profile"+esc(userID), nil, nil, nil, SmallLimit)
	if isNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// Send sends a m.room.message with txnID.
func (c *Client) Send(ctx context.Context, roomID, txnID string, content any) (eventID string, err error) {
	var r struct {
		EventID string `json:"event_id"`
	}
	p := v3 + "/rooms" + esc(roomID, "send", "m.room.message", txnID)
	err = c.do(ctx, http.MethodPut, p, nil, content, &r, SmallLimit)
	return r.EventID, err
}

// Sync runs one /sync. since and filter may be empty. The deadline is timeout plus SyncSlack.
func (c *Client) Sync(ctx context.Context, since, filter string, timeout time.Duration) (*SyncResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+SyncSlack)
	defer cancel()
	q := url.Values{}
	q.Set("timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	q.Set("set_presence", "offline")
	if since != "" {
		q.Set("since", since)
	}
	if filter != "" {
		q.Set("filter", filter)
	}
	var r SyncResponse
	if err := c.do(ctx, http.MethodGet, v3+"/sync", q, nil, &r, LargeLimit); err != nil {
		return nil, err
	}
	return &r, nil
}

// Messages pages backwards from from.
func (c *Client) Messages(ctx context.Context, roomID, from string, limit int, filter string) (*MessagesResponse, error) {
	q := url.Values{}
	q.Set("dir", "b")
	q.Set("limit", strconv.Itoa(limit))
	if from != "" {
		q.Set("from", from)
	}
	if filter != "" {
		q.Set("filter", filter)
	}
	var r MessagesResponse
	if err := c.do(ctx, http.MethodGet, v3+"/rooms"+esc(roomID, "messages"), q, nil, &r, LargeLimit); err != nil {
		return nil, err
	}
	return &r, nil
}

// Event fetches one event.
func (c *Client) Event(ctx context.Context, roomID, eventID string) (*Event, error) {
	var ev Event
	if err := c.do(ctx, http.MethodGet, v3+"/rooms"+esc(roomID, "event", eventID), nil, nil, &ev, SmallLimit); err != nil {
		return nil, err
	}
	return &ev, nil
}

// ThreadReplies returns up to limit m.thread relations of rootID, newest first.
func (c *Client) ThreadReplies(ctx context.Context, roomID, rootID string, limit int) ([]Event, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("dir", "b")
	var r struct {
		Chunk []Event `json:"chunk"`
	}
	p := v1 + "/rooms" + esc(roomID, "relations", rootID, "m.thread")
	if err := c.do(ctx, http.MethodGet, p, q, nil, &r, LargeLimit); err != nil {
		return nil, err
	}
	return r.Chunk, nil
}

// JoinedMembers returns the user ids joined to roomID, sorted.
func (c *Client) JoinedMembers(ctx context.Context, roomID string) ([]string, error) {
	var r struct {
		Joined map[string]json.RawMessage `json:"joined"`
	}
	if err := c.do(ctx, http.MethodGet, v3+"/rooms"+esc(roomID, "joined_members"), nil, nil, &r, LargeLimit); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(r.Joined))
	for id := range r.Joined {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// RoomEncrypted reports whether roomID has an m.room.encryption state event.
func (c *Client) RoomEncrypted(ctx context.Context, roomID string) (bool, error) {
	err := c.do(ctx, http.MethodGet, v3+"/rooms"+esc(roomID, "state", "m.room.encryption"), nil, nil, nil, SmallLimit)
	if isNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
