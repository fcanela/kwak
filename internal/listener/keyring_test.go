package listener

import (
	"context"
	"errors"
	"testing"

	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

// A machine paired by an older kwak has no keyring_user in config.json and its key under the old
// shared name: it keeps working with no action (decision 40).
func TestKeyOfOlderPairing(t *testing.T) {
	e := newEnv(t)
	c := machineConf()
	c.KeyringUser = ""
	writeConf(t, e.confPath, c)
	e.sec.Delete(secretsKey)
	e.sec.Set(secrets.OwnerKeyName(wiretest.ServerName), wiretest.OwnerKey)
	e.connect()
	e.reg.mu.Lock()
	defer e.reg.mu.Unlock()
	if e.reg.key != wiretest.OwnerKey || e.reg.standings != 1 {
		t.Fatalf("key %q, %d standing calls", e.reg.key, e.reg.standings)
	}
}

// A pairing made by this kwak reads only its own item. When that item is gone, the old shared
// item (another pairing's key, as of a real pairing next to a test one) is never used: the
// listener is unpaired and calls nothing.
func TestKeyNeverFromAnotherPairing(t *testing.T) {
	e := newEnv(t)
	e.sec.Delete(secretsKey)
	e.sec.Set(secrets.OwnerKeyName(wiretest.ServerName), wiretest.OldOwnerKey)
	err := e.l.connect(context.Background())
	var up *errUnpaired
	if !errors.As(err, &up) || up.status != textKeyMissing {
		t.Fatalf("connect: %v", err)
	}
	if n := e.reg.standingCalls(); n != 0 {
		t.Fatalf("%d standing calls with another pairing's key", n)
	}
}

// Two pairings of one server on one machine (a real one and a test one, each with its own
// KWAK_CONFIG_DIR) keep two items; each listener reads its own.
func TestKeyTwoPairingsOneKeyring(t *testing.T) {
	e := newEnv(t)
	e.sec.Set(secrets.PairingKeyName(wiretest.ServerName, wiretest.OldPairingID), wiretest.OldOwnerKey)
	e.sec.Set(secrets.OwnerKeyName(wiretest.ServerName), wiretest.OldOwnerKey)
	e.connect()
	e.reg.mu.Lock()
	defer e.reg.mu.Unlock()
	if e.reg.key != wiretest.OwnerKey {
		t.Fatalf("key %q, want this pairing's own", e.reg.key)
	}
}
