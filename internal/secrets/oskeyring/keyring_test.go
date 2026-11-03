//go:build keyring

package oskeyring

import (
	"errors"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/secrets"
)

// TestRealKeyring writes, reads and deletes one item in the real OS keyring. Run by hand only:
// go test -tags keyring ./internal/secrets/oskeyring/
func TestRealKeyring(t *testing.T) {
	if err := Preflight(); err != nil {
		t.Skip(err)
	}
	s := New("kwak-test", 10*time.Second)
	key := secrets.OwnerKeyName("kwak.test")
	t.Cleanup(func() { _ = s.Delete(key) })
	if err := s.Set(key, "value"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(key); err != nil || v != "value" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if err := s.Delete(key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(key); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
}
