package regclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

type seen struct {
	Method, Path, Auth, Type string
	Body                     []byte
}

type recorder struct {
	mu   sync.Mutex
	reqs []seen
}

func (r *recorder) add(req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, seen{req.Method, req.URL.RequestURI(), req.Header.Get("Authorization"), req.Header.Get("Content-Type"), b})
}

func (r *recorder) all() []seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seen(nil), r.reqs...)
}

func tlsClient(srv *httptest.Server) *http.Client {
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// replayServer answers every request with row's status, Retry-After and body.
func replayServer(t *testing.T, row wiretest.Exchange) (*httptest.Server, *recorder) {
	rec := &recorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.add(req)
		if row.RetryAfter != "" {
			w.Header().Set("Retry-After", row.RetryAfter)
		}
		if row.Response != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(row.Status)
		io.WriteString(w, row.Response)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// expectedBody is what this client sends for a row: the row's body re-encoded through its typed
// request, so a BadBody row's unknown field is dropped, and a list over wire.MaxIDs is cut to its
// first chunk.
func expectedBody(t *testing.T, row wiretest.Exchange) []byte {
	if row.Request == "" {
		return nil
	}
	v := row.NewRequest()
	if err := json.Unmarshal([]byte(row.Request), v); err != nil {
		t.Fatalf("row request: %v", err)
	}
	switch r := v.(type) {
	case *wire.CloseRequest:
		r.UserIDs = r.UserIDs[:min(len(r.UserIDs), wire.MaxIDs)]
	case *wire.AliveRequest:
		r.UserIDs = r.UserIDs[:min(len(r.UserIDs), wire.MaxIDs)]
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// run calls the client method for row's route with row's request and returns the answer as the
// JSON the registrar would have sent (nil for routes whose method drops the body).
func run(t *testing.T, c *Client, row wiretest.Exchange) ([]byte, error) {
	ctx := context.Background()
	auth := strings.TrimPrefix(row.Auth, "Bearer ")
	var req any
	if row.Request != "" {
		req = row.NewRequest()
		if err := json.Unmarshal([]byte(row.Request), req); err != nil {
			t.Fatal(err)
		}
	}
	var out any
	var err error
	switch strings.TrimPrefix(row.Path, wire.APIPrefix) {
	case wire.PathPair:
		out, err = c.Pair(ctx, *req.(*wire.PairRequest), auth)
	case wire.PathPairClaim:
		c.Key = nil
		out, err = c.Claim(ctx, auth)
	case wire.PathStanding:
		out, err = c.Standing(ctx)
	case wire.PathAgents:
		out, err = c.Agent(ctx, *req.(*wire.AgentRequest))
	case wire.PathAgentsClose:
		r := req.(*wire.CloseRequest)
		err = c.Close(ctx, r.UserIDs, r.Reason)
	case wire.PathAgentsAlive:
		var a wire.AliveResponse
		a.Unknown, a.Closed, err = c.Alive(ctx, req.(*wire.AliveRequest).UserIDs, req.(*wire.AliveRequest).Touch)
		out = a
	case wire.PathUnpair:
		err = c.Unpair(ctx)
		out = wire.Empty{}
	case wire.PathMe:
		out, err = c.Me(ctx)
	default:
		t.Fatalf("no client method for %s", row.Path)
	}
	if err != nil || out == nil {
		return nil, err
	}
	b, merr := json.Marshal(out)
	if merr != nil {
		t.Fatal(merr)
	}
	return b, nil
}

// TestTranscript replays every row of the HTTP transcript table. A few rows hold a request the
// typed methods cannot produce (an unknown field, over wire.MaxIDs ids). For those the typed
// method must send a valid body instead, and the row's raw body is replayed through the
// transport, so the row's request and answer are both exercised.
func TestTranscript(t *testing.T) {
	typed, raw := 0, 0
	for _, row := range wiretest.HTTP {
		t.Run(row.Name, func(t *testing.T) {
			srv, rec := replayServer(t, row)
			key := strings.TrimPrefix(row.Auth, "Bearer ")
			c := &Client{Base: srv.URL, HTTP: tlsClient(srv), Key: func() (string, error) { return key, nil }}
			got, err := run(t, c, row)

			reqs := rec.all()
			if len(reqs) != 1 {
				t.Fatalf("%d requests, want 1", len(reqs))
			}
			r := reqs[0]
			checkHead(t, row, r)
			want := expectedBody(t, row)
			switch {
			case want == nil:
				if len(r.Body) != 0 {
					t.Errorf("sent body %s, want none", r.Body)
				}
			case !jsonEqual(r.Body, want):
				t.Errorf("sent body %s, want %s", r.Body, want)
			}
			if row.Request != "" && !jsonEqual([]byte(row.Request), want) {
				raw++
				checkValid(t, row, r.Body)
				replayRaw(t, row)
			} else {
				typed++
			}

			if row.Status < 300 {
				if err != nil {
					t.Fatalf("error %v", err)
				}
				if got != nil {
					if err := row.Check(row.Status, got); err != nil {
						t.Error(err)
					}
				}
				return
			}
			checkErr(t, row, err)
		})
	}
	t.Logf("%d rows replayed through the typed methods, %d raw rows through the transport", typed, raw)
}

func checkHead(t *testing.T, row wiretest.Exchange, r seen) {
	t.Helper()
	if r.Method != row.Method || r.Path != row.Path || r.Auth != row.Auth {
		t.Errorf("sent %s %s auth %q, want %s %s auth %q", r.Method, r.Path, r.Auth, row.Method, row.Path, row.Auth)
	}
	if row.Request != "" && r.Type != "application/json" {
		t.Errorf("Content-Type %q", r.Type)
	}
}

// checkValid checks that a body the typed method sent for a refused row would be accepted.
func checkValid(t *testing.T, row wiretest.Exchange, body []byte) {
	t.Helper()
	v := row.NewRequest()
	if err := wire.DecodeAPI(body, v); err != nil {
		t.Errorf("typed method sent a body the registrar refuses: %v", err)
	}
	var ids []string
	switch r := v.(type) {
	case *wire.CloseRequest:
		ids = r.UserIDs
	case *wire.AliveRequest:
		ids = r.UserIDs
	}
	if len(ids) > wire.MaxIDs {
		t.Errorf("typed method sent %d ids", len(ids))
	}
}

// replayRaw sends the row's exact body through the client's transport and checks the answer.
func replayRaw(t *testing.T, row wiretest.Exchange) {
	t.Helper()
	srv, rec := replayServer(t, row)
	c := &Client{Base: srv.URL, HTTP: tlsClient(srv)}
	path := strings.TrimPrefix(row.Path, wire.APIPrefix)
	_, err := c.call(context.Background(), row.Method, path, strings.TrimPrefix(row.Auth, "Bearer "),
		json.RawMessage(row.Request), nil, DefaultTimeout)
	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("raw: %d requests, want 1", len(reqs))
	}
	checkHead(t, row, reqs[0])
	if !jsonEqual(reqs[0].Body, []byte(row.Request)) {
		t.Errorf("raw: sent body %s, want the row's", reqs[0].Body)
	}
	if row.Status < 300 {
		t.Fatalf("raw row %q answers %d", row.Name, row.Status)
	}
	checkErr(t, row, err)
}

func checkErr(t *testing.T, row wiretest.Exchange, err error) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v, want *Error", err)
	}
	if e.Status != row.Status || e.Errcode != row.Errcode {
		t.Errorf("got %d %q, want %d %q", e.Status, e.Errcode, row.Status, row.Errcode)
	}
	if row.Response != "" {
		var a wire.APIError
		json.Unmarshal([]byte(row.Response), &a)
		if e.Message != a.Message {
			t.Errorf("message %q, want %q", e.Message, a.Message)
		}
	}
	if row.RetryAfter != "" && e.RetryAfter.String() != row.RetryAfter+"s" {
		t.Errorf("RetryAfter %v, want %ss", e.RetryAfter, row.RetryAfter)
	}
	var api *wire.APIError
	if !errors.As(err, &api) || api.Errcode != row.Errcode || api.Status != row.Status {
		t.Errorf("does not unwrap to the wire.APIError: %v", api)
	}
	if Code(err) != row.Errcode || Status(err) != row.Status {
		t.Errorf("Code %q Status %d", Code(err), Status(err))
	}
}

func TestHTTPSOnly(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		io.WriteString(w, `{"user_id":"@a:kwak.test","access_token":"t","device_id":"d"}`)
	}))
	defer srv.Close()
	key := func() (string, error) { return wiretest.OwnerKey, nil }
	for _, base := range []string{srv.URL, "ftp://kwak.test", "kwak.test", "https://", "https://u:p@kwak.test"} {
		c := &Client{Base: base, Key: key, HTTP: srv.Client()}
		if _, err := c.Standing(context.Background()); err == nil {
			t.Errorf("%s accepted", base)
		}
	}
	if n := len(rec.all()); n != 0 {
		t.Fatalf("%d requests sent to refused bases", n)
	}
	c := &Client{Base: srv.URL, Key: key, InsecureHTTPForTests: true}
	if _, err := c.Standing(context.Background()); err != nil {
		t.Fatalf("test option: %v", err)
	}
}

