// Package phone normalizes phone number strings for allowlist comparison.
package phone

import "strings"

// Normalize canonicalizes a phone number for comparison: keeps a leading '+'
// and digits only, dropping punctuation, spaces, and country-call-internal
// characters.
func Normalize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}
