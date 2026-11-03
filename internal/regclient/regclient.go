// Package regclient is the HTTPS client of the registrar's /_kwak/v1 API (spec 3.4). The listener
// uses it with the owner key; kwak pair uses Pair and Claim.
package regclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
)

// Deadlines for a context that has none.
const (
	DefaultTimeout = 30 * time.Second
	ClaimTimeout   = 40 * time.Second // the registrar holds a claim for up to 25 s
)

// Client calls the registrar's listener API. Key is called for every authenticated request, so a
// key replaced in the keyring is picked up at the next call, unless SetKey gave one. An empty key
// sends no Authorization. A Client must not be copied after first use.
type Client struct {
	Base string // "https://<server>", without /_kwak/v1
	// BaseFunc gives the base for every request when Base is "", so a server that changes (a first
	// kwak pair under a running listener) is used at the next call.
	BaseFunc func() (string, error)
	Key      func() (string, error)
	HTTP     matrix.Doer // nil: matrix.NewHTTPClient(), which never follows a redirect
	// InsecureHTTPForTests accepts an http:// Base. Production never sets it (spec 2.1).
	InsecureHTTPForTests bool

	mu  sync.Mutex
	set bool
	k   string
}

// SetKey makes every later request use key instead of calling Key (listener.KeySetter).
func (c *Client) SetKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set, c.k = true, key
}

// Error is a non-2xx answer. Errcode is empty when the body was not a registrar error, for
// example a reverse proxy's 429 or a proxy's 502. It unwraps to the *wire.APIError of the answer.
type Error struct {
	Status     int
	Errcode    string
	Message    string
	RetryAfter time.Duration // from Retry-After
}

func (e *Error) Error() string {
	s := "registrar: HTTP " + strconv.Itoa(e.Status)
	if e.Errcode != "" {
		s += " " + e.Errcode
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Unwrap gives the answer as a *wire.APIError.
func (e *Error) Unwrap() error {
	return &wire.APIError{Status: e.Status, Errcode: e.Errcode, Message: e.Message}
}

// Code returns the registrar errcode of err, or "".
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Errcode
	}
	return ""
}

// Status returns the HTTP status of err, or 0 when the registrar did not answer.
func Status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func (c *Client) key() (string, error) {
	c.mu.Lock()
	set, k := c.set, c.k
	c.mu.Unlock()
	if set {
		return k, nil
	}
	if c.Key == nil {
		return "", nil
	}
	return c.Key()
}

// call sends one request. auth is the bearer value ("" sends none); in and out may be nil.
func (c *Client) call(ctx context.Context, method, path, auth string, in, out any, timeout time.Duration) (int, error) {
	base := c.Base
	if base == "" && c.BaseFunc != nil {
		var err error
		if base, err = c.BaseFunc(); err != nil {
			return 0, fmt.Errorf("registrar: %w", err)
		}
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return 0, errors.New("registrar: bad server URL")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && c.InsecureHTTPForTests:
	default:
		return 0, fmt.Errorf("registrar: server URL must start with https://")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+wire.APIPrefix+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	doer := c.HTTP
	if doer == nil {
		doer = matrix.NewHTTPClient()
	}
	resp, err := doer.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, fmt.Errorf("registrar: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, wire.MaxBody+1))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("registrar: %s %s: %w", method, path, err)
	}
	if len(data) > wire.MaxBody {
		return resp.StatusCode, fmt.Errorf("registrar: %s %s: answer over %d bytes", method, path, wire.MaxBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, answerError(resp, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("registrar: %s %s: decode: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func answerError(resp *http.Response, data []byte) error {
	e := &Error{Status: resp.StatusCode}
	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		e.Message = "redirect refused"
		return e
	}
	var a wire.APIError
	if json.Unmarshal(data, &a) == nil && strings.HasPrefix(a.Errcode, "KWAK_") {
		e.Errcode, e.Message = a.Errcode, a.Message
	} else {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
		e.RetryAfter = time.Duration(s) * time.Second
	}
	return e
}

func (c *Client) authed(ctx context.Context, method, path string, in, out any) error {
	k, err := c.key()
	if err != nil {
		return err
	}
	_, err = c.call(ctx, method, path, k, in, out, DefaultTimeout)
	return err
}

// Pair starts a pairing (POST /pair). oldKey is this machine's current key on a re-pair, else "".
func (c *Client) Pair(ctx context.Context, r wire.PairRequest, oldKey string) (wire.PairResponse, error) {
	var out wire.PairResponse
	_, err := c.call(ctx, http.MethodPost, wire.PathPair, oldKey, r, &out, DefaultTimeout)
	return out, err
}

// Claim long-polls POST /pair/claim once. A pending pairing gives Status wire.ClaimPending; an
// expired or collected one gives an *Error with wire.ErrExpired.
func (c *Client) Claim(ctx context.Context, claim string) (wire.ClaimResponse, error) {
	var out wire.ClaimResponse
	_, err := c.call(ctx, http.MethodPost, wire.PathPairClaim, claim, wire.Empty{}, &out, ClaimTimeout)
	return out, err
}

// Standing mints the standing token (POST /standing). The first call confirms a new key.
func (c *Client) Standing(ctx context.Context) (wire.Standing, error) {
	var out wire.Standing
	err := c.authed(ctx, http.MethodPost, wire.PathStanding, wire.Empty{}, &out)
	return out, err
}

// Agent creates, re-mints or reopens an agent (POST /agents).
func (c *Client) Agent(ctx context.Context, r wire.AgentRequest) (wire.Agent, error) {
	var out wire.Agent
	err := c.authed(ctx, http.MethodPost, wire.PathAgents, r, &out)
	return out, err
}

// Close closes agents of this pairing (POST /agents/close), at most wire.MaxIDs per request.
func (c *Client) Close(ctx context.Context, userIDs []string, reason string) error {
	for _, ids := range chunks(userIDs) {
		var out wire.CloseResponse
		if err := c.authed(ctx, http.MethodPost, wire.PathAgentsClose, wire.CloseRequest{UserIDs: ids, Reason: reason}, &out); err != nil {
			return err
		}
	}
	return nil
}

// Alive checks agents (POST /agents/alive), at most wire.MaxIDs per request, marks the open ones
// active when touch is set, and returns the ids the registrar does not know and those that are closed.
func (c *Client) Alive(ctx context.Context, userIDs []string, touch bool) (unknown, closed []string, err error) {
	unknown, closed = []string{}, []string{}
	for _, ids := range chunks(userIDs) {
		var out wire.AliveResponse
		if err := c.authed(ctx, http.MethodPost, wire.PathAgentsAlive, wire.AliveRequest{UserIDs: ids, Touch: touch}, &out); err != nil {
			return nil, nil, err
		}
		unknown = append(unknown, out.Unknown...)
		closed = append(closed, out.Closed...)
	}
	return unknown, closed, nil
}

// Me returns the pairing and the owner's settings (GET /me).
func (c *Client) Me(ctx context.Context) (wire.Me, error) {
	var out wire.Me
	err := c.authed(ctx, http.MethodGet, wire.PathMe, nil, &out)
	return out, err
}

// Unpair revokes this pairing (POST /unpair).
func (c *Client) Unpair(ctx context.Context) error {
	var out wire.Empty
	return c.authed(ctx, http.MethodPost, wire.PathUnpair, wire.Empty{}, &out)
}

func chunks(ids []string) [][]string {
	var out [][]string
	for len(ids) > 0 {
		n := min(len(ids), wire.MaxIDs)
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	return out
}
