package secretstest

import (
	"errors"
	"sync"
	"testing"

	"github.com/fcanela/kwak/internal/secrets"
)

func TestMem(t *testing.T) {
	s := Mem()
	key := secrets.OwnerKeyName("kwak.test")
	if key != "kwak.test/owner-key" {
		t.Fatalf("OwnerKeyName = %q", key)
	}
	if _, err := s.Get(key); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Get empty: %v", err)
	}
	if err := s.Delete(key); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Delete empty: %v", err)
	}
	if err := s.Set(key, "k1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(key, "k2"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(key); err != nil || v != "k2" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if s.Sets() != 2 {
		t.Fatalf("Sets = %d", s.Sets())
	}

	s.Fail(secrets.ErrTimeout)
	if _, err := s.Get(key); !errors.Is(err, secrets.ErrTimeout) {
		t.Fatalf("Get while failing: %v", err)
	}
	if err := s.Set(key, "k3"); !errors.Is(err, secrets.ErrTimeout) {
		t.Fatalf("Set while failing: %v", err)
	}
	if err := s.Delete(key); !errors.Is(err, secrets.ErrTimeout) {
		t.Fatalf("Delete while failing: %v", err)
	}
	s.Fail(nil)
	if v, _ := s.Get(key); v != "k2" {
		t.Fatalf("value changed while failing: %q", v)
	}
	if err := s.Delete(key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(key); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
}

func TestMemConcurrent(t *testing.T) {
	s := Mem()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := string(rune('a' + i))
			s.Set(k, k)
			s.Get(k)
			s.Delete(k)
		}()
	}
	wg.Wait()
}
