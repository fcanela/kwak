package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/listener"
	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/proc/proctest"
	"github.com/fcanela/kwak/internal/regclient"
	"github.com/fcanela/kwak/internal/secrets/oskeyring"
	"github.com/fcanela/kwak/internal/secrets/secretstest"
	"github.com/fcanela/kwak/internal/service"
	"github.com/fcanela/kwak/internal/setup"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

var (
	_ listener.Registrar = (*regclient.Client)(nil)
	_ listener.KeySetter = (*regclient.Client)(nil)
	_ Service            = (*fakeService)(nil)

	// The production implementations P6 wires into Env, ListenConfig and kwak doctor.
	_ Service = service.Manager(nil)
	_         = Env{PrintHarness: setup.Print, Preflight: oskeyring.Preflight}
	_         = ListenConfig{Preflight: oskeyring.Preflight}
	_         = setup.DoctorDeps{Call: Caller(nil), Preflight: oskeyring.Preflight}
)

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

var errNoRegistrar = errors.New("no registrar in this test")

type noRegistrar struct{}

func (noRegistrar) Standing(context.Context) (wire.Standing, error) {
	return wire.Standing{}, errNoRegistrar
}
func (noRegistrar) Agent(context.Context, wire.AgentRequest) (wire.Agent, error) {
	return wire.Agent{}, errNoRegistrar
}
func (noRegistrar) Close(context.Context, []string, string) error { return errNoRegistrar }
func (noRegistrar) Alive(context.Context, []string, bool) ([]string, []string, error) {
	return nil, nil, errNoRegistrar
}
func (noRegistrar) Me(context.Context) (wire.Me, error) { return wire.Me{}, errNoRegistrar }
func (noRegistrar) Unpair(context.Context) error        { return errNoRegistrar }

func listenRig(t *testing.T) (ListenConfig, listener.Deps) {
	dir := shortDir(t)
	cfg := ListenConfig{
		Listener: listener.Config{ConfigPath: filepath.Join(dir, "config", ConfigFile)},
		StateDir: filepath.Join(dir, "state"),
		Socket:   filepath.Join(dir, "run", "kwak.sock"),
	}
	d := listener.Deps{
		Secrets:   secretstest.Mem(),
		Registrar: noRegistrar{},
		Matrix:    func(string) listener.Matrix { return nil },
		Procs:     proctest.New(),
		Clock:     wallClock{},
	}
	return cfg, d
}

func TestListenServesUnpaired(t *testing.T) {
	cfg, d := listenRig(t)
	done := make(chan int, 1)
	var errb bytes.Buffer
	go func() { done <- Listen(cfg, d)(nil, nil, nil, &errb) }()

	call := SocketCaller(cfg.Socket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var st wire.StatusResponse
	for {
		err := call(ctx, wire.Head{V: 1, Op: wire.OpStatus}, &st)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("listener never answered: %v (%s)", err, errb.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !st.OK || st.State != wire.StateUnpaired {
		t.Errorf("status %+v", st)
	}

	second := Listen(cfg, d)
	var out bytes.Buffer
	if code := second(nil, nil, nil, &out); code != 0 || !strings.Contains(out.String(), "already running") {
		t.Errorf("second instance: exit %d: %s", code, out.String())
	}

	var ack wire.Ack
	if err := call(ctx, wire.Head{V: 1, Op: wire.OpShutdown}, &ack); err != nil || !ack.OK {
		t.Fatalf("shutdown: %v %+v", err, ack)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d: %s", code, errb.String())
		}
	case <-ctx.Done():
		t.Fatal("listener did not stop after shutdown")
	}
	if _, err := os.Lstat(cfg.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Error("socket left behind")
	}
	release, err := ipc.Lock(filepath.Join(cfg.StateDir, "listen.lock"))
	if err != nil {
		t.Errorf("lock not released: %v", err)
	} else {
		release()
	}
}

var errNoMatrix = errors.New("no homeserver in this test")

type noMatrix struct{}

func (noMatrix) Sync(context.Context, string, string, time.Duration) (*matrix.SyncResponse, error) {
	return nil, errNoMatrix
}
func (noMatrix) Send(context.Context, string, string, any) (string, error) { return "", errNoMatrix }
func (noMatrix) Messages(context.Context, string, string, int, string) (*matrix.MessagesResponse, error) {
	return nil, errNoMatrix
}
func (noMatrix) Event(context.Context, string, string) (*matrix.Event, error) {
	return nil, errNoMatrix
}
func (noMatrix) ThreadReplies(context.Context, string, string, int) ([]matrix.Event, error) {
	return nil, errNoMatrix
}
func (noMatrix) JoinedMembers(context.Context, string) ([]string, error) { return nil, errNoMatrix }

// A listener started before the first pairing takes the server from config.json at reload.
func TestListenPairLater(t *testing.T) {
	cfg, d := listenRig(t)
	reg := &fakeRegistrar{}
	srv := httptest.NewTLSServer(http.HandlerFunc(reg.handler))
	defer srv.Close()
	cfg.HTTP = srv.Client()
	d.Registrar = nil
	d.Matrix = func(string) listener.Matrix { return noMatrix{} }
	keys := secretstest.Mem()
	d.Secrets = keys
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan int, 1)
	var errb bytes.Buffer
	go func() { done <- Listen(cfg, d)(nil, nil, nil, &errb) }()
	call := SocketCaller(cfg.Socket)
	var st wire.StatusResponse
	for call(ctx, wire.Head{V: 1, Op: wire.OpStatus}, &st) != nil {
		if ctx.Err() != nil {
			t.Fatalf("listener never answered (%s)", errb.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	conf := `{"version":1,"server":"` + srv.URL + `","server_name":"kwak.test","owner":"fcanela","machine":"laptop",` +
		`"pairing_id":"` + wiretest.PairingID + `","room_id":"` + wiretest.RoomID + `","standing_user":"` + wiretest.StandingUser + `"}`
	os.MkdirAll(filepath.Dir(cfg.Listener.ConfigPath), 0o700)
	if err := os.WriteFile(cfg.Listener.ConfigPath, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	keys.Set(keyName, wiretest.OwnerKey)
	var rr wire.ReloadResponse
	if err := call(ctx, wire.Head{V: 1, Op: wire.OpReload}, &rr); err != nil || !rr.OK {
		t.Errorf("reload: %v %+v", err, rr)
	}
	if calls := reg.log(); len(calls) == 0 || calls[0] != wire.PathStanding+" Bearer "+wiretest.OwnerKey {
		t.Errorf("registrar calls %v", calls)
	}

	var ack wire.Ack
	call(ctx, wire.Head{V: 1, Op: wire.OpShutdown}, &ack)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("listener did not stop")
	}
}

func TestListenPreflight(t *testing.T) {
	cfg, d := listenRig(t)
	cfg.Preflight = func() error { return errors.New("no session bus; run under your desktop session or systemd --user") }
	var errb bytes.Buffer
	if code := Listen(cfg, d)(nil, nil, nil, &errb); code != 1 || !strings.Contains(errb.String(), "no session bus") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if _, err := os.Lstat(cfg.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Error("socket created before the preflight")
	}
}

func TestListenArgs(t *testing.T) {
	cfg, d := listenRig(t)
	var errb bytes.Buffer
	if code := Listen(cfg, d)([]string{"--detach"}, nil, nil, &errb); code != 2 {
		t.Errorf("exit %d", code)
	}
}
