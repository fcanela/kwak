// Package oskeyring is the OS keyring behind secrets.Secrets: go-keyring, with every call under a
// deadline, because a locked keyring can block forever (spec 5.1).
package oskeyring

import (
	"errors"
	"time"

	"github.com/fcanela/kwak/internal/secrets"
	"github.com/zalando/go-keyring"
)

// backend is go-keyring's call shape; osKeyring forwards to its package functions.
type backend interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (osKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (osKeyring) Delete(service, user string) error { return keyring.Delete(service, user) }

// New returns the OS keyring for service, each call answering within deadline or failing with
// secrets.ErrTimeout. On Linux call Preflight first.
func New(service string, deadline time.Duration) secrets.Secrets {
	return newStore(osKeyring{}, service, deadline)
}

// store runs one keyring call at a time. A call stuck past its deadline keeps the slot, so later
// calls time out without starting another call that would block (and maybe prompt) as well.
type store struct {
	kr       backend
	service  string
	deadline time.Duration
	slot     chan struct{}
}

func newStore(kr backend, service string, deadline time.Duration) *store {
	return &store{kr: kr, service: service, deadline: deadline, slot: make(chan struct{}, 1)}
}

func (s *store) Get(key string) (string, error) {
	return s.call(func() (string, error) { return s.kr.Get(s.service, key) })
}

func (s *store) Set(key, value string) error {
	_, err := s.call(func() (string, error) { return "", s.kr.Set(s.service, key, value) })
	return err
}

func (s *store) Delete(key string) error {
	_, err := s.call(func() (string, error) { return "", s.kr.Delete(s.service, key) })
	return err
}

func (s *store) call(f func() (string, error)) (string, error) {
	t := time.NewTimer(s.deadline)
	defer t.Stop()
	select {
	case s.slot <- struct{}{}:
	case <-t.C:
		return "", secrets.ErrTimeout
	}
	type result struct {
		v   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() { <-s.slot }()
		v, err := f()
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		if errors.Is(r.err, keyring.ErrNotFound) {
			return "", secrets.ErrNotFound
		}
		return r.v, r.err
	case <-t.C:
		return "", secrets.ErrTimeout
	}
}

var _ secrets.Secrets = (*store)(nil)
