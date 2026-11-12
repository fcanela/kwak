package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/secrets/secretstest"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

// The keyring bug of decision 40: every pairing of a server used to share one item, so a test
// pairing next to a real one overwrote the real key. Now each pairing has its own item.
func TestTwoPairingsOneMachine(t *testing.T) {
	keys := secretstest.Mem()
	realRig := newRigOn(t, keys, false)
	realRig.paired() // an older kwak: the shared item
	test := newRigOn(t, keys, true)
	test.reg.test, test.reg.pairing = true, "5e5e5e5e"
	test.ln.replies[wire.OpReload] = []string{`{"v":1,"ok":true}`}
	code, out, errs := test.pair("--owner", "fcanela")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "    pair test "+wiretest.PairCode+"\n") || !strings.Contains(out, "This is a test pairing") {
		t.Errorf("output:\n%s", out)
	}
	if cfg := test.config(); cfg["test"] != true || cfg["keyring_user"] != "kwak.test/5e5e5e5e/owner-key" {
		t.Errorf("test config %v", cfg)
	}
	if realRig.key() != wiretest.OldOwnerKey {
		t.Fatalf("the real pairing's key became %q", realRig.key())
	}
	if test.key() != wiretest.OwnerKey {
		t.Fatalf("the test pairing's key is %q", test.key())
	}
	if got := strings.Join(keys.Items(), " "); got != "kwak.test/5e5e5e5e/owner-key kwak.test/owner-key" {
		t.Errorf("keyring items %s", got)
	}

	// Unpairing the test machine leaves the real key alone.
	if code, _, errs := test.run(test.cli.Unpair, ""); code != 0 {
		t.Fatalf("unpair: exit %d: %s", code, errs)
	}
	if realRig.key() != wiretest.OldOwnerKey || strings.Join(keys.Items(), " ") != "kwak.test/owner-key" {
		t.Errorf("after the test unpair: real key %q, items %v", realRig.key(), keys.Items())
	}
}

// A re-pair made by this kwak gets a new pairing id, so a new item; the old item goes once the
// registrar took the new key, and stays when it did not.
func TestRepairMovesKeyringItem(t *testing.T) {
	r := newRig(t)
	r.ln.replies[wire.OpReload] = []string{`{"v":1,"ok":true}`, `{"v":1,"ok":true}`}
	if code, _, errs := r.pair("--owner", "fcanela"); code != 0 {
		t.Fatalf("first: exit %d: %s", code, errs)
	}
	r.reg.pairing = "5e5e5e5e"
	if code, _, errs := r.run(r.cli.Pair, ""); code != 0 {
		t.Fatalf("re-pair: exit %d: %s", code, errs)
	}
	if got := strings.Join(r.ownerItems(), " "); got != "kwak.test/5e5e5e5e/owner-key" {
		t.Errorf("keyring items %s", got)
	}
	if r.key() != wiretest.OwnerKey || r.config()["keyring_user"] != "kwak.test/5e5e5e5e/owner-key" {
		t.Errorf("key %q config %v", r.key(), r.config())
	}

	// The confirmation fails: the old item and config are back, the new item is gone.
	before := r.config()
	r.reg.pairing, r.reg.standing = "6f6f6f6f", 502
	if code, _, errs := r.run(r.cli.Pair, ""); code != 1 || !strings.Contains(errs, "The old key and config are back.") {
		t.Fatalf("failed re-pair: exit %d: %s", code, errs)
	}
	if got := strings.Join(r.ownerItems(), " "); got != "kwak.test/5e5e5e5e/owner-key" {
		t.Errorf("after a failed re-pair: keyring items %s", got)
	}
	if !jsonSame(before, r.config()) || r.key() != wiretest.OwnerKey {
		t.Errorf("after a failed re-pair: key %q config %v", r.key(), r.config())
	}
}

// A dev build that gets a normal pairing says so: its agents join the main agents room.
func TestDevBuildNormalPairingNote(t *testing.T) {
	r := newRigOn(t, secretstest.Mem(), true)
	r.ln.replies[wire.OpReload] = []string{`{"v":1,"ok":true}`}
	code, out, errs := r.pair("--owner", "fcanela")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "This is a dev build of kwak, so it suggests a test pairing.") ||
		!strings.Contains(out, "this dev build got a normal pairing") {
		t.Errorf("output:\n%s", out)
	}
	if _, ok := r.config()["test"]; ok {
		t.Errorf("a normal pairing wrote test: %v", r.config())
	}
}

