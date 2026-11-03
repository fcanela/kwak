//go:build integration && linux

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/test/integration/ittest"
)

// probe is one fact probe of spec 10.3. Every measurement is logged as a line starting with
// "FACT <id>", so FACTS.md is built from the test output.
type probe struct {
	t   *testing.T
	env *ittest.Env
	id  string
	ctx context.Context
}

// startProbe returns nil on the host, where ittest runs the test inside the runner.
func startProbe(t *testing.T, id string) *probe {
	t.Helper()
	env := ittest.Start(t)
	if env == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	t.Cleanup(cancel)
	p := &probe{t: t, env: env, id: id, ctx: ctx}
	t.Cleanup(func() {
		res := "PASS"
		if t.Failed() {
			res = "FAIL"
		}
		t.Logf("FACT %s RESULT %s", id, res)
	})
	return p
}

// check records a measurement the spec relies on; a contradiction fails the probe.
func (p *probe) check(ok bool, what, format string, args ...any) {
	p.t.Helper()
	res := "PASS"
	if !ok {
		res = "FAIL"
	}
	p.t.Logf("FACT %s %s %s: %s", p.id, res, what, fmt.Sprintf(format, args...))
	if !ok {
		p.t.Errorf("%s: %s contradicts the spec", p.id, what)
	}
}

// note records a measurement that cannot fail the probe.
func (p *probe) note(what, format string, args ...any) {
	p.t.Helper()
	p.t.Logf("FACT %s NOTE %s: %s", p.id, what, fmt.Sprintf(format, args...))
}

// must stops the probe on a setup step that is not itself the fact under test.
func (p *probe) must(err error, what string) {
	p.t.Helper()
	if err != nil {
		p.t.Fatalf("%s: setup %s: %s", p.id, what, describe(err))
	}
}

func (p *probe) client(token string) *matrix.Client {
	return &matrix.Client{
		Base:         p.env.Homeserver,
		Token:        token,
		ForwardedFor: ittest.ForwardedFor,
		AllowHTTP:    true,
		HTTP:         p.env.HTTP,
	}
}

func (p *probe) as() *matrix.Client { return p.client(p.env.Registration.ASToken) }

func (p *probe) asUser(userID string) *matrix.Client { return p.as().As(userID) }

func mxid(localpart string) string { return "@" + localpart + ":" + ittest.ServerName }

// agent registers a passwordless appservice user and gives it the localpart as display name, as the
// registrar does.
func (p *probe) agent(localpart, deviceID string) matrix.Login {
	p.t.Helper()
	l, err := p.as().RegisterAppservice(p.ctx, localpart, deviceID)
	p.must(err, "register "+localpart)
	return l
}

// agentsRoom creates the agents room as admin with the spec 3.11 body.
func (p *probe) agentsRoom() string {
	p.t.Helper()
	room, err := p.client(p.env.Admin.AccessToken).CreateRoom(p.ctx, agentsRoomBody())
	p.must(err, "create the 3.11 agents room")
	return room
}

