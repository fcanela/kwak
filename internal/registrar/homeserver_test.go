package registrar

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type seenReq struct {
	method, path, rawQuery, xff, auth string
	body                              map[string]any
}

// recordingHS answers every client-server call with a plausible body and records the request.
func recordingHS(t *testing.T) (*httptest.Server, func() []seenReq) {
	var mu sync.Mutex
	var seen []seenReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		mu.Lock()
		seen = append(seen, seenReq{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("X-Forwarded-For"), r.Header.Get("Authorization"), body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/joined_members"):
			w.Write([]byte(`{"joined":{"@a:kwak.test":{},"@registrar:kwak.test":{}}}`))
		case strings.Contains(r.URL.Path, "/state/m.room.encryption"):
			w.WriteHeader(404)
			w.Write([]byte(`{"errcode":"M_NOT_FOUND"}`))
		case strings.Contains(r.URL.Path, "/event/"):
			w.Write([]byte(`{"event_id":"$e","sender":"@a:kwak.test","type":"m.room.message","content":{}}`))
		default:
			w.Write([]byte(`{"room_id":"!r:kwak.test","user_id":"@x:kwak.test","access_token":"t","device_id":"KWAK","event_id":"$e"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenReq(nil), seen...)
	}
}

func TestHomeserverAdapterSendsXForwardedForOnEveryRequest(t *testing.T) {
	srv, seen := recordingHS(t)
	hs := NewHomeserver(srv.URL, "as-secret", srv.Client())
	ctx := context.Background()
	agent := "@agent.o.0123456789abcdef:kwak.test"
	calls := []func() error{
		func() error { _, err := hs.Register(ctx, "agent.o.0123456789abcdef", "KWAK"); return err },
		func() error { _, err := hs.Login(ctx, agent, "KWAK"); return err },
		func() error { return hs.LogoutDevice(ctx, agent, "KWAK-3f9a1c2e") },
		func() error { return hs.LogoutAll(ctx, agent) },
		func() error { return hs.JoinAs(ctx, agent, "!r:kwak.test") },
		func() error { return hs.JoinAs(ctx, "", "!r:kwak.test") },
		func() error { return hs.LeaveAs(ctx, agent, "!r:kwak.test") },
		func() error { return hs.SetDisplayName(ctx, agent, "agent.o.0123456789abcdef") },
		func() error { return hs.SetAvatarURL(ctx, agent, "") },
		func() error { _, err := hs.UserExists(ctx, "@a:kwak.test"); return err },
		func() error { _, err := hs.CreateDM(ctx, "@a:kwak.test"); return err },
		func() error { return hs.Invite(ctx, "!dm:kwak.test", "@a:kwak.test") },
		func() error { _, _, err := hs.RoomInfo(ctx, "!dm:kwak.test"); return err },
		func() error { _, err := hs.FetchEvent(ctx, "!dm:kwak.test", "$e"); return err },
		func() error { return hs.Notice(ctx, "!dm:kwak.test", "txn1", "hello") },
		func() error { _, err := hs.ResolveAlias(ctx, "#agents:kwak.test"); return err },
	}
	for i, c := range calls {
		if err := c(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	reqs := seen()
	if len(reqs) < len(calls) {
		t.Fatalf("%d requests for %d calls", len(reqs), len(calls))
	}
	for _, r := range reqs {
		if r.xff != ForwardedFor {
			t.Errorf("%s %s: X-Forwarded-For %q", r.method, r.path, r.xff)
		}
		if r.auth != "Bearer as-secret" {
			t.Errorf("%s %s: Authorization %q", r.method, r.path, r.auth)
		}
		if strings.Contains(r.rawQuery, "access_token") || strings.Contains(r.rawQuery, "as-secret") {
			t.Errorf("%s %s: token in the URL", r.method, r.path)
		}
	}
	find := func(sub string) seenReq {
		for _, r := range reqs {
			if strings.Contains(r.path, sub) {
				return r
			}
		}
		t.Fatalf("no request to %s", sub)
		return seenReq{}
	}
	if q := find("/logout").rawQuery; !strings.Contains(q, "device_id=KWAK-3f9a1c2e") || !strings.Contains(q, "user_id=%40agent.o") {
		t.Errorf("device logout query %q", q)
	}
	if q := find("/logout/all").rawQuery; strings.Contains(q, "device_id") || !strings.Contains(q, "user_id=") {
		t.Errorf("logout/all query %q", q)
	}
	if r := find("/createRoom"); r.body["is_direct"] != true {
		t.Errorf("DM body %v", r.body)
	} else if pl, _ := r.body["power_level_content_override"].(map[string]any); pl == nil || pl["users"] != nil {
		t.Errorf("DM power levels must have no users key: %v", r.body)
	}
	if r := find("/send/m.room.message/txn1"); r.body["msgtype"] != "m.notice" || r.body["body"] != "hello" {
		t.Errorf("notice body %v", r.body)
	}
}

func TestHomeserverAdapterAllowsHTTPOnlyForItsOwnBase(t *testing.T) {
	srv, _ := recordingHS(t)
	if !strings.HasPrefix(srv.URL, "http://") {
		t.Fatal("test server is not http")
	}
	hs := NewHomeserver(srv.URL, "as", srv.Client()).(*asClient)
	if !hs.c.AllowHTTP {
		t.Fatal("registrar client refuses its http:// homeserver")
	}
	hs2 := NewHomeserver("https://kwak.test", "as", srv.Client()).(*asClient)
	if hs2.c.AllowHTTP {
		t.Fatal("AllowHTTP set for an https base")
	}
}
