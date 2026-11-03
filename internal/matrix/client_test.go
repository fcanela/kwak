package matrix_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
)

type seen struct {
	Method, URI, Path string
	Query             url.Values
	Header            http.Header
	Body              []byte
}

type server struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []seen
}

func (s *server) all() []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seen(nil), s.reqs...)
}

func (s *server) last(t *testing.T) seen {
	t.Helper()
	r := s.all()
	if len(r) == 0 {
		t.Fatal("no request reached the server")
	}
	return r[len(r)-1]
}

func newServer(t *testing.T, h http.HandlerFunc) *server {
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, _, _ := strings.Cut(r.RequestURI, "?")
		s.mu.Lock()
		s.reqs = append(s.reqs, seen{r.Method, r.RequestURI, path, r.URL.Query(), r.Header.Clone(), b})
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func client(s *server) *matrix.Client {
	return &matrix.Client{
		Base:         s.URL,
		Token:        "tok-secret",
		ForwardedFor: "10.1.2.3",
		AllowHTTP:    true,
		HTTP:         matrix.NewHTTPClient(),
	}
}

func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("body %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want %q: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

const (
	room  = "!room:kwak.test"
	eroom = "%21room:kwak.test"
	alias = "#agents:kwak.test"
	agent = "@agent.x:kwak.test"
)

func TestEndpoints(t *testing.T) {
	type tc struct {
		name   string
		run    func(context.Context, *matrix.Client) (any, error)
		method string
		path   string
		query  url.Values
		body   string // "" means no body
		resp   string
		want   any
	}
	cases := []tc{
		{
			name: "register appservice",
			run: func(ctx context.Context, c *matrix.Client) (any, error) {
				return c.RegisterAppservice(ctx, "agent.x", "KWAK")
			},
			method: "POST", path: "/_matrix/client/v3/register",
			body: `{"type":"m.login.application_service","username":"agent.x","device_id":"KWAK"}`,
			resp: `{"user_id":"@agent.x:kwak.test","access_token":"t2","device_id":"KWAK"}`,
			want: matrix.Login{UserID: agent, AccessToken: "t2", DeviceID: "KWAK"},
		},
		{
			name: "login appservice",
			run: func(ctx context.Context, c *matrix.Client) (any, error) {
				return c.LoginAppservice(ctx, agent, "KWAK-1")
			},
			method: "POST", path: "/_matrix/client/v3/login",
			body: `{"type":"m.login.application_service","identifier":{"type":"m.id.user","user":"@agent.x:kwak.test"},"device_id":"KWAK-1"}`,
			resp: `{"user_id":"@agent.x:kwak.test","access_token":"t3","device_id":"KWAK-1"}`,
			want: matrix.Login{UserID: agent, AccessToken: "t3", DeviceID: "KWAK-1"},
		},
		{
			name:   "logout",
			run:    func(ctx context.Context, c *matrix.Client) (any, error) { return nil, c.Logout(ctx) },
			method: "POST", path: "/_matrix/client/v3/logout", body: `{}`, resp: `{}`,
		},
		{
			name:   "logout all",
			run:    func(ctx context.Context, c *matrix.Client) (any, error) { return nil, c.LogoutAll(ctx) },
			method: "POST", path: "/_matrix/client/v3/logout/all", body: `{}`, resp: `{}`,
		},
		{
			name:   "join alias",
			run:    func(ctx context.Context, c *matrix.Client) (any, error) { return c.Join(ctx, alias) },
			method: "POST", path: "/_matrix/client/v3/join/%23agents:kwak.test", body: `{}`,
			resp: `{"room_id":"!room:kwak.test"}`, want: room,
		},
		{
			name:   "join id",
			run:    func(ctx context.Context, c *matrix.Client) (any, error) { return c.Join(ctx, room) },
			method: "POST", path: "/_matrix/client/v3/join/" + eroom, body: `{}`,
			resp: `{"room_id":"!room:kwak.test"}`, want: room,
		},
		{
			name:   "leave",
			run:    func(ctx context.Context, c *matrix.Client) (any, error) { return nil, c.Leave(ctx, room) },
			method: "POST", path: "/_matrix/client/v3/rooms/" + eroom + "/leave", body: `{}`, resp: `{}`,
		},
		{
			name: "invite",
			run: func(ctx context.Context, c *matrix.Client) (any, error) {
				return nil, c.Invite(ctx, room, "@ann:kwak.test")
			},
			method: "POST", path: "/_matrix/client/v3/rooms/" + eroom + "/invite",
			body: `{"user_id":"@ann:kwak.test"}`, resp: `{}`,
		},
		{
			name:   "resolve alias",
			run:    func(ctx context.Context, c *matrix.Client) (any, error) { return c.ResolveAlias(ctx, alias) },
			method: "GET", path: "/_matrix/client/v3/directory/room/%23agents:kwak.test",
			resp: `{"room_id":"!room:kwak.test","servers":["kwak.test"]}`, want: room,
		},
		{
			name: "create room",
			run: func(ctx context.Context, c *matrix.Client) (any, error) {
				return c.CreateRoom(ctx, map[string]any{"preset": "private_chat", "is_direct": true})
			},
			method: "POST", path: "/_matrix/client/v3/createRoom",
			body: `{"preset":"private_chat","is_direct":true}`,
			resp: `{"room_id":"!new:kwak.test"}`, want: "!new:kwak.test",
		},
		{
			name: "set display name",
			run: func(ctx context.Context, c *matrix.Client) (any, error) {
				return nil, c.SetDisplayName(ctx, agent, "agent.x")
			},
			method: "PUT", path: "/_matrix/client/v3/profile/@agent.x:kwak.test/displayname",
			body: `{"displayname":"agent.x"}`, resp: `{}`,
		},
		{
			name: "clear avatar",
			run: func(ctx context.Context, c *matrix.Client) (any, error) {
				return nil, c.SetAvatarURL(ctx, agent, "")
			},
			method: "PUT", path: "/_matrix/client/v3/profile/@agent.x:kwak.test/avatar_url",
			body: `{"avatar_url":""}`, resp: `{}`,
		},
		{
			name:   "profile exists",
			run:    func(ctx context.Context, c *matrix.Client) (any, error) { return c.ProfileExists(ctx, agent) },
			method: "GET", path: "/_matrix/client/v3/profile/@agent.x:kwak.test",
			resp: `{"displayname":"agent.x"}`, want: true,
		},
		{
			name: "send",
			run: func(ctx context.Context, c *matrix.Client) (any, error) {
				return c.Send(ctx, room, "kwak-0123456789abcdef", map[string]any{"msgtype": "m.text", "body": "hi", "m.mentions": map[string]any{}})
			},
			method: "PUT", path: "/_matrix/client/v3/rooms/" + eroom + "/send/m.room.message/kwak-0123456789abcdef",
			body: `{"msgtype":"m.text","body":"hi","m.mentions":{}}`,
			resp: `{"event_id":"$sent"}`, want: "$sent",
		},
		{
			name: "sync full",
			run: func(ctx context.Context, c *matrix.Client) (any, error) {
				r, err := c.Sync(ctx, "s1", `{"room":{"rooms":["!a"]}}`, 30*time.Second)
				if err != nil {
					return nil, err
				}
				return r.NextBatch, nil
			},
			method: "GET", path: "/_matrix/client/v3/sync",
			query: url.Values{"since": {"s1"}, "timeout": {"30000"}, "set_presence": {"offline"}, "filter": {`{"room":{"rooms":["!a"]}}`}},
			resp:  `{"next_batch":"s2"}`, want: "s2",
		},
		{
			name: "sync first",
			run: func(ctx context.Context, c *matrix.Client) (any, error) {
				r, err := c.Sync(ctx, "", "", 0)
				if err != nil {
					return nil, err
				}
				return r.NextBatch, nil
			},
			method: "GET", path: "/_matrix/client/v3/sync",
			query: url.Values{"timeout": {"0"}, "set_presence": {"offline"}},
			resp:  `{"next_batch":"s1"}`, want: "s1",
		},
		{
			name: "messages",
			run: func(ctx context.Context, c *matrix.Client) (any, error) {
				r, err := c.Messages(ctx, room, "p1", 100, `{"types":["m.room.message"]}`)
				if err != nil {
					return nil, err
				}
				return *r, nil
			},
			method: "GET", path: "/_matrix/client/v3/rooms/" + eroom + "/messages",
			query: url.Values{"dir": {"b"}, "from": {"p1"}, "limit": {"100"}, "filter": {`{"types":["m.room.message"]}`}},
			resp:  `{"chunk":[],"start":"p1"}`,
			want:  matrix.MessagesResponse{Chunk: []matrix.Event{}, Start: "p1"},
		},
		{
			name: "event",
			run: func(ctx context.Context, c *matrix.Client) (any, error) {
				ev, err := c.Event(ctx, room, "$ev/1+x")
				if err != nil {
					return nil, err
				}
				return ev.EventID + " " + ev.Sender + " " + string(ev.Content), nil
			},
			method: "GET", path: "/_matrix/client/v3/rooms/" + eroom + "/event/$ev%2F1+x",
			resp: `{"event_id":"$ev/1+x","sender":"@ann:kwak.test","type":"m.room.message","origin_server_ts":5,"content":{"body":"b"}}`,
			want: `$ev/1+x @ann:kwak.test {"body":"b"}`,
		},
		{
			name: "thread replies",
			run: func(ctx context.Context, c *matrix.Client) (any, error) {
				evs, err := c.ThreadReplies(ctx, room, "$root", 50)
				if err != nil {
					return nil, err
				}
				return len(evs), nil
			},
			method: "GET", path: "/_matrix/client/v1/rooms/" + eroom + "/relations/$root/m.thread",
			query: url.Values{"limit": {"50"}, "dir": {"b"}},
			resp:  `{"chunk":[{"event_id":"$a"},{"event_id":"$b"}]}`, want: 2,
		},
		{
			name:   "joined members",
			run:    func(ctx context.Context, c *matrix.Client) (any, error) { return c.JoinedMembers(ctx, room) },
			method: "GET", path: "/_matrix/client/v3/rooms/" + eroom + "/joined_members",
			resp: `{"joined":{"@b:kwak.test":{"display_name":"b"},"@a:kwak.test":{}}}`,
			want: []string{"@a:kwak.test", "@b:kwak.test"},
		},
		{
			name:   "room encrypted",
			run:    func(ctx context.Context, c *matrix.Client) (any, error) { return c.RoomEncrypted(ctx, room) },
			method: "GET", path: "/_matrix/client/v3/rooms/" + eroom + "/state/m.room.encryption",
			resp: `{"algorithm":"m.megolm.v1.aes-sha2"}`, want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t, reply(200, tc.resp))
			got, err := tc.run(t.Context(), client(s))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != nil && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("result = %#v, want %#v", got, tc.want)
			}
			r := s.last(t)
			if r.Method != tc.method || r.Path != tc.path {
				t.Fatalf("request = %s %s, want %s %s", r.Method, r.Path, tc.method, tc.path)
			}
			q := tc.query
			if q == nil {
				q = url.Values{}
			}
			if !reflect.DeepEqual(r.Query, q) {
				t.Fatalf("query = %v, want %v", r.Query, q)
			}
			if tc.body == "" {
				if len(r.Body) != 0 {
					t.Fatalf("unexpected body %q", r.Body)
				}
			} else {
				jsonEqual(t, r.Body, tc.body)
				if ct := r.Header.Get("Content-Type"); ct != "application/json" {
					t.Fatalf("Content-Type = %q", ct)
				}
			}
			if a := r.Header.Get("Authorization"); a != "Bearer tok-secret" {
				t.Fatalf("Authorization = %q", a)
			}
			if x := r.Header.Get("X-Forwarded-For"); x != "10.1.2.3" {
				t.Fatalf("X-Forwarded-For = %q", x)
			}
			if strings.Contains(r.URI, "access_token") || strings.Contains(r.URI, "tok-secret") {
				t.Fatalf("token in URL: %s", r.URI)
			}
		})
	}
}

