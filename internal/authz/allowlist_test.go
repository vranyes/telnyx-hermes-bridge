package authz

import (
	"io"
	"log/slog"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAllowlist_EmptyDeniesAll(t *testing.T) {
	a := NewAllowlist(nil, testLogger())
	if a.Allowed("+15551234567") {
		t.Fatal("empty allowlist should deny all callers")
	}
}

func TestAllowlist_Match(t *testing.T) {
	a := NewAllowlist([]string{"+15551234567", "+1 (555) 111-2222"}, testLogger())
	if !a.Allowed("+15551234567") {
		t.Fatal("exact match should be allowed")
	}
	if !a.Allowed("+15551112222") {
		t.Fatal("normalized match (spaces/dashes stripped) should be allowed")
	}
	if a.Allowed("+19990000000") {
		t.Fatal("unlisted number should be denied")
	}
	if a.Allowed("") {
		t.Fatal("empty number should be denied when allowlist enabled")
	}
}
