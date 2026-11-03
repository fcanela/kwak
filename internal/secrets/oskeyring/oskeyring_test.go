package oskeyring

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/secrets"
	"github.com/zalando/go-keyring"
)

// fakeKeyring is a map whose calls block while gate is closed.
type fakeKeyring struct {
	mu    sync.Mutex
	m     map[string]string
	gate  chan struct{}
	calls atomic.Int32
}

func newFake() *fakeKeyring {
	g := make(chan struct{})
	close(g)
	return &fakeKeyring{m: map[string]string{}, gate: g}
}

func (f *fakeKeyring) wait() {
	f.calls.Add(1)
	f.mu.Lock()
	g := f.gate
	f.mu.Unlock()
	<-g
}

func (f *fakeKeyring) block() (release func()) {
	g := make(chan struct{})
	f.mu.Lock()
	f.gate = g
	f.mu.Unlock()
	return func() { close(g) }
}

func (f *fakeKeyring) Get(service, user string) (string, error) {
	f.wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[service+"\x00"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (f *fakeKeyring) Set(service, user, password string) error {
	f.wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[service+"\x00"+user] = password
	return nil
}

func (f *fakeKeyring) Delete(service, user string) error {
	f.wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	k := service + "\x00" + user
	if _, ok := f.m[k]; !ok {
		return keyring.ErrNotFound
	}
	delete(f.m, k)
	return nil
}

func TestRoundTrip(t *testing.T) {
	f := newFake()
	s := newStore(f, secrets.Service, time.Second)
	key := secrets.OwnerKeyName("kwak.test")
	if _, err := s.Get(key); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Get missing: %v, want ErrNotFound", err)
	}
	if err := s.Delete(key); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Delete missing: %v, want ErrNotFound", err)
	}
	if err := s.Set(key, "k1"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(key); err != nil || v != "k1" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if _, ok := f.m["kwak\x00kwak.test/owner-key"]; !ok {
		t.Fatalf("stored under the wrong service or user: %v", f.m)
	}
	if err := s.Delete(key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(key); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
}

func TestDeadline(t *testing.T) {
	f := newFake()
	s := newStore(f, secrets.Service, 50*time.Millisecond)
	if err := s.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	release := f.block()

	start := time.Now()
	if _, err := s.Get("k"); !errors.Is(err, secrets.ErrTimeout) {
		t.Fatalf("blocked Get: %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("blocked Get took %v", d)
	}
	if err := s.Set("k", "w"); !errors.Is(err, secrets.ErrTimeout) {
		t.Fatalf("Set while stuck: %v, want ErrTimeout", err)
	}
	if err := s.Delete("k"); !errors.Is(err, secrets.ErrTimeout) {
		t.Fatalf("Delete while stuck: %v, want ErrTimeout", err)
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("keyring calls = %d, want 2 (one Set, one stuck Get)", n)
	}

	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, err := s.Get("k")
		if err == nil {
			if v != "v" {
				t.Fatalf("Get after unlock = %q", v)
			}
			break
		}
		if !errors.Is(err, secrets.ErrTimeout) || time.Now().After(deadline) {
			t.Fatalf("Get after unlock: %v", err)
		}
	}
}

func TestDeadlineConcurrent(t *testing.T) {
	f := newFake()
	s := newStore(f, secrets.Service, 30*time.Millisecond)
	release := f.block()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Get("k"); !errors.Is(err, secrets.ErrTimeout) {
				t.Errorf("Get: %v, want ErrTimeout", err)
			}
		}()
	}
	wg.Wait()
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("keyring calls = %d, want 1", n)
	}
	release()
}

func TestPreflight(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	none := func(string) bool { return false }
	bus := func(p string) bool { return p == "/run/user/1000/bus" }
	cases := []struct {
		name   string
		env    map[string]string
		exists func(string) bool
		ok     bool
	}{
		{"address", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/x/bus"}, none, true},
		{"run user bus", nil, bus, true},
		{"nothing", nil, none, false},
		{"autolaunch", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "autolaunch:"}, none, false},
		{"autolaunch with bus", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "autolaunch:"}, bus, true},
		{"xdg runtime dir is not read", map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}, none, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := preflight(env(c.env), c.exists, 1000)
			if c.ok && err != nil {
				t.Fatalf("err = %v", err)
			}
			if !c.ok && !errors.Is(err, ErrNoSessionBus) {
				t.Fatalf("err = %v, want ErrNoSessionBus", err)
			}
		})
	}
	if ErrNoSessionBus.Error() != "no session bus; run under your desktop session or systemd --user" {
		t.Fatalf("message = %q", ErrNoSessionBus)
	}
}
