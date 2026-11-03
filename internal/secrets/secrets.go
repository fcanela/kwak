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

// OwnerKeyName is the keyring user of the owner key: "<server_name>/owner-key".
func OwnerKeyName(serverName string) string { return serverName + "/owner-key" }