func TestRedirectRefused(t *testing.T) {
	target := &recorder{}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { target.add(r) }))
	defer plain.Close()
	redirect := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}
	tls := httptest.NewTLSServer(http.HandlerFunc(redirect))
	defer tls.Close()
	insecure := httptest.NewServer(http.HandlerFunc(redirect))
	defer insecure.Close()

	key := func() (string, error) { return wiretest.OwnerKey, nil }
	clients := map[string]*Client{
		"tls":     {Base: tls.URL, Key: key, HTTP: tlsClient(tls)},
		"default": {Base: insecure.URL, Key: key, InsecureHTTPForTests: true},
	}
	for name, c := range clients {
		_, err := c.Standing(context.Background())
		if Status(err) != http.StatusTemporaryRedirect {
			t.Errorf("%s: error %v, want a 307 error", name, err)
		}
	}
	for _, r := range target.all() {
		t.Errorf("redirect followed, Authorization %q", r.Auth)
	}
}

func idList(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("@agent.fcanela.%016x:kwak.test", i)
	}
	return ids
}

func TestChunks(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		var in wire.AliveRequest
		json.Unmarshal(rec.all()[len(rec.all())-1].Body, &in)
		if r.URL.Path == wire.APIPrefix+wire.PathAgentsClose {
			json.NewEncoder(w).Encode(wire.CloseResponse{Closed: in.UserIDs})
			return
		}
		json.NewEncoder(w).Encode(wire.AliveResponse{Unknown: in.UserIDs[:1], Closed: []string{}})
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: tlsClient(srv), Key: func() (string, error) { return wiretest.OwnerKey, nil }}
	ids := idList(1001)

	unknown, closed, err := c.Alive(context.Background(), ids, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ids[0], ids[500], ids[1000]}
	if !reflect.DeepEqual(unknown, want) || closed == nil || len(closed) != 0 {
		t.Errorf("unknown %v closed %v", unknown, closed)
	}
	if err := c.Close(context.Background(), ids, wire.ReasonSessionEnded); err != nil {
		t.Fatal(err)
	}
	reqs := rec.all()
	if len(reqs) != 6 {
		t.Fatalf("%d requests, want 6", len(reqs))
	}
	var sent []string
	for i, r := range reqs {
		var in wire.CloseRequest
		if err := json.Unmarshal(r.Body, &in); err != nil {
			t.Fatal(err)
		}
		if n := len(in.UserIDs); n > wire.MaxIDs || n != []int{500, 500, 1}[i%3] {
			t.Errorf("request %d has %d ids", i, n)
		}
		if i >= 3 {
			if in.Reason != wire.ReasonSessionEnded {
				t.Errorf("reason %q", in.Reason)
			}
			sent = append(sent, in.UserIDs...)
		}
	}
	if !reflect.DeepEqual(sent, ids) {
		t.Error("close ids not sent in order")
	}

	unknown, closed, err = c.Alive(context.Background(), nil, true)
	if err != nil || unknown == nil || closed == nil || len(unknown)+len(closed) != 0 {
		t.Errorf("empty alive: %v %v %v", unknown, closed, err)
	}
	if err := c.Close(context.Background(), nil, wire.ReasonSessionEnded); err != nil {
		t.Error(err)
	}
	if n := len(rec.all()); n != 6 {
		t.Errorf("empty lists sent %d requests", n-6)
	}
}