func TestHeadersOptional(t *testing.T) {
	s := newServer(t, reply(200, `{}`))
	c := &matrix.Client{Base: s.URL, AllowHTTP: true, HTTP: matrix.NewHTTPClient()}
	if err := c.Leave(t.Context(), room); err != nil {
		t.Fatal(err)
	}
	r := s.last(t)
	if _, ok := r.Header["Authorization"]; ok {
		t.Fatal("Authorization sent without a token")
	}
	if _, ok := r.Header["X-Forwarded-For"]; ok {
		t.Fatal("X-Forwarded-For sent without ForwardedFor")
	}
}

func TestAppserviceAssertion(t *testing.T) {
	s := newServer(t, reply(200, `{"room_id":"!room:kwak.test"}`))
	base := client(s)
	base.AsDevice = "stale"

	as := base.As(agent)
	if as == base || base.AsUser != "" || base.AsDevice != "stale" {
		t.Fatal("As changed the original")
	}
	if as.Token != base.Token || as.ForwardedFor != base.ForwardedFor || as.Base != base.Base || !as.AllowHTTP || as.HTTP == nil {
		t.Fatalf("As dropped fields: %+v", as)
	}
	if _, err := as.Join(t.Context(), alias); err != nil {
		t.Fatal(err)
	}
	if q := s.last(t).Query; !reflect.DeepEqual(q, url.Values{"user_id": {agent}}) {
		t.Fatalf("query = %v", q)
	}

	dev := client(s)
	dev.AsUser, dev.AsDevice = "@agent.o.listener:kwak.test", "KWAK-3f9a1c2e"
	if err := dev.Logout(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := url.Values{"user_id": {"@agent.o.listener:kwak.test"}, "device_id": {"KWAK-3f9a1c2e"}}
	if q := s.last(t).Query; !reflect.DeepEqual(q, want) {
		t.Fatalf("query = %v, want %v", q, want)
	}

	nouser := client(s)
	nouser.AsDevice = "KWAK"
	if err := nouser.LogoutAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if q := s.last(t).Query; len(q) != 0 {
		t.Fatalf("device_id sent without AsUser: %v", q)
	}

	if _, err := as.Sync(t.Context(), "s1", "", time.Second); err != nil {
		t.Fatal(err)
	}
	if q := s.last(t).Query; q.Get("user_id") != agent || q.Get("since") != "s1" {
		t.Fatalf("query = %v", q)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name   string
		h      http.HandlerFunc
		status int
		code   string
		msg    string
		retry  time.Duration
		body   bool
	}{
		{
			name:   "retry_after_ms",
			h:      reply(429, `{"errcode":"M_LIMIT_EXCEEDED","error":"Too many requests","retry_after_ms":1500}`),
			status: 429, code: matrix.ErrLimit, msg: "Too many requests", retry: 1500 * time.Millisecond, body: true,
		},
		{
			name: "Retry-After header",
			h: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "3")
				reply(429, `{"errcode":"M_LIMIT_EXCEEDED"}`)(w, r)
			},
			status: 429, code: matrix.ErrLimit, retry: 3 * time.Second, body: true,
		},
		{
			name: "ms wins over header",
			h: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "9")
				reply(429, `{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":200}`)(w, r)
			},
			status: 429, code: matrix.ErrLimit, retry: 200 * time.Millisecond, body: true,
		},
		{
			name:   "user in use",
			h:      reply(400, `{"errcode":"M_USER_IN_USE","error":"taken"}`),
			status: 400, code: matrix.ErrUserInUse, msg: "taken", body: true,
		},
		{
			name:   "unknown token",
			h:      reply(401, `{"errcode":"M_UNKNOWN_TOKEN","error":"gone","soft_logout":false}`),
			status: 401, code: matrix.ErrUnknownToken, msg: "gone", body: true,
		},
		{
			name:   "not json",
			h:      reply(502, `<html>bad gateway</html>`),
			status: 502, msg: "Bad Gateway",
		},
		{
			name:   "empty",
			h:      reply(500, ``),
			status: 500, msg: "Internal Server Error",
		},
		{
			name:   "huge error body",
			h:      reply(500, `{"errcode":"M_UNKNOWN","error":"`+strings.Repeat("x", matrix.SmallLimit)+`"}`),
			status: 500, msg: "Internal Server Error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t, tc.h)
			_, err := client(s).Join(t.Context(), alias)
			var me *matrix.Error
			if !errors.As(err, &me) {
				t.Fatalf("err = %v, want *matrix.Error", err)
			}
			if me.Status != tc.status || me.Code != tc.code || me.Msg != tc.msg || me.RetryAfter != tc.retry {
				t.Fatalf("got %+v", me)
			}
			if (len(me.Body) > 0) != tc.body {
				t.Fatalf("Body = %q", me.Body)
			}
			if tc.code != "" && !matrix.IsCode(err, tc.code) {
				t.Fatal("IsCode false")
			}
		})
	}
}

