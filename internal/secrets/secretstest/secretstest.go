// Package secretstest is an in-memory secrets.Secrets for tests.
package secretstest

import (
	"sort"
	"sync"

	"github.com/fcanela/kwak/internal/secrets"
)

// Memory is a keyring in a map. It is safe for concurrent use.
type Memory struct {
	mu   sync.Mutex
	m    map[string]string
	err  error
	sets int
}

// Mem returns an empty keyring.
func Mem() *Memory { return &Memory{m: map[string]string{}} }

// Fail makes every call return err until Fail(nil), as a locked keyring would
// (secrets.ErrTimeout).
func (s *Memory) Fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Sets counts the successful Set calls.
func (s *Memory) Sets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sets
}

// Items lists the keys that hold a value, sorted.
func (s *Memory) Items() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Get implements secrets.Secrets.
func (s *Memory) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	v, ok := s.m[key]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}

// Set implements secrets.Secrets.
func (s *Memory) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.m[key] = value
	s.sets++
	return nil
}

// Delete implements secrets.Secrets.
func (s *Memory) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if _, ok := s.m[key]; !ok {
		return secrets.ErrNotFound
	}
	delete(s.m, key)
	return nil
}

var _ secrets.Secrets = (*Memory)(nil)
