// Package secrets is the keyring seam. The OS keyring lives in secrets/oskeyring and the test
// double in secrets/secretstest.
package secrets

import "errors"

// ErrNotFound is returned by Get when the key has no value.
var ErrNotFound = errors.New("secrets: not found")

// ErrTimeout is returned when the keyring does not answer within its deadline (locked?).
var ErrTimeout = errors.New("secrets: keyring did not answer in time")

// Service is the keyring service of every kwak item.
const Service = "kwak"

// Secrets stores string values by key.
type Secrets interface {
	Get(key string) (string, error) // ErrNotFound
	Set(key, value string) error
	Delete(key string) error // ErrNotFound when there is nothing to delete
}

// OwnerKeyName is the old, shared keyring user of the owner key: "<server_name>/owner-key". kwak
// before 2026-11-12 kept every pairing of a server under it, so two pairings on one machine (a
// real one and a test one with another KWAK_CONFIG_DIR, decision 40) overwrote each other's key.
// It is still read for a config.json that an older kwak wrote (KeyName).
func OwnerKeyName(serverName string) string { return serverName + "/owner-key" }

// PairingKeyName is the keyring user of one pairing's owner key:
// "<server_name>/<pairing_id>/owner-key". Each pairing has its own item, so pairings on one
// machine never share one.
func PairingKeyName(serverName, pairingID string) string {
	return serverName + "/" + pairingID + "/owner-key"
}

// KeyName is the keyring user that holds the owner key of the pairing a config.json describes.
// kwak pair writes "keyring_user" into config.json since 2026-11-12, and it is used when it is
// exactly PairingKeyName(serverName, pairingID). Otherwise the config was written by an older
// kwak (no keyring_user), or re-paired by one (a keyring_user left over from an earlier pairing),
// and that kwak stored the key under the old shared OwnerKeyName, which is then read. So a machine
// paired before keeps working with no action, and a pairing made by this kwak never reads another
// pairing's key.
func KeyName(serverName, pairingID, keyringUser string) string {
	if pairingID != "" && keyringUser == PairingKeyName(serverName, pairingID) {
		return keyringUser
	}
	return OwnerKeyName(serverName)
}
