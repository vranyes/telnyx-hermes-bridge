// Package authz enforces the caller allowlist — the SMS-MVP gateway's
// sole authorization control (ADR-0006 §4). The OIDC/JWKS workload-identity
// verifier and the /actions endpoint are retired; the gateway's public
// surface is inbound-only (Telnyx webhooks, Ed25519-verified).
package authz

import (
	"log/slog"
	"sync"

	"github.com/vranyes/telnyx-hermes-bridge/internal/phone"
)

// Allowlist restricts which phone numbers may drive the gateway. Per
// ADR-0002 there is no PIN gate; the allowlist is the access control, and
// confirmation of irreversible actions is Hermes's responsibility.
//
// The allowlist fails closed: an empty list denies everyone. config.Load
// requires at least one entry at startup, so the allow-all development mode
// is gone.
type Allowlist struct {
	mu      sync.RWMutex
	numbers map[string]struct{}
	logger  *slog.Logger
}

// NewAllowlist builds an allowlist from normalized numbers. An empty list
// denies all callers (fail closed); config.Load refuses to start without one.
func NewAllowlist(numbers []string, logger *slog.Logger) *Allowlist {
	if logger == nil {
		logger = slog.Default()
	}
	a := &Allowlist{
		numbers: make(map[string]struct{}, len(numbers)),
		logger:  logger,
	}
	for _, n := range numbers {
		if nrm := phone.Normalize(n); nrm != "" {
			a.numbers[nrm] = struct{}{}
		}
	}
	if len(a.numbers) == 0 {
		logger.Warn("authz: allowlist is empty; no callers permitted (fail closed)")
	}
	return a
}

// Allowed reports whether the given phone number may use the gateway.
func (a *Allowlist) Allowed(number string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.numbers[phone.Normalize(number)]
	return ok
}
