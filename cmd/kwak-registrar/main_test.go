package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/wire"
)

func testDeps(t *testing.T) deps {
	return deps{ctx: t.Context(), isTerminal: func(any) bool { return false }, http: http.DefaultClient, hsHTTP: http.DefaultClient}
}

func runCmd(t *testing.T, d deps, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb, d)
	return code, out.String(), errb.String()
}

func TestGenRegistrationFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kwak.yaml")
	code, out, errs := runCmd(t, testDeps(t), "", "gen-registration", "-url", "http://kwak-registrar:8009", "-server", "kwak.test", "-o", path)
	if code != 0 || out != "" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errs)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi.Mode(), err)
	}
	data, _ := os.ReadFile(path)
	r, err := wire.ParseRegistration(data)
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != "http://kwak-registrar:8009" || r.Namespaces.Users[0].Regex != `^@agent\..+:kwak\.test$` || len(r.ASToken) != 64 {
		t.Fatalf("registration %+v", r)
	}
	if strings.Contains(errs, r.ASToken) || strings.Contains(errs, r.HSToken) {
		t.Fatal("a token reached stderr")
	}
	if code, _, _ := runCmd(t, testDeps(t), "", "gen-registration", "-url", "http://x:8009", "-server", "kwak.test", "-o", path); code == 0 {
		t.Fatal("overwrote an existing file")
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, data) {
		t.Fatal("existing file changed")
	}
}

func TestGenRegistrationStdout(t *testing.T) {
	d := testDeps(t)
	code, out, _ := runCmd(t, d, "", "gen-registration", "-url", "http://kwak-registrar:8009", "-server", "kwak.test", "-o", "-")
	if code != 0 {
		t.Fatal(code)
	}
	if _, err := wire.ParseRegistration([]byte(out)); err != nil {
		t.Fatal(err)
	}
	d.isTerminal = func(any) bool { return true }
	code, out, errs := runCmd(t, d, "", "gen-registration", "-url", "http://kwak-registrar:8009", "-server", "kwak.test", "-o", "-")
	if code == 0 || out != "" || !strings.Contains(errs, "refusing to print the registration to a terminal") {
		t.Fatalf("terminal: %d %q %q", code, out, errs)
	}
}

func TestGenRegistrationYAML(t *testing.T) {
	code, out, _ := runCmd(t, testDeps(t), "", "gen-registration", "-url", "http://kwak-registrar:8009", "-server", "kwak.test", "-format", "yaml", "-o", "-")
	if code != 0 {
		t.Fatal(code)
	}
	for _, re := range []string{
		`(?m)^id: "kwak"$`, `(?m)^as_token: "[0-9a-f]{64}"$`, `(?m)^hs_token: "[0-9a-f]{64}"$`,
		`(?m)^sender_localpart: "registrar"$`, `(?m)^io\.element\.msc4190: false$`,
		`(?m)^    - exclusive: true\n      regex: "\^@agent\\\\\.\.\+:kwak\\\\\.test\$"$`,
	} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("yaml does not match %s:\n%s", re, out)
		}
	}
}

func TestGenRegistrationNeedsFlags(t *testing.T) {
	for _, args := range [][]string{
		{"gen-registration", "-server", "kwak.test", "-o", "-"},
		{"gen-registration", "-url", "http://x", "-o", "-"},
		{"gen-registration", "-url", "http://x", "-server", "kwak.test"},
		{"gen-registration", "-url", "http://x", "-server", "kwak.test", "-o", "-", "-format", "toml"},
	} {
		if code, _, _ := runCmd(t, testDeps(t), "", args...); code != 2 {
			t.Errorf("%v: code %d", args, code)
		}
	}
}

