//go:build integration && linux

package e2e

import (
	"fmt"
	"testing"

	"github.com/fcanela/kwak/internal/registrar"
	"github.com/fcanela/kwak/internal/wire"
)

// Listener returns the user id of the pairing's own listener account (decision 42), as the
// registrar's state has it, and fails when the pairing has none.
func (r *Registrar) Listener(t *testing.T, pairing string) string {
	t.Helper()
	var lp string
	r.View(func(st *registrar.State) {
		if p := st.Pairings[pairing]; p != nil {
			lp = p.Listener
		}
	})
	if lp == "" {
		t.Fatalf("pairing %s has no listener account", pairing)
	}
	return "@" + lp + ":" + Server
}

// PairedNotice is the start of the bot's reply to `pair <code>` for pairing on machine, which
// names the pairing's listener account by its word pair (decision 42).
func PairedNotice(t *testing.T, r *Registrar, machine, pairing string) string {
	t.Helper()
	uid := r.Listener(t, pairing)
	n, err := wire.ParseLocalpart(uid[1 : len(uid)-len(":"+Server)])
	if err != nil || n.Pair == "" {
		t.Fatalf("listener account %s: %+v %v", uid, n, err)
	}
	return fmt.Sprintf("Paired machine '%s' (linux) as %s (pairing %s), asked from ", machine, n.Pair, pairing)
}
