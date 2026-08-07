package reqctx

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestWithIDAndFromContext(t *testing.T) {
	ctx := WithID(context.Background(), "req-1")
	if got := FromContext(ctx); got != "req-1" {
		t.Fatalf("FromContext = %q, want req-1", got)
	}
}

func TestFromContextAbsent(t *testing.T) {
	if got := FromContext(context.Background()); got != "" {
		t.Fatalf("FromContext(empty) = %q, want \"\"", got)
	}
}

func TestFromContextNonStringValue(t *testing.T) {
	ctx := context.WithValue(context.Background(), key{}, 42)
	if got := FromContext(ctx); got != "" {
		t.Fatalf("FromContext(non-string) = %q, want \"\"", got)
	}
}

func TestIDFromRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if got := ID(r); got != "" {
		t.Fatalf("ID(unset) = %q, want \"\"", got)
	}
	r = r.WithContext(WithID(r.Context(), "req-2"))
	if got := ID(r); got != "req-2" {
		t.Fatalf("ID(set) = %q, want req-2", got)
	}
}