func TestKey(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	key := wiretest.OldOwnerKey
	c := &Client{Base: srv.URL, HTTP: tlsClient(srv), Key: func() (string, error) { return key, nil }}
	c.Unpair(context.Background())
	key = wiretest.OwnerKey
	c.Unpair(context.Background())
	reqs := rec.all()
	if reqs[0].Auth != "Bearer "+wiretest.OldOwnerKey || reqs[1].Auth != "Bearer "+wiretest.OwnerKey {
		t.Errorf("auth %q then %q", reqs[0].Auth, reqs[1].Auth)
	}

	c.SetKey(wiretest.BadKey)
	c.Unpair(context.Background())
	if a := rec.all()[2].Auth; a != "Bearer "+wiretest.BadKey {
		t.Errorf("after SetKey auth %q", a)
	}
	c = &Client{Base: srv.URL, HTTP: tlsClient(srv)}
	locked := errors.New("keyring locked")
	c.Key = func() (string, error) { return "", locked }
	if err := c.Unpair(context.Background()); !errors.Is(err, locked) {
		t.Errorf("error %v", err)
	}
	if len(rec.all()) != 3 {
		t.Error("request sent without a key")
	}
}

func TestErrors(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case wire.APIPrefix + wire.PathMe:
			io.WriteString(w, `{"owner":"`+strings.Repeat("x", wire.MaxBody)+`"}`)
		case wire.APIPrefix + wire.PathStanding:
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `<html>Bad Gateway</html>`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"errcode":"M_UNKNOWN","error":"not ours"}`)
		}
	}))
	key := func() (string, error) { return wiretest.OwnerKey, nil }
	c := &Client{Base: srv.URL, HTTP: tlsClient(srv), Key: key}
	if _, err := c.Me(context.Background()); err == nil {
		t.Error("oversize answer accepted")
	}
	_, err := c.Standing(context.Background())
	if Status(err) != 502 || Code(err) != "" {
		t.Errorf("proxy 502: %v", err)
	}
	_, err = c.Agent(context.Background(), wire.AgentRequest{})
	if Status(err) != 500 || Code(err) != "" {
		t.Errorf("non-kwak errcode: %v", err)
	}
	base := srv.URL
	srv.Close()
	_, err = c.Standing(context.Background())
	if err == nil || Status(err) != 0 || strings.Contains(err.Error(), base) {
		t.Errorf("network error %v", err)
	}
}

func TestDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c := &Client{Base: srv.URL, HTTP: tlsClient(srv)}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Claim(ctx, wiretest.Claim); err == nil {
		t.Fatal("no error")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("context deadline ignored")
	}
}

func TestBaseFunc(t *testing.T) {
	a, b := &recorder{}, &recorder{}
	answer := func(rec *recorder) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rec.add(r)
			io.WriteString(w, `{}`)
		}
	}
	srvA := httptest.NewTLSServer(answer(a))
	defer srvA.Close()
	srvB := httptest.NewTLSServer(answer(b))
	defer srvB.Close()
	base, berr := "", errors.New("this machine is not paired")
	c := &Client{BaseFunc: func() (string, error) { return base, berr }, HTTP: tlsClient(srvA)}
	if err := c.Unpair(context.Background()); !errors.Is(err, berr) {
		t.Errorf("error %v", err)
	}
	base, berr = srvA.URL, nil
	if err := c.Unpair(context.Background()); err != nil {
		t.Fatal(err)
	}
	base = srvB.URL
	c.HTTP = tlsClient(srvB)
	if err := c.Unpair(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.all()) != 1 || len(b.all()) != 1 {
		t.Errorf("requests: first server %d, second %d", len(a.all()), len(b.all()))
	}
	c.Base = "http://kwak.test"
	if err := c.Unpair(context.Background()); err == nil {
		t.Error("Base did not win over BaseFunc")
	}
	c = &Client{BaseFunc: func() (string, error) { return "http://kwak.test", nil }, HTTP: tlsClient(srvA)}
	if err := c.Unpair(context.Background()); err == nil || len(a.all()) != 1 {
		t.Error("http:// from BaseFunc accepted")
	}
}