func TestErrorString(t *testing.T) {
	cases := map[string]*matrix.Error{
		"matrix: HTTP 429 M_LIMIT_EXCEEDED: slow down": {Status: 429, Code: "M_LIMIT_EXCEEDED", Msg: "slow down"},
		"matrix: HTTP 400 M_USER_IN_USE":               {Status: 400, Code: "M_USER_IN_USE"},
		"matrix: HTTP 502: Bad Gateway":                {Status: 502, Msg: "Bad Gateway"},
	}
	for want, e := range cases {
		if got := e.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	}
	if matrix.IsCode(errors.New("x"), matrix.ErrLimit) || matrix.IsCode(nil, matrix.ErrLimit) {
		t.Error("IsCode true for a non-matrix error")
	}
}

func TestNotFoundIsFalse(t *testing.T) {
	s := newServer(t, reply(404, `{"errcode":"M_NOT_FOUND","error":"nope"}`))
	c := client(s)
	ok, err := c.ProfileExists(t.Context(), agent)
	if ok || err != nil {
		t.Fatalf("ProfileExists = %v, %v", ok, err)
	}
	ok, err = c.RoomEncrypted(t.Context(), room)
	if ok || err != nil {
		t.Fatalf("RoomEncrypted = %v, %v", ok, err)
	}

	s2 := newServer(t, reply(403, `{"errcode":"M_FORBIDDEN"}`))
	c2 := client(s2)
	if ok, err := c2.ProfileExists(t.Context(), agent); ok || !matrix.IsCode(err, matrix.ErrForbidden) {
		t.Fatalf("ProfileExists = %v, %v", ok, err)
	}
	if ok, err := c2.RoomEncrypted(t.Context(), room); ok || !matrix.IsCode(err, matrix.ErrForbidden) {
		t.Fatalf("RoomEncrypted = %v, %v", ok, err)
	}
}

