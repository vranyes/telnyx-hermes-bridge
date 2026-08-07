// Package server wires the gateway: dependency construction, HTTP routing,
// and middleware (correlation IDs, request logging, panic recovery).
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/vranyes/telnyx-hermes-bridge/internal/idgen"
	"github.com/vranyes/telnyx-hermes-bridge/internal/reqctx"
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// withRequestID tags every request with a correlation ID, honoring an
// inbound X-Correlation-ID if present (e.g. from Hermes or an upstream).
// It must wrap the logging middleware so the ID is in context when logged.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Correlation-ID")
		if id == "" {
			id = idgen.New()
		}
		w.Header().Set("X-Correlation-ID", id)
		next.ServeHTTP(w, r.WithContext(reqctx.WithID(r.Context(), id)))
	})
}

// probePaths are polled by orchestrators/observability on a fixed schedule;
// their successful requests are omitted from request logs to avoid noise.
var probePaths = map[string]bool{
	"/healthz": true,
	"/readyz":  true,
	"/metrics": true,
}

// withLogging logs one line per request with status and duration. Successful
// probe requests (healthz/readyz/metrics) are suppressed; failures still log.
func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		args := []any{
			"request_id", reqctx.ID(r),
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if probePaths[r.URL.Path] && sw.status < http.StatusBadRequest {
			return
		}
		logger.Info("http request", args...)
	})
}

// withRecovery converts panics into 500s so a single bad request cannot take
// down the pod.
func withRecovery(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered",
					"request_id", reqctx.ID(r),
					"panic", rec,
				)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
