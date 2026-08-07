package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vranyes/telnyx-hermes-bridge/internal/config"
	"github.com/vranyes/telnyx-hermes-bridge/internal/reqctx"
	"github.com/vranyes/telnyx-hermes-bridge/internal/telnyx"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func telnyxFixture(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub), priv
}

func signWebhook(priv ed25519.PrivateKey, body []byte) (ts, sig string) {
	ts = strconv.FormatInt(time.Now().Unix(), 10)
	msg := append([]byte(ts), '|')
	msg = append(msg, body...)
	return ts, base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
}

func smsWebhookBody(t *testing.T, from, to string) []byte {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"data": map[string]any{
			"record_type": "event",
			"event_type":  "message.received",
			"id":          "evt-wire-1",
			"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
			"payload": map[string]any{
				"id":   "msg-wire-1",
				"from": map[string]string{"phone_number": from},
				"to":   []map[string]string{{"phone_number": to}},
				"text": "hello gateway",
			},
		},
	})
	return b
}

func chatCompletionBody() []byte {
	return []byte(`{"choices":[{"message":{"role":"assistant","content":"hello from hermes"}}]}`)
}

// newTestServer wires a gateway with stubbed-out Telnyx and Hermes
// dependencies so the test exercises the full HTTP pipeline.
func newTestServer(t *testing.T) (*Server, ed25519.PrivateKey) {
	t.Helper()
	telnyxPub, telnyxPriv := telnyxFixture(t)

	_ = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Telnyx SMS reply accepts everything.
		w.WriteHeader(http.StatusOK)
	}))
	hermesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(chatCompletionBody())
	}))
	t.Cleanup(hermesSrv.Close)

	cfg := config.Config{
		Addr:              ":0",
		TelnyxAPIKey:      "api-key",
		TelnyxPublicKey:   telnyxPub,
		TelnyxBaseURL:     "https://api.telnyx.example/v2",
		TelnyxFromNumber:  "+15559876543",
		HermesBaseURL:     hermesSrv.URL,
		APIServerKey:      "shared-secret",
		Allowlist:         []string{"+15551234567"},
		WebhookTolerance:  time.Minute,
		SystemPrompt:      "TEST",
		QueueOverflowText: "TEST",
		SMSFallbackText:   "TEST",

		HermesAPIPath:    "/v1/chat/completions",
		RetryBudget:      30 * time.Second,
		ReplyRetryBudget: 200 * time.Millisecond,
		WindowTurns:      3,
		PerSenderQueue:   4,
		DedupWindow:      time.Minute,
	}

	s, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return s, telnyxPriv
}

func TestNewFailsClosedOnInvalidTelnyxKey(t *testing.T) {
	cfg := config.Config{
		TelnyxPublicKey: "not-base64-!!!",
		Allowlist:       []string{"+15551234567"},
		HermesBaseURL:   "http://hermes",
		APIServerKey:    "secret",
		HermesAPIPath:   "/v1/chat/completions",
	}
	if _, err := New(cfg, testLogger()); err == nil {
		t.Fatal("New() with invalid TelnyxPublicKey succeeded, want error")
	}
}