func TestLargeResponses(t *testing.T) {
	t.Run("3000 members", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`{"joined":{`)
		for i := range 3000 {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"@agent.owner%04d.0123456789abcdef:kwak.test":{"display_name":"agent.owner%04d.0123456789abcdef","avatar_url":null}`, i, i)
		}
		b.WriteString(`}}`)
		if b.Len() <= matrix.SmallLimit {
			t.Fatalf("test body too small: %d", b.Len())
		}
		s := newServer(t, reply(200, b.String()))
		ids, err := client(s).JoinedMembers(t.Context(), room)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 3000 || ids[0] != "@agent.owner0000.0123456789abcdef:kwak.test" {
			t.Fatalf("got %d ids, first %q", len(ids), ids[0])
		}
	})
	t.Run("50 replies of 16000 chars", func(t *testing.T) {
		text := strings.Repeat("é", 16000)
		var evs []map[string]any
		for i := range 50 {
			evs = append(evs, map[string]any{
				"event_id": fmt.Sprintf("$r%d", i), "sender": agent, "type": "m.room.message",
				"origin_server_ts": i, "content": map[string]any{"msgtype": "m.text", "body": text},
			})
		}
		body, _ := json.Marshal(map[string]any{"chunk": evs})
		s := newServer(t, reply(200, string(body)))
		got, err := client(s).ThreadReplies(t.Context(), room, "$root", 50)
		if err != nil {
			t.Fatal(err)
		}
		var c struct{ Body string }
		if err := json.Unmarshal(got[49].Content, &c); err != nil || len(got) != 50 || c.Body != text {
			t.Fatalf("got %d replies, err %v", len(got), err)
		}
	})
	big := func(n int) string { return `{"chunk":[],"start":"` + strings.Repeat("x", n) + `"}` }
	t.Run("small limit", func(t *testing.T) {
		pad := matrix.SmallLimit - len(`{"event_id":""}`)
		s := newServer(t, reply(200, `{"event_id":"`+strings.Repeat("x", pad)+`"}`))
		if _, err := client(s).Event(t.Context(), room, "$e"); err != nil {
			t.Fatalf("exactly 64 KiB: %v", err)
		}
		s2 := newServer(t, reply(200, `{"event_id":"`+strings.Repeat("x", matrix.SmallLimit)+`"}`))
		if _, err := client(s2).Event(t.Context(), room, "$e"); !errors.Is(err, matrix.ErrTooLarge) {
			t.Fatalf("err = %v, want ErrTooLarge", err)
		}
	})
	t.Run("large limit", func(t *testing.T) {
		exact := big(matrix.LargeLimit - len(big(0)))
		s := newServer(t, reply(200, exact))
		if _, err := client(s).Messages(t.Context(), room, "", 100, ""); err != nil {
			t.Fatalf("exactly 8 MiB: %v", err)
		}
		s2 := newServer(t, reply(200, exact+" "))
		if _, err := client(s2).Messages(t.Context(), room, "", 100, ""); !errors.Is(err, matrix.ErrTooLarge) {
			t.Fatalf("err = %v, want ErrTooLarge", err)
		}
		s3 := newServer(t, reply(200, `{"next_batch":"`+strings.Repeat("x", matrix.LargeLimit)+`"}`))
		if _, err := client(s3).Sync(t.Context(), "", "", 0); !errors.Is(err, matrix.ErrTooLarge) {
			t.Fatalf("sync err = %v, want ErrTooLarge", err)
		}
	})
}

func TestBaseScheme(t *testing.T) {
	s := newServer(t, reply(200, `{}`))
	for _, base := range []string{s.URL, "", "ftp://kwak.test", "kwak.test", "HTTP://kwak.test"} {
		c := client(s)
		c.Base, c.AllowHTTP = base, false
		if err := c.Leave(t.Context(), room); err == nil {
			t.Errorf("base %q accepted without AllowHTTP", base)
		}
	}
	c := client(s)
	c.Base = "ftp://kwak.test"
	if err := c.Leave(t.Context(), room); err == nil {
		t.Error("ftp accepted with AllowHTTP")
	}
	if n := len(s.all()); n != 0 {
		t.Fatalf("%d requests reached the server", n)
	}

	c = client(s)
	c.Base = s.URL + "/"
	if err := c.Leave(t.Context(), room); err != nil {
		t.Fatal(err)
	}
	if p := s.last(t).Path; p != "/_matrix/client/v3/rooms/"+eroom+"/leave" {
		t.Fatalf("trailing slash kept: %s", p)
	}

	c = client(s)
	c.HTTP = nil
	if err := c.Leave(t.Context(), room); err == nil {
		t.Fatal("nil HTTP accepted")
	}
}

func TestHTTPSAndRedirects(t *testing.T) {
	var plainHits int
	var plainAuth []string
	var mu sync.Mutex
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		plainHits++
		plainAuth = append(plainAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		io.WriteString(w, `{"room_id":"!stolen:kwak.test"}`)
	}))
	t.Cleanup(plain.Close)

	var tlsPaths []string
	var tlsAuth []string
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tlsPaths = append(tlsPaths, r.URL.Path)
		tlsAuth = append(tlsAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/to-http"):
			http.Redirect(w, r, plain.URL+"/_matrix/client/v3/join/x", http.StatusTemporaryRedirect)
		case strings.HasSuffix(r.URL.Path, "/same-host"):
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		default:
			io.WriteString(w, `{"room_id":"!room:kwak.test"}`)
		}
	}))
	t.Cleanup(tls.Close)

	hc := matrix.NewHTTPClient()
	hc.Transport = tls.Client().Transport
	c := &matrix.Client{Base: tls.URL, Token: "tok-secret", HTTP: hc}

	if !strings.HasPrefix(tls.URL, "https://") {
		t.Fatalf("tls url %s", tls.URL)
	}
	if id, err := c.Join(t.Context(), "!ok"); err != nil || id != room {
		t.Fatalf("https join = %q, %v", id, err)
	}

	for _, target := range []string{"to-http", "same-host"} {
		_, err := c.Join(t.Context(), target)
		var me *matrix.Error
		if !errors.As(err, &me) || me.Status < 300 || me.Status > 399 {
			t.Fatalf("%s: err = %v, want a 3xx *matrix.Error", target, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if plainHits != 0 {
		t.Fatalf("redirect followed to http://, Authorization %q", plainAuth)
	}
	for _, p := range tlsPaths {
		if p == "/elsewhere" {
			t.Fatal("same-host redirect followed")
		}
	}
	if len(tlsPaths) != 3 {
		t.Fatalf("tls paths = %v", tlsPaths)
	}
	for _, a := range tlsAuth {
		if a != "Bearer tok-secret" {
			t.Fatalf("Authorization = %q", a)
		}
	}
}

type deadlineDoer struct {
	got  []time.Time
	none bool
}

func (d *deadlineDoer) Do(r *http.Request) (*http.Response, error) {
	dl, ok := r.Context().Deadline()
	if !ok {
		d.none = true
	}
	d.got = append(d.got, dl)
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"next_batch":"n"}`)),
	}, nil
}

