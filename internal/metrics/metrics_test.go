package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerRendersCounters(t *testing.T) {
	m := &Metrics{}
	m.WebhooksReceived.Add(3)
	m.TurnsCompleted.Add(5)
	m.TurnsReplyEmpty.Add(1)

	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}

	body := rr.Body.String()
	for _, want := range []string{
		"hermes_gateway_webhooks_received_total 3",
		"hermes_gateway_turns_completed_total 5",
		"hermes_gateway_turns_reply_empty_total 1",
		"hermes_gateway_webhooks_rejected_total 0",
		"hermes_gateway_reply_send_retries_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	for _, gone := range []string{
		"events_queued",
		"actions_received",
		"voice_calls",
		"auth_rejected",
	} {
		if strings.Contains(body, gone) {
			t.Errorf("metrics output still contains retired metric %q", gone)
		}
	}
}