func TestHealthRoutes(t *testing.T) {
	f, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "ok\n" {
		t.Fatalf("healthz = %d %q", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	f.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "ok\n" {
		t.Fatalf("readyz = %d %q", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	f.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rr.Code)
	}
	for _, want := range []string{"hermes_gateway_webhooks_received_total", "hermes_gateway_turns_completed_total"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("metrics missing %q:\n%s", want, rr.Body.String())
		}
	}
	for _, gone := range []string{"actions_received", "auth_rejected", "voice_calls", "events_queued"} {
		if strings.Contains(rr.Body.String(), gone) {
			t.Fatalf("metrics still contains retired counter %q", gone)
		}
	}
}

func TestFullSMSWiring(t *testing.T) {
	// SMS webhook -> chat/completions turn -> reply SMS via Telnyx. The
	// Telnyx API is stubbed by recording the outgoing POST.
	f, telnyxPriv := newTestServer(t)

	var replyMu sync.Mutex
	var sawReplyTo, sawReplyText string
	replySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		replyMu.Lock()
		sawReplyTo, _ = body["to"].(string)
		sawReplyText, _ = body["text"].(string)
		replyMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(replySrv.Close)
	// Patch the gateway's telnyx client to point at the reply spy.
	f.cfg.TelnyxBaseURL = replySrv.URL
	// Rebuild with the patched Telnyx URL so the client talks to the spy.
	s2, err := New(f.cfg, testLogger())
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	body := smsWebhookBody(t, "+15551234567", "+15559876543")
	ts, sig := signWebhook(telnyxPriv, body)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telnyx/sms", bytes.NewReader(body))
	req.Header.Set(telnyx.TimestampHeader, ts)
	req.Header.Set(telnyx.SignatureHeader, sig)
	s2.handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("sms webhook status = %d", rr.Code)
	}

	// Wait up to 2s for the asynchronous turn + reply to land in the spy.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		replyMu.Lock()
		got := sawReplyTo != "" && sawReplyText != ""
		replyMu.Unlock()
		if got {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	replyMu.Lock()
	defer replyMu.Unlock()
	if sawReplyTo == "" || sawReplyText == "" {
		t.Fatal("reply SMS never reached Telnyx")
	}
	if sawReplyTo != "+15551234567" {
		t.Errorf("reply to = %q, want +15551234567", sawReplyTo)
	}
	if !strings.Contains(sawReplyText, "hello from hermes") {
		t.Errorf("reply text = %q", sawReplyText)
	}
}

func TestServerRunShutdownsCleanly(t *testing.T) {
	f, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancel")
	}
}

func TestRequestIDMiddlewareHonorsInbound(t *testing.T) {
	var gotID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = reqctx.ID(r)
		w.WriteHeader(http.StatusNoContent)
	})
	wrapped := withRequestID(inner)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Correlation-ID", "client-provided-id")
	wrapped.ServeHTTP(rr, req)
	if gotID != "client-provided-id" {
		t.Fatalf("inbound id = %q, want client-provided-id", gotID)
	}
	if h := rr.Header().Get("X-Correlation-ID"); h != "client-provided-id" {
		t.Fatalf("echoed header = %q", h)
	}
}

func TestRequestIDMiddlewareGeneratesWhenAbsent(t *testing.T) {
	var gotID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = reqctx.ID(r)
		w.WriteHeader(http.StatusNoContent)
	})
	wrapped := withRequestID(inner)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if gotID == "" {
		t.Fatal("no correlation id generated")
	}
	if rr.Header().Get("X-Correlation-ID") != gotID {
		t.Fatal("generated id not echoed in response header")
	}
}

func TestLoggingSuppressesSuccessfulProbes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz":
			w.WriteHeader(http.StatusOK)
		case "/metrics":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	})
	wrapped := withLogging(logger, inner)

	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		buf.Reset()
		rr := httptest.NewRecorder()
		wrapped.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if buf.Len() != 0 {
			t.Errorf("probe %s logged on success: %q", p, buf.String())
		}
	}

	buf.Reset()
	failHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	rr := httptest.NewRecorder()
	withLogging(logger, failHandler).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !strings.Contains(buf.String(), "http request") {
		t.Errorf("failing probe did not log: %q", buf.String())
	}

	buf.Reset()
	rr = httptest.NewRecorder()
	wrapped.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/other", nil))
	if !strings.Contains(buf.String(), "http request") {
		t.Errorf("non-probe request did not log: %q", buf.String())
	}
}

func TestRecoveryConvertsPanicTo500(t *testing.T) {
	panicHandler := withRecovery(testLogger(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rr := httptest.NewRecorder()
	panicHandler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}