func TestDeadlines(t *testing.T) {
	d := &deadlineDoer{}
	c := &matrix.Client{Base: "https://kwak.test", HTTP: d}
	start := time.Now()
	ctx := context.Background()

	if err := c.Leave(ctx, room); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sync(ctx, "", "", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := c.Sync(short, "", "", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if d.none {
		t.Fatal("a request had no deadline")
	}
	near := func(got time.Time, want time.Duration) bool {
		dt := got.Sub(start)
		return dt > want-5*time.Second && dt <= want+time.Second
	}
	if !near(d.got[0], matrix.DefaultTimeout) {
		t.Errorf("default deadline in %v", d.got[0].Sub(start))
	}
	if !near(d.got[1], 30*time.Second+matrix.SyncSlack) {
		t.Errorf("sync deadline in %v", d.got[1].Sub(start))
	}
	if d.got[2].Sub(start) > 2*time.Second+time.Second {
		t.Errorf("caller's shorter deadline lost: %v", d.got[2].Sub(start))
	}
}

// noURL fails when err's text holds the base, a path or a query of the request (spec I4).
func noURL(t *testing.T, err error, base string) {
	t.Helper()
	for _, bad := range []string{"tok-secret", base, "/_matrix", "user_id", "kwak.test", "%21room"} {
		if strings.Contains(err.Error(), bad) {
			t.Fatalf("error holds %q: %v", bad, err)
		}
	}
}

func TestTransportError(t *testing.T) {
	s := newServer(t, reply(200, `{}`))
	c := client(s)
	c.AsUser = agent
	s.Close()
	err := c.Leave(t.Context(), room)
	if err == nil {
		t.Fatal("no error from a closed server")
	}
	noURL(t, err, s.URL)
	var ue *url.Error
	if errors.As(err, &ue) {
		t.Fatalf("error still wraps a *url.Error: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = client(newServer(t, reply(200, `{}`))).Leave(ctx, room)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v, want context.Canceled", err)
	}
}

func TestBadJSON(t *testing.T) {
	s := newServer(t, reply(200, `{"room_id":`))
	_, err := client(s).Join(t.Context(), alias)
	if err == nil {
		t.Fatal("truncated JSON accepted")
	}
	noURL(t, err, s.URL)
}
