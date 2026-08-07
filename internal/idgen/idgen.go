// Package idgen generates random hexadecimal identifiers.
package idgen

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a 128-bit random hex string. Sufficient for correlation and
// idempotency keys in the MVP's single-replica, best-effort posture.
func New() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failures are unrecoverable; panic is the honest exit.
		panic("idgen: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
