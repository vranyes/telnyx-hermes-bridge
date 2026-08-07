// Package reqctx stores and retrieves the per-request correlation ID in the
// request context. Shared by the server middleware and the handlers so the
// handlers package does not import the server package.
package reqctx

import (
	"context"
	"net/http"
)

type key struct{}

// WithID returns a context carrying the correlation ID.
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, key{}, id)
}

// FromContext returns the correlation ID stored in the context, or "" if
// absent. Works for contexts detached from the HTTP request (e.g. async
// retry tasks that carry the ID via context.WithoutCancel).
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(key{}).(string); ok {
		return v
	}
	return ""
}

// ID returns the correlation ID for the request, or "" if absent.
func ID(r *http.Request) string {
	return FromContext(r.Context())
}
