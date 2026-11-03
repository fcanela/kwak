//go:build integration && linux

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/registrar"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/test/integration/ittest"
)

// csAPIAddr is where the real kwak-registrar serves the listener API inside the runner.
const csAPIAddr = "127.0.0.1:8010"

// TestRegistrarContainer is the smoke test of the real kwak-registrar binary (plan P6, spec 10.4):
// serve -config with a deployment-shaped config, one pairing and one agent. ittest has no option
// for a second container, so the binary runs as a process in the runner, where tuwunel reaches
// its appservice port through the runner's alias.
func TestRegistrarContainer(t *testing.T) {
	kwakBin, regBin := "", ""
	if !ittest.Inner() {
		kwakBin = buildKwak(t)
		regBin = csBuild(t, "github.com/fcanela/kwak/cmd/kwak-registrar", "kwak-registrar")
	}
	env := ittest.Start(t, ittest.WithBinary("kwak", kwakBin), ittest.WithBinary("kwak-registrar", regBin))
	if env == nil {
		return
	}
	w := &World{Env: env, Ctx: t.Context(), Kwak: env.Binaries["kwak"], top: t}
	w.sink = &sink{w: t.Output()}
	t.Cleanup(w.sink.close)
	w.Bot = "@" + env.Registration.SenderLocalpart + ":" + Server
	w.Admin = &Human{Login: env.Admin, C: env.Client(env.Admin.AccessToken)}
	room, err := w.Admin.C.CreateRoom(w.Ctx, registrar.RoomBody(w.Bot))
	if err != nil {
		t.Fatalf("create the agents room: %v", err)
	}
	w.Room = room

	// The edge's /_kwak/v1 side goes to the real binary; /_matrix goes to tuwunel as before.
	w.Reg = &Registrar{w: w}
	var api http.Handler = httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: csAPIAddr})
	w.Reg.api.Store(&api)
	w.startEdge(t)

	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	cfg := map[string]any{
		"server_name": Server, "homeserver": ittest.HomeserverURL, "registration": ittest.RegistrationPath,
		"state": state, "appservice_addr": ittest.AppserviceAddr, "api_addr": csAPIAddr,
		"room": "#agents:" + Server, "admins": []string{env.Admin.UserID},
		"idle_after": "1h", "close_grace": "2s", "key_lifetime": "2160h", "key_warn_before": "336h",
		"pairing_ttl": "10m", "unconfirmed_ttl": "10m", "prune_closed_after": "2160h", "jobs_every": "1s",
	}
	cfgPath := filepath.Join(dir, "config.json")
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	reg := csServe(t, w, env.Binaries["kwak-registrar"], cfgPath)

	w.Admin.DM = csWaitDM(t, state, env.Admin.UserID)
	if _, err := w.Admin.C.Join(w.Ctx, w.Admin.DM); err != nil {
		t.Fatalf("admin joins the DM: %v", err)
	}
	o := w.newOwner(t, "alice", env.Alice)
	Send(t, w.Admin.C, w.Admin.DM, "owner add "+o.Human.Login.UserID)
	WaitNotice(t, w.Admin.C, w.Admin.DM, "Added "+o.Human.Login.UserID+" as owner alice.")
	o.Human.DM = csWaitDM(t, state, o.Human.Login.UserID)
	if _, err := o.Human.C.Join(w.Ctx, o.Human.DM); err != nil {
		t.Fatalf("alice joins the DM: %v", err)
	}
	WaitNotice(t, o.Human.C, o.Human.DM, "You are now a kwak owner as `alice`.")

	o.StartListener(t)
	p := o.Pair(t)
	if !strings.HasPrefix(p.Key, "kwak_ok_") || !strings.Contains(p.Out, "Paired as alice on "+o.Machine+" (pairing "+p.PairingID) {
		t.Fatalf("pairing through the real registrar: key %t, output:\n%s", strings.HasPrefix(p.Key, "kwak_ok_"), p.Out)
	}
	WaitNotice(t, o.Human.C, o.Human.DM, fmt.Sprintf("Paired machine '%s' (linux) as pairing %s, asked from ", o.Machine, p.PairingID))

	h := o.NewHarness(t, wire.HarnessClaude)
	h.Begin(t)
	uid := o.WaitAgent(t, h)
	if uid != AgentID("alice", wire.HarnessClaude, h.SessionID) {
		t.Fatalf("agent %s", uid)
	}
	w.WaitMember(t, uid, true)
	lp := strings.TrimPrefix(Short(uid), "@")
	if dn := w.DisplayName(t, uid); dn != lp {
		t.Fatalf("display name %q", dn)
	}
	sent := h.MustCall(t, wire.ToolSend, map[string]any{"text": "smoke: hello from the real registrar"})
	if !strings.HasPrefix(sent, "Sent as "+Short(uid)+" in thread ") {
		t.Fatalf("send: %q", sent)
	}

	reg.stop(t)
	fi, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("state.json mode %v, want 0600", fi.Mode().Perm())
	}
	var st registrar.State
	raw, _ := os.ReadFile(state)
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("parse state.json: %v", err)
	}
	pr := st.Pairings[p.PairingID]
	a := st.Agents[lp]
	if pr == nil || pr.Owner != "alice" || pr.Confirmed == nil || pr.Revoked != nil {
		t.Fatalf("pairing in state.json: %+v", pr)
	}
	if a == nil || a.Pairing != p.PairingID || a.Closed != nil || a.PendingSince != nil || st.RoomID != w.Room {
		t.Fatalf("agent in state.json: %+v, room %s", a, st.RoomID)
	}
}

