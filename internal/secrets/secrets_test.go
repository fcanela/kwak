package secrets

import "testing"

func TestKeyNames(t *testing.T) {
	if got := OwnerKeyName("kwak.test"); got != "kwak.test/owner-key" {
		t.Fatalf("OwnerKeyName = %q", got)
	}
	if got := PairingKeyName("kwak.test", "3f9a1c2e"); got != "kwak.test/3f9a1c2e/owner-key" {
		t.Fatalf("PairingKeyName = %q", got)
	}
	// Two pairings of one server on one machine get two items.
	if PairingKeyName("kwak.test", "3f9a1c2e") == PairingKeyName("kwak.test", "77aa00bb") {
		t.Fatal("two pairings share a keyring item")
	}
	for _, c := range []struct {
		name, pairing, user, want string
	}{
		// Written by this kwak: its own item.
		{"own item", "3f9a1c2e", "kwak.test/3f9a1c2e/owner-key", "kwak.test/3f9a1c2e/owner-key"},
		// Written by an older kwak: the shared item, so the machine keeps working with no action.
		{"older kwak", "3f9a1c2e", "", "kwak.test/owner-key"},
		// Re-paired by an older kwak, which kept the field of the earlier pairing and wrote the
		// shared item.
		{"left over", "77aa00bb", "kwak.test/3f9a1c2e/owner-key", "kwak.test/owner-key"},
		// A field naming another server's item is never followed.
		{"other server", "3f9a1c2e", "evil.test/3f9a1c2e/owner-key", "kwak.test/owner-key"},
		{"no pairing id", "", "kwak.test//owner-key", "kwak.test/owner-key"},
	} {
		if got := KeyName("kwak.test", c.pairing, c.user); got != c.want {
			t.Errorf("%s: KeyName = %q, want %q", c.name, got, c.want)
		}
	}
}