// agentsRoomBody is the spec 3.11 body with the test server name.
func agentsRoomBody() map[string]any {
	return map[string]any{
		"preset":          "public_chat",
		"visibility":      "private",
		"name":            "agents",
		"room_alias_name": "agents",
		"topic":           "kwak agents room. A thread's first message is a question; replies in it are answers.",
		"power_level_content_override": map[string]any{
			"users":          map[string]any{mxid("registrar"): 50},
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

// dmBody is the spec 3.5 DM create body with the test server name.
func dmBody(invite string) map[string]any {
	return map[string]any{
		"preset":    "private_chat",
		"is_direct": true,
		"invite":    []string{invite},
		"name":      "kwak registrar",
		"power_level_content_override": map[string]any{
			"state_default": 100,
			"invite":        100,
			"events":        map[string]any{"m.room.encryption": 100},
		},
	}
}

// syncFilter is the spec 5.3 filter for room.
func syncFilter(room string) string {
	b, _ := json.Marshal(map[string]any{
		"presence":     map[string]any{"types": []string{}},
		"account_data": map[string]any{"types": []string{}},
		"room": map[string]any{
			"rooms":        []string{room},
			"account_data": map[string]any{"types": []string{}},
			"ephemeral":    map[string]any{"types": []string{}},
			"state":        map[string]any{"types": []string{}, "lazy_load_members": true},
			"timeline":     map[string]any{"limit": 100, "types": []string{"m.room.message"}},
		},
	})
	return string(b)
}

func text(body string) map[string]any { return map[string]any{"msgtype": "m.text", "body": body} }

// raw sends one request the matrix client has no method for. xff false leaves X-Forwarded-For out.
func (p *probe) raw(method, path, token string, xff bool, body any) (int, []byte) {
	p.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			p.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(p.ctx, method, p.env.Homeserver+path, r)
	if err != nil {
		p.t.Fatal(err)
	}
	if xff {
		req.Header.Set("X-Forwarded-For", ittest.ForwardedFor)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := p.env.HTTP.Do(req)
	if err != nil {
		p.t.Fatalf("%s: %s %s: %v", p.id, method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b
}

// esc joins path parts, each escaped.
func esc(parts ...string) string {
	var b strings.Builder
	for _, s := range parts {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

const v3 = "/_matrix/client/v3"

// whoami returns the status, the user id and the errcode of /account/whoami for token, optionally
// asserting asUser.
func (p *probe) whoami(token, asUser string) (int, string, string) {
	p.t.Helper()
	path := v3 + "/account/whoami"
	if asUser != "" {
		path += "?user_id=" + url.QueryEscape(asUser)
	}
	st, b := p.raw(http.MethodGet, path, token, true, nil)
	var r struct {
		UserID  string `json:"user_id"`
		Errcode string `json:"errcode"`
	}
	json.Unmarshal(b, &r)
	return st, r.UserID, r.Errcode
}

// state reads one state event's content with token. A missing event gives the status and nil.
func (p *probe) state(token, room, typ, key string) (int, map[string]any) {
	p.t.Helper()
	path := v3 + "/rooms" + esc(room, "state", typ)
	if key != "" {
		path += esc(key)
	}
	st, b := p.raw(http.MethodGet, path, token, true, nil)
	if st != http.StatusOK {
		return st, nil
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return st, m
}

// profileField reads one profile field with token; "" when absent.
func (p *probe) profileField(token, userID, field string) (int, string) {
	p.t.Helper()
	st, b := p.raw(http.MethodGet, v3+"/profile"+esc(userID, field), token, true, nil)
	var m map[string]any
	json.Unmarshal(b, &m)
	s, _ := m[field].(string)
	return st, s
}

// describe prints an error as status and errcode, or "ok".
func describe(err error) string {
	if err == nil {
		return "ok"
	}
	var me *matrix.Error
	if errors.As(err, &me) {
		return me.Error()
	}
	return err.Error()
}

func errcode(err error) string {
	var me *matrix.Error
	if errors.As(err, &me) {
		return me.Code
	}
	return ""
}

func bodyOf(ev matrix.Event) string {
	var c struct {
		Body string `json:"body"`
	}
	json.Unmarshal(ev.Content, &c)
	return c.Body
}

// netMode tells the two runner layouts apart from inside: with a shared network namespace both
// names resolve to tuwunel's address. The host-side switch is not passed into the runner.
func netMode() string {
	hs, err1 := net.LookupHost("tuwunel")
	rn, err2 := net.LookupHost(ittest.RunnerAlias)
	switch {
	case err1 != nil || err2 != nil:
		return fmt.Sprintf("unknown (lookup: %v, %v)", err1, err2)
	case slices.Equal(hs, rn):
		return fmt.Sprintf("shared network namespace (both names resolve to %v)", hs)
	default:
		return fmt.Sprintf("spec layout over the internal bridge (tuwunel %v, runner %v)", hs, rn)
	}
}

// hit is one request tuwunel made to the appservice.
type hit struct {
	Method, Path     string
	BearerMatches    bool
	BearerPresent    bool
	QueryMatches     bool
	QueryPresent     bool
	TransactionCount int
}

// recorder is the appservice side for the probes: it answers every transaction and ping with
// 200 {}, so tuwunel's queue never blocks, and keeps what it saw.
type recorder struct {
	hsToken string
	mu      sync.Mutex
	hits    []hit
	events  []matrix.Event
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := hit{Method: r.Method, Path: r.URL.Path}
	if a := r.Header.Get("Authorization"); a != "" {
		h.BearerPresent = true
		h.BearerMatches = a == "Bearer "+rc.hsToken
	}
	if q := r.URL.Query().Get("access_token"); q != "" {
		h.QueryPresent = true
		h.QueryMatches = q == rc.hsToken
	}
	var txn struct {
		Events []matrix.Event `json:"events"`
	}
	if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/_matrix/app/v1/transactions/") {
		json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&txn)
		h.TransactionCount = len(txn.Events)
	}
	rc.mu.Lock()
	rc.hits = append(rc.hits, h)
	rc.events = append(rc.events, txn.Events...)
	rc.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
}

func (p *probe) serveRecorder() *recorder {
	rc := &recorder{hsToken: p.env.Registration.HSToken}
	p.env.ServeAppservice(p.t, rc)
	return rc
}

// waitHit waits for a request whose path starts with prefix.
func (rc *recorder) waitHit(ctx context.Context, prefix string, d time.Duration) (hit, bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		rc.mu.Lock()
		for _, h := range rc.hits {
			if strings.HasPrefix(h.Path, prefix) {
				rc.mu.Unlock()
				return h, true
			}
		}
		rc.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	return hit{}, false
}

// waitEvent waits for a transaction event that match accepts.
func (rc *recorder) waitEvent(ctx context.Context, d time.Duration, match func(matrix.Event) bool) (matrix.Event, bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		rc.mu.Lock()
		for _, ev := range rc.events {
			if match(ev) {
				rc.mu.Unlock()
				return ev, true
			}
		}
		rc.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	return matrix.Event{}, false
}

func (rc *recorder) transactionHits() []hit {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []hit
	for _, h := range rc.hits {
		if strings.HasPrefix(h.Path, "/_matrix/app/v1/transactions/") {
			out = append(out, h)
		}
	}
	return out
}