// csBuild builds a static linux binary of pkg on the host.
func csBuild(t *testing.T, pkg, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	gobin := "go"
	if _, err := exec.LookPath(gobin); err != nil {
		gobin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	cmd := exec.Command(gobin, "build", "-o", out, pkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
	return out
}

type csProc struct {
	cmd    *exec.Cmd
	log    *lockedBuf
	exited chan struct{}
	code   int
}

// csServe runs kwak-registrar serve -config path and waits until both ports listen.
func csServe(t *testing.T, w *World, bin, path string) *csProc {
	t.Helper()
	p := &csProc{log: &lockedBuf{}, exited: make(chan struct{})}
	p.cmd = exec.Command(bin, "serve", "-config", path)
	p.cmd.Stdout = io.MultiWriter(w.sink, p.log)
	p.cmd.Stderr = io.MultiWriter(w.sink, p.log)
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start kwak-registrar: %v", err)
	}
	go func() {
		p.code = exitCode(p.cmd.Wait())
		close(p.exited)
	}()
	t.Cleanup(func() {
		p.cmd.Process.Signal(syscall.SIGKILL)
		<-p.exited
	})
	EventuallyMsg(t, "kwak-registrar started", func() (bool, string) {
		select {
		case <-p.exited:
			t.Fatalf("kwak-registrar exited %d:\n%s", p.code, p.log)
		default:
		}
		return strings.Contains(p.log.String(), "kwak-registrar started"), p.log.String()
	})
	return p
}

// stop sends SIGTERM and checks a clean exit, which flushes state.json (spec 3.2).
func (p *csProc) stop(t *testing.T) {
	t.Helper()
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(30 * time.Second):
		t.Fatal("kwak-registrar did not stop on SIGTERM")
	}
	if p.code != 0 || !strings.Contains(p.log.String(), "kwak-registrar stopped") {
		t.Fatalf("kwak-registrar exited %d:\n%s", p.code, p.log)
	}
}

// csWaitDM waits for the DM the real registrar recorded for human in its state.json.
func csWaitDM(t *testing.T, state, human string) string {
	t.Helper()
	var dm string
	EventuallyMsg(t, "the DM of "+human, func() (bool, string) {
		raw, err := os.ReadFile(state)
		if err != nil {
			return false, err.Error()
		}
		var st struct {
			DMs map[string]string `json:"dms"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return false, err.Error()
		}
		dm = st.DMs[human]
		return dm != "", fmt.Sprintf("dms %v", st.DMs)
	})
	return dm
}
