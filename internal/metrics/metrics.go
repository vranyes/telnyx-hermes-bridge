// Package metrics provides a dependency-free Prometheus text-format metrics
// endpoint backed by atomic counters.
package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// Metrics aggregates gateway counters for the Prometheus scrape endpoint.
//
// Counters are grouped by the ADR that introduced them. The SMS-MVP surface
// (ADR-0006) carries the turn and reply counters; legacy counters from the
// retired Event/Action contract and the voice path are gone.
type Metrics struct {
	// Webhook ingress.
	WebhooksReceived atomic.Uint64
	WebhooksRejected atomic.Uint64
	WebhooksDeduped  atomic.Uint64

	// Turns (gateway -> Hermes chat/completions, ADR-0006 §2).
	TurnsStarted         atomic.Uint64 // turn entered the agent loop and returned a body
	TurnsCompleted       atomic.Uint64 // turn returned a non-empty assistant reply
	TurnsRetried         atomic.Uint64 // gateway->hermes retry attempts
	TurnsPermanentFailed atomic.Uint64 // rejected permanently (4xx other than 429/408)
	TurnsFallbackSent    atomic.Uint64 // turn ended in Exhausted/Permanent/unknown -> canned SMS fallback sent
	TurnsReplyEmpty      atomic.Uint64 // agent ran but produced no reply text (soft miss)
	TurnsQueueOverflow   atomic.Uint64 // per-sender queue overflow canned line sent
	// IdempotentReplays counts turns where a retried request returned the
	// cached response by Idempotency-Key (the open item #1 signal).
	IdempotentReplays atomic.Uint64

	// Reply send (gateway -> Telnyx, ADR-0006 §2 reply-send retry).
	ReplySendRetries atomic.Uint64 // reply-send retry attempts
	ReplySendFailed  atomic.Uint64 // reply-send budget exhausted; reply dropped silently
}

// Handler returns an HTTP handler rendering counters in Prometheus text
// format.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeCounter(w, "hermes_gateway_webhooks_received_total", "Inbound Telnyx webhooks received.", m.WebhooksReceived.Load())
		writeCounter(w, "hermes_gateway_webhooks_rejected_total", "Webhooks rejected (signature/parse/auth).", m.WebhooksRejected.Load())
		writeCounter(w, "hermes_gateway_webhooks_deduped_total", "Webhook deliveries ignored as duplicates.", m.WebhooksDeduped.Load())
		writeCounter(w, "hermes_gateway_turns_started_total", "Chat/completions turns that returned a body.", m.TurnsStarted.Load())
		writeCounter(w, "hermes_gateway_turns_completed_total", "Turns that returned a non-empty assistant reply.", m.TurnsCompleted.Load())
		writeCounter(w, "hermes_gateway_turns_retried_total", "Gateway->Hermes retry attempts.", m.TurnsRetried.Load())
		writeCounter(w, "hermes_gateway_turns_permanent_failed_total", "Turns rejected permanently by Hermes.", m.TurnsPermanentFailed.Load())
		writeCounter(w, "hermes_gateway_turns_fallback_sent_total", "Turns that ended in Exhausted/Permanent/unknown and triggered the canned SMS fallback.", m.TurnsFallbackSent.Load())
		writeCounter(w, "hermes_gateway_turns_reply_empty_total", "Turns that returned an empty assistant reply (soft miss).", m.TurnsReplyEmpty.Load())
		writeCounter(w, "hermes_gateway_turns_queue_overflow_total", "Per-sender queue overflow canned lines sent.", m.TurnsQueueOverflow.Load())
		writeCounter(w, "hermes_gateway_idempotent_replays_total", "Turns where a retried request hit the Hermes Idempotency-Key cache.", m.IdempotentReplays.Load())
		writeCounter(w, "hermes_gateway_reply_send_retries_total", "Reply-SMS send retry attempts.", m.ReplySendRetries.Load())
		writeCounter(w, "hermes_gateway_reply_send_failed_total", "Reply-SMS sends that exhausted retry and were dropped.", m.ReplySendFailed.Load())
	})
}

func writeCounter(w http.ResponseWriter, name, help string, v uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
}
