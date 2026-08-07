package idgen

import (
	"strings"
	"testing"
)

func TestNewLength(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := New()
		if len(id) != 32 {
			t.Fatalf("New() length = %d, want 32", len(id))
		}
		if strings.ContainsAny(id, "-") {
			t.Fatalf("New() = %q contains '-', want plain hex", id)
		}
		if !isHex(id) {
			t.Fatalf("New() = %q is not hex", id)
		}
	}
}

func TestNewUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 10000)
	for i := 0; i < 10000; i++ {
		id := New()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
