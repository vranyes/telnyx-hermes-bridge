// Package handlers contains the HTTP endpoints of the gateway.
package handlers

import "net/http"

// Healthz is the liveness probe.
func Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

// Readyz is the readiness probe. The MVP holds no durable dependencies, so
// readiness is always true when the process is up.
func Readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}