func TestCreateRoom(t *testing.T) {
	var mu sync.Mutex
	var auth, path string
	var body map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth, path = r.Header.Get("Authorization"), r.URL.RequestURI()
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"room_id":"!agents:kwak.test"}`))
	}))
	defer srv.Close()
	d := testDeps(t)
	d.http = srv.Client()
	code, out, errs := runCmd(t, d, "human-token\n", "create-room", "-homeserver", srv.URL, "-server", "kwak.test")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "Bearer human-token" || path != "/_matrix/client/v3/createRoom" {
		t.Fatalf("auth %q path %q", auth, path)
	}
	pl := body["power_level_content_override"].(map[string]any)
	if users := pl["users"].(map[string]any); users["@registrar:kwak.test"] != float64(50) || len(users) != 1 {
		t.Fatalf("users %v", users)
	}
	if body["preset"] != "public_chat" || body["visibility"] != "private" || body["room_alias_name"] != "agents" {
		t.Fatalf("body %v", body)
	}
	if !strings.Contains(out, "!agents:kwak.test") || strings.Contains(out+errs, "human-token") {
		t.Fatalf("out %q", out)
	}
}

func TestCreateRoomErrorHoldsNoURL(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	d := testDeps(t)
	d.http = srv.Client()
	base := srv.URL
	srv.Close()
	code, _, errs := runCmd(t, d, "human-token\n", "create-room", "-homeserver", base, "-server", "kwak.test")
	if code != 1 || !strings.Contains(errs, "create the room") {
		t.Fatalf("%d %q", code, errs)
	}
	if strings.Contains(errs, "/_matrix") || strings.Contains(errs, "https://") {
		t.Fatalf("stderr holds the URL: %q", errs)
	}
}

func TestCreateRoomRefusals(t *testing.T) {
	if code, _, _ := runCmd(t, testDeps(t), "tok", "create-room", "-homeserver", "http://kwak.test"); code != 2 {
		t.Error("accepted http://")
	}
	if code, _, _ := runCmd(t, testDeps(t), "", "create-room", "-homeserver", "https://kwak.test"); code != 2 {
		t.Error("accepted an empty token")
	}
	if code, _, _ := runCmd(t, testDeps(t), "a b", "create-room", "-homeserver", "https://kwak.test"); code != 2 {
		t.Error("accepted two words")
	}
}

// fakeTuwunel answers every call with a room id and records whether X-Forwarded-For was sent.
func fakeTuwunel(t *testing.T) (*httptest.Server, func() (int, int)) {
	var mu sync.Mutex
	var n, noXFF int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		if r.Header.Get("X-Forwarded-For") != "127.0.0.1" {
			noXFF++
		}
		mu.Unlock()
		w.Write([]byte(`{"room_id":"!agents:kwak.test"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() (int, int) { mu.Lock(); defer mu.Unlock(); return n, noXFF }
}

func writeServeConfig(t *testing.T, hs string) (string, string) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "kwak.yaml")
	if code, _, _ := runCmd(t, testDeps(t), "", "gen-registration", "-url", "http://x:8009", "-server", "kwak.test", "-o", reg); code != 0 {
		t.Fatal("gen-registration")
	}
	cfg := map[string]any{
		"server_name": "kwak.test", "homeserver": hs, "registration": reg,
		"state": filepath.Join(dir, "state.json"), "appservice_addr": "127.0.0.1:0", "api_addr": "127.0.0.1:0",
		"room": "#agents:kwak.test", "admins": []string{"@admin:kwak.test"}, "jobs_every": "1s", "idle_after": "3s",
	}
	b, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, b, 0o600)
	return path, filepath.Join(dir, "state.json")
}

func TestServe(t *testing.T) {
	hs, counts := fakeTuwunel(t)
	cfgPath, statePath := writeServeConfig(t, hs.URL)
	ctx, cancel := context.WithCancel(t.Context())
	d := testDeps(t)
	d.ctx = ctx
	addrs := make(chan [2]net.Addr, 1)
	d.ready = func(api, as net.Addr) { addrs <- [2]net.Addr{api, as} }
	done := make(chan int)
	var errb bytes.Buffer
	go func() { done <- run([]string{"serve", "-config", cfgPath}, nil, io.Discard, &errb, d) }()
	var a [2]net.Addr
	select {
	case a = <-addrs:
	case code := <-done:
		t.Fatalf("serve exited %d: %s", code, errb.String())
	}
	get := func(method, url, auth string) int {
		req, _ := http.NewRequest(method, url, strings.NewReader("{}"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get("GET", "http://"+a[0].String()+"/_kwak/v1/me", ""); c != 401 {
		t.Errorf("me without a key: %d", c)
	}
	if c := get("POST", "http://"+a[1].String()+"/_matrix/app/v1/ping", ""); c != 403 {
		t.Errorf("ping without hs_token: %d", c)
	}
	if c := get("GET", "http://"+a[0].String()+"/_matrix/app/v1/ping", ""); c != 404 {
		t.Errorf("appservice route on the API port: %d", c)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(statePath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the registrar never stored the room")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("serve exited %d: %s", code, errb.String())
	}
	if n, noXFF := counts(); n == 0 || noXFF != 0 {
		t.Fatalf("%d requests to tuwunel, %d without X-Forwarded-For", n, noXFF)
	}
}

func TestServeRefusesACorruptState(t *testing.T) {
	hs, _ := fakeTuwunel(t)
	cfgPath, statePath := writeServeConfig(t, hs.URL)
	os.WriteFile(statePath, []byte("{"), 0o600)
	code, _, errs := runCmd(t, testDeps(t), "", "serve", "-config", cfgPath)
	if code != 1 || !strings.Contains(errs, "never resets its state") {
		t.Fatalf("%d %q", code, errs)
	}
	if b, _ := os.ReadFile(statePath); string(b) != "{" {
		t.Fatal("state file changed")
	}
}
