package matrixtest_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/matrix/matrixtest"
)

type step struct {
	status int
	body   string
}

type script struct {
	mu    sync.Mutex
	steps []step
	got   []map[string]any
	xff   []string
	paths []string
}

func serve(t *testing.T, steps ...step) (*script, *matrix.Client) {
	s := &script{steps: steps}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.got = append(s.got, m)
		s.xff = append(s.xff, r.Header.Get("X-Forwarded-For"))
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		if len(s.steps) == 0 {
			w.WriteHeader(500)
			return
		}
		st := s.steps[0]
		s.steps = s.steps[1:]
		w.WriteHeader(st.status)
		io.WriteString(w, st.body)
	}))
	t.Cleanup(srv.Close)
	return s, &matrix.Client{Base: srv.URL, AllowHTTP: true, HTTP: matrix.NewHTTPClient()}
}

const ok = `{"user_id":"@ann:kwak.test","access_token":"at","device_id":"D1"}`

func TestRegisterHumanToken(t *testing.T) {
	s, c := serve(t,
		step{401, `{"session":"S1","flows":[{"stages":["m.login.registration_token"]}],"params":{}}`},
		step{200, ok},
	)
	l, err := matrixtest.RegisterHuman(t.Context(), c, "ann", "pw", "RT")
	if err != nil {
		t.Fatal(err)
	}
	if l != (matrix.Login{UserID: "@ann:kwak.test", AccessToken: "at", DeviceID: "D1"}) {
		t.Fatalf("login = %+v", l)
	}
	if len(s.got) != 2 {
		t.Fatalf("%d requests", len(s.got))
	}
	if _, has := s.got[0]["auth"]; has || s.got[0]["username"] != "ann" || s.got[0]["password"] != "pw" {
		t.Fatalf("first body = %v", s.got[0])
	}
	want := map[string]any{"type": "m.login.registration_token", "token": "RT", "session": "S1"}
	if !reflect.DeepEqual(s.got[1]["auth"], want) || s.got[1]["username"] != "ann" {
		t.Fatalf("second body = %v", s.got[1])
	}
	for i, x := range s.xff {
		if x != matrixtest.DefaultForwardedFor {
			t.Fatalf("request %d X-Forwarded-For = %q", i, x)
		}
	}
	if c.ForwardedFor != "" {
		t.Fatal("caller's client was changed")
	}
	for _, p := range s.paths {
		if p != "POST /_matrix/client/v3/register" {
			t.Fatalf("path %s", p)
		}
	}
}

func TestRegisterHumanTokenThenDummy(t *testing.T) {
	flows := `"flows":[{"stages":["m.login.recaptcha"]},{"stages":["m.login.registration_token","m.login.dummy"]}]`
	s, c := serve(t,
		step{401, `{"session":"S1",` + flows + `}`},
		step{401, `{"session":"S1","completed":["m.login.registration_token"],` + flows + `}`},
		step{200, ok},
	)
	c.ForwardedFor = "10.9.9.9"
	if _, err := matrixtest.RegisterHuman(t.Context(), c, "ann", "pw", "RT"); err != nil {
		t.Fatal(err)
	}
	if got := s.got[2]["auth"]; !reflect.DeepEqual(got, map[string]any{"type": "m.login.dummy", "session": "S1"}) {
		t.Fatalf("third auth = %v", got)
	}
	for _, x := range s.xff {
		if x != "10.9.9.9" {
			t.Fatalf("X-Forwarded-For = %q", x)
		}
	}
}

func TestRegisterHumanFailures(t *testing.T) {
	cases := map[string][]step{
		"bad token": {
			{401, `{"session":"S1","flows":[{"stages":["m.login.registration_token"]}]}`},
			{401, `{"errcode":"M_FORBIDDEN","error":"bad token","session":"S1","flows":[{"stages":["m.login.registration_token"]}]}`},
		},
		"taken":           {{400, `{"errcode":"M_USER_IN_USE"}`}},
		"no usable flow":  {{401, `{"session":"S1","flows":[{"stages":["m.login.recaptcha"]}]}`}},
		"401 no session":  {{401, `{"flows":[]}`}},
		"server says 500": {{500, ``}},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			_, c := serve(t, steps...)
			if _, err := matrixtest.RegisterHuman(t.Context(), c, "ann", "pw", "RT"); err == nil {
				t.Fatal("no error")
			}
		})
	}
	_, c := serve(t, step{400, `{"errcode":"M_USER_IN_USE"}`})
	_, err := matrixtest.RegisterHuman(t.Context(), c, "ann", "pw", "RT")
	if !matrix.IsCode(err, matrix.ErrUserInUse) {
		t.Fatalf("err = %v, want M_USER_IN_USE kept", err)
	}
}

func TestLoginPassword(t *testing.T) {
	s, c := serve(t, step{200, ok})
	l, err := matrixtest.LoginPassword(t.Context(), c, "@ann:kwak.test", "pw")
	if err != nil || l.AccessToken != "at" {
		t.Fatalf("login = %+v, %v", l, err)
	}
	want := map[string]any{
		"type":       "m.login.password",
		"identifier": map[string]any{"type": "m.id.user", "user": "@ann:kwak.test"},
		"password":   "pw",
	}
	if !reflect.DeepEqual(s.got[0], want) || s.paths[0] != "POST /_matrix/client/v3/login" {
		t.Fatalf("got %s %v", s.paths[0], s.got[0])
	}
	if s.xff[0] != matrixtest.DefaultForwardedFor {
		t.Fatalf("X-Forwarded-For = %q", s.xff[0])
	}
}