// unpair --force on a config.json it cannot read still finds the right item: the pairing's own
// one when keyring_user names it, never the shared one of another pairing.
func TestUnpairForceBrokenConfigKeyringItem(t *testing.T) {
	r := newRig(t)
	r.ln.replies[wire.OpReload] = []string{`{"v":1,"ok":true}`}
	if code, _, errs := r.pair("--owner", "fcanela"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	r.keys.Set(secrets.OwnerKeyName(wiretest.ServerName), wiretest.OldOwnerKey) // another pairing's
	data := `{"version":1,"server":"http://not-https","server_name":"kwak.test","pairing_id":"` + wiretest.PairingID +
		`","keyring_user":"` + keyName + `"}`
	writeFile(r.cfgDir, ConfigFile, []byte(data))
	if code, out, errs := r.run(r.cli.Unpair, "", "--force"); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	if got := strings.Join(r.keys.Items(), " "); got != "kwak.test/owner-key" {
		t.Errorf("keyring items %s, want only the other pairing's", got)
	}
}

// kwak status names a dev build's folders, and marks a test pairing (decision 40).
func TestStatusDevAndTest(t *testing.T) {
	r := newRigOn(t, secretstest.Mem(), true)
	r.ln.replies[wire.OpStatus] = []string{`{"v":1,"ok":true,"kwak":"0.1.0","state":"syncing","caller_checks":"peer",` +
		`"owner":"fcanela","machine":"laptop","pairing_id":"5e5e5e5e","server":"https://kwak.test","test":true,` +
		`"resume_runs":0,"resume_active":0,"sessions":[]}`}
	code, out, errs := r.run(r.cli.Status, "")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{
		"Dev build: it uses its own test folder (config " + r.cfgDir + ", state " + r.stateDir + "), not a real pairing's.\n",
		"Pairing: fcanela on laptop, pairing 5e5e5e5e, server https://kwak.test (a test pairing: only the test rooms)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// A release build prints no dev line.
	r = newRig(t)
	r.ln.replies[wire.OpStatus] = []string{`{"v":1,"ok":true,"kwak":"0.1.0","state":"syncing","caller_checks":"peer","resume_runs":0,"resume_active":0,"sessions":[]}`}
	if _, out, _ := r.run(r.cli.Status, ""); strings.Contains(out, "Dev build") {
		t.Errorf("release status:\n%s", out)
	}
}

// Two folders on one machine that an older kwak paired both read the old shared item (decision
// 40). Re-pairing one of them with this kwak, here as a test pairing, keeps that item: the other
// folder still uses it. The earlier pairing stays live at the registrar, and pair says how to
// revoke it.
func TestRepairOfASecondLegacyFolderKeepsTheSharedItem(t *testing.T) {
	keys := secretstest.Mem()
	realRig := newRigOn(t, keys, false)
	realRig.paired()
	dev := newRigOn(t, keys, true)
	dev.paired() // the same shared item, as an older kwak wrote it
	dev.reg.test, dev.reg.pairing = true, "5e5e5e5e"
	dev.ln.replies[wire.OpReload] = []string{`{"v":1,"ok":true}`}
	code, out, errs := dev.pair()
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if realRig.key() != wiretest.OldOwnerKey {
		t.Fatalf("the other folder's key became %q", realRig.key())
	}
	if got := strings.Join(keys.Items(), " "); got != "kwak.test/5e5e5e5e/owner-key kwak.test/owner-key" {
		t.Errorf("keyring items %s", got)
	}
	for _, want := range []string{
		`The old shared keyring item "kwak.test/owner-key" (service kwak) is kept`,
		"The earlier pairing " + wiretest.OldPairingID + " stays live until you revoke it or it expires",
		"send \"revoke " + wiretest.OldPairingID + "\" to " + wiretest.Bot + ".\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "no longer accepts") {
		t.Errorf("output says the old key is refused:\n%s", out)
	}
}

// A pairing of the other kind from a config this kwak paired is a new pairing (decision 41): the
// registrar keeps the earlier one, so pair says in one line that it stays live and how to revoke
// it, and removes the earlier pairing's own keyring item.
func TestRepairAcrossKindsNamesTheLivePairing(t *testing.T) {
	r := newRig(t)
	r.paired()
	oldItem := secrets.PairingKeyName(wiretest.ServerName, wiretest.OldPairingID)
	cfg := r.config()
	cfg["keyring_user"] = oldItem
	b, _ := json.Marshal(cfg)
	writeFile(r.cfgDir, ConfigFile, b)
	r.keys.Delete(legacyKeyName)
	r.keys.Set(oldItem, wiretest.OldOwnerKey)
	r.reg.test, r.reg.pairing = true, "5e5e5e5e"
	r.ln.replies[wire.OpReload] = []string{`{"v":1,"ok":true}`}
	code, out, errs := r.run(r.cli.Pair, "")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if got := strings.Join(r.ownerItems(), " "); got != "kwak.test/5e5e5e5e/owner-key" {
		t.Errorf("keyring items %s", got)
	}
	want := "The earlier pairing " + wiretest.OldPairingID + " stays live until you revoke it or it expires; to end it, send \"revoke " +
		wiretest.OldPairingID + "\" to " + wiretest.Bot + ".\n"
	if !strings.Contains(out, want) || strings.Contains(out, "is kept") {
		t.Errorf("output:\n%s", out)
	}
}
