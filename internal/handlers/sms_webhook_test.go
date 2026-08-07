package handlers

import (
	"bytes"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/vranyes/telnyx-hermes-bridge/internal/authz"
	"github.com/vranyes/telnyx-hermes-bridge/internal/hermes"
	"github.com/vranyes/telnyx-hermes-bridge/internal/metrics"
	"github.com/vranyes/telnyx-hermes-bridge/internal/reqctx"
	"github.com/vranyes/telnyx-hermes-bridge/internal/telnyx"
	"github.com/vranyes/telnyx-hermes-bridge/internal/transcript"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ed25519Priv carries the keypair and lets tests sign webhook payloads.
type ed25519Priv struct {
	key ed25519.PrivateKey
}

func (p *ed25519Priv) sign(msg []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(p.key, msg))
}

// telnyxFixture returns the base64 public key and a sign-capable private.
func telnyxFixture(t *testing.T) (pubB64 string, priv *ed25519Priv) {
	t.Helper()
	pub, p, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub), &ed25519Priv{key: p}
}

func webhookRequest(priv *ed25519Priv, body []byte) *http.Request {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	msg := append([]byte(ts), '|')
	msg = append(msg, body...)
	r := httptest.NewRequest(http.MethodPost, "/webhooks/telnyx/sms", bytes.NewReader(body))
	r.Header.Set(telnyx.TimestampHeader, ts)
	r.Header.Set(telnyx.SignatureHeader, priv.sign(msg))
	return r
}

func smsBody(from, to, text string) []byte {
	return smsBodyEvent("evt-msg-1", from, to, text)
}

func smsBodyEvent(eventID, from, to, text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"data": map[string]any{
			"record_type": "event",
			"event_type":  "message.received",
			"id":          eventID,
			"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
			"payload": map[string]any{
				"id":   "msg-" + eventID,
				"from": map[string]string{"phone_number": from},
				"to":   []map[string]string{{"phone_number": to}},
				"text": text,
			},
		},
	})
	return b
}

// chatBody produces an OpenAI chat/completions response body whose first
// choice's content is `reply`. An empty string is the soft miss.
func chatBody(reply string) []byte {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{"role": "assistant", "content": reply}},
		},
	})
	return b
}

type hermesSpy struct {
	mu       sync.Mutex
	requests []spyReq
}

type spyReq struct {
	idempotencyKey string
	auth           string
	messages       []hermes.Message
}

func (s *hermesSpy) add(r spyReq) {
	s.mu.Lock()
	s.requests = append(s.requests, r)
	s.mu.Unlock()
}

func (s *hermesSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *hermesSpy) last() spyReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return spyReq{}
	}
	return s.requests[len(s.requests)-1]
}

type telnyxSpy struct {
	mu      sync.Mutex
	smsSent []string
}

func (s *telnyxSpy) saw(to, text string) {
	s.mu.Lock()
	s.smsSent = append(s.smsSent, to+"|"+text)
	s.mu.Unlock()
}

func (s *telnyxSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.smsSent)
}

// smsFixture bundles everything a test needs to drive a wired SMSHandler.
type smsFixture struct {
	handler   *SMSHandler
	hermes    *hermesSpy
	telnyx    *telnyxSpy
	priv      *ed25519Priv
	window    *transcript.Store
	metrics   *metrics.Metrics
	hermesURL string
	telnyxURL string
}

// newSMSFixture wires an end-to-end handler. hermesHandler is invoked for
// every chat/completions turn; telnyxHandler for every SendSMS. The spies
// capture Idempotency-Key + messages (hermes) and to|text (telnyx).
func newSMSFixture(t *testing.T, hermesHandler, telnyxHandler http.HandlerFunc) *smsFixture {
	t.Helper()
	pubB64, priv := telnyxFixture(t)
	verifier, err := telnyx.NewVerifier(pubB64, 5*time.Minute)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	hSpy := &hermesSpy{}
	tSpy := &telnyxSpy{}

	hermesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req hermes.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		hSpy.add(spyReq{
			idempotencyKey: r.Header.Get("Idempotency-Key"),
			auth:           r.Header.Get("Authorization"),
			messages:       req.Messages,
		})
		if hermesHandler != nil {
			hermesHandler(w, r)
			return
		}
		_, _ = w.Write(chatBody("ok"))
	}))
	t.Cleanup(hermesSrv.Close)

	telnyxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		to, _ := body["to"].(string)
		text, _ := body["text"].(string)
		tSpy.saw(to, text)
		if telnyxHandler != nil {
			telnyxHandler(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(telnyxSrv.Close)

	al := authz.NewAllowlist([]string{"+15551234567"}, discardLogger())
	window := transcript.New(3)
	m := &metrics.Metrics{}
	hc := hermes.NewClient(hermesSrv.URL, "/v1/chat/completions", "shared-secret", 60*time.Second, hermesSrv.Client(), discardLogger(), m)
	tc := telnyx.NewClient("k", telnyxSrv.URL, "+15559876543", telnyxSrv.Client(), discardLogger())

	return &smsFixture{
		handler: &SMSHandler{
			Verifier:          verifier,
			Dedup:             telnyx.NewDedup(5 * time.Minute),
			Allowlist:         al,
			Hermes:            hc,
			Telnyx:            tc,
			Window:            window,
			Queue:             NewTurnQueue(4),
			Metrics:           m,
			Logger:            discardLogger(),
			Now:               time.Now,
			SystemPrompt:      "TEST SYSTEM PROMPT",
			QueueOverflowText: "TEST OVERFLOW",
			SMSFallbackText:   "TEST FALLBACK",
			ReplyRetryBudget:  300 * time.Millisecond,
		},
		hermes:    hSpy,
		telnyx:    tSpy,
		priv:      priv,
		window:    window,
		metrics:   m,
		hermesURL: hermesSrv.URL,
		telnyxURL: telnyxSrv.URL,
	}
}

func waitHermes(t *testing.T, spy *hermesSpy, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for spy.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d hermes turns observed, want %d", spy.count(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitReply(t *testing.T, spy *telnyxSpy, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for spy.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d replies observed, want %d", spy.count(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSMSHandler_HappyPath(t *testing.T) {
	f := newSMSFixture(t, nil, nil)
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, webhookRequest(f.priv, smsBody("+15551234567", "+15559876543", "hi")))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	waitHermes(t, f.hermes, 1)
	r := f.hermes.last()
	if r.idempotencyKey != "evt-msg-1" {
		t.Errorf("Idempotency-Key = %q, want evt-msg-1", r.idempotencyKey)
	}
	if r.auth != "Bearer shared-secret" {
		t.Errorf("auth = %q", r.auth)
	}
	if len(r.messages) < 2 || r.messages[0].Content != "TEST SYSTEM PROMPT" {
		t.Fatalf("system prompt not first: %+v", r.messages)
	}
	if last := r.messages[len(r.messages)-1]; last.Role != "user" || last.Content != "hi" {
		t.Fatalf("last message not user/hi: %+v", last)
	}
	waitReply(t, f.telnyx, 1)
	if !strings.Contains(f.telnyx.smsSent[0], "ok") {
		t.Fatalf("reply not sent: %v", f.telnyx.smsSent)
	}
}

func TestSMSHandler_RejectsBadSignature(t *testing.T) {
	f := newSMSFixture(t, nil, nil)
	body := smsBody("+15551234567", "+15559876543", "hi")
	r := webhookRequest(f.priv, body)
	r.Header.Set(telnyx.SignatureHeader, base64.StdEncoding.EncodeToString([]byte("forged")))
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if f.hermes.count() != 0 {
		t.Fatal("turn forwarded despite bad signature")
	}
}

func TestSMSHandler_Deduplicates(t *testing.T) {
	f := newSMSFixture(t, nil, nil)
	body := smsBody("+15551234567", "+15559876543", "dup")
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, webhookRequest(f.priv, body))
	waitHermes(t, f.hermes, 1)
	rr = httptest.NewRecorder()
	f.handler.ServeHTTP(rr, webhookRequest(f.priv, body))
	time.Sleep(100 * time.Millisecond)
	if f.hermes.count() != 1 {
		t.Fatalf("duplicate delivered: %d turns", f.hermes.count())
	}
}

func TestSMSHandler_DropsUnallowlistedSender(t *testing.T) {
	f := newSMSFixture(t, nil, nil)
	body := smsBody("+13330000000", "+15559876543", "nope")
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, webhookRequest(f.priv, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	time.Sleep(100 * time.Millisecond)
	if f.hermes.count() != 0 {
		t.Fatal("unallowlisted sender reached Hermes")
	}
}

func TestSMSHandler_FallbackOnHermesDown(t *testing.T) {
	// Hermes always returns 503; budget is shrunk so the test is fast.
	// Verifies exactly one fallback reply is sent (a previous design
	// had both a hermes-client Fallback seam and a handler case that
	// each sent their own copy — this is the regression guard).
	f := newSMSFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}, nil)
	f.handler.Hermes = hermes.NewClient(f.hermesURL, "/v1/chat/completions", "shared-secret", 150*time.Millisecond, http.DefaultClient, discardLogger(), f.metrics)

	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, webhookRequest(f.priv, smsBody("+15551234567", "+15559876543", "fail")))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	waitReply(t, f.telnyx, 1)
	if n := f.telnyx.count(); n != 1 {
		t.Fatalf("exactly one fallback SMS expected, got %d: %v", n, f.telnyx.smsSent)
	}
	if !strings.Contains(f.telnyx.smsSent[0], "TEST FALLBACK") {
		t.Fatalf("fallback not sent: %v", f.telnyx.smsSent)
	}
	if n := f.metrics.TurnsFallbackSent.Load(); n != 1 {
		t.Fatalf("TurnsFallbackSent = %d, want 1", n)
	}
}

func TestSMSHandler_EmptyReplySoftMiss(t *testing.T) {
	// Hermes returns 200 with empty content; the gateway sends nothing.
	f := newSMSFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(chatBody(""))
	}, nil)
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, webhookRequest(f.priv, smsBody("+15551234567", "+15559876543", "go")))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	waitHermes(t, f.hermes, 1)
	time.Sleep(150 * time.Millisecond)
	if f.telnyx.count() != 0 {
		t.Fatalf("soft-miss case sent a reply: %v", f.telnyx.smsSent)
	}
}

func TestSMSHandler_PerSenderQueueOverflow(t *testing.T) {
	// Per-sender cap is 4 (1 active + 4 queued). 6 webhooks → at most 5
	// turns; the 6th triggers the overflow canned line.
	f := newSMSFixture(t, nil, nil)

	// holdSrv blocks until release; it records into f.hermes so the
	// existing waitHermes/telnyx spies work.
	release := make(chan struct{})
	holdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req hermes.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.hermes.add(spyReq{
			idempotencyKey: r.Header.Get("Idempotency-Key"),
			auth:           r.Header.Get("Authorization"),
			messages:       req.Messages,
		})
		<-release
		_, _ = w.Write(chatBody("ok"))
	}))
	t.Cleanup(holdSrv.Close)
	f.handler.Hermes = hermes.NewClient(holdSrv.URL, "/v1/chat/completions", "shared-secret", 60*time.Second, holdSrv.Client(), discardLogger(), f.metrics)

	for i := 0; i < 6; i++ {
		body := smsBodyEvent("evt-"+strconv.Itoa(i), "+15551234567", "+15559876543", "msg "+strconv.Itoa(i))
		rr := httptest.NewRecorder()
		f.handler.ServeHTTP(rr, webhookRequest(f.priv, body))
		if rr.Code != http.StatusOK {
			t.Fatalf("webhook %d status = %d", i, rr.Code)
		}
	}
	waitReply(t, f.telnyx, 1)
	if !strings.Contains(f.telnyx.smsSent[0], "TEST OVERFLOW") {
		t.Fatalf("overflow canned line not sent: %v", f.telnyx.smsSent)
	}

	close(release)
	waitHermes(t, f.hermes, 5)
	if f.hermes.count() != 5 {
		t.Fatalf("expected 5 turns delivered (1 active + 4 queued), got %d", f.hermes.count())
	}
}

func TestSMSHandler_RollingWindowReplayed(t *testing.T) {
	f := newSMSFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(chatBody("ok reply"))
	}, nil)

	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, webhookRequest(f.priv, smsBodyEvent("evt-1", "+15551234567", "+15559876543", "first")))
	waitHermes(t, f.hermes, 1)

	rr = httptest.NewRecorder()
	f.handler.ServeHTTP(rr, webhookRequest(f.priv, smsBodyEvent("evt-2", "+15551234567", "+15559876543", "second")))
	waitHermes(t, f.hermes, 2)

	r := f.hermes.last()
	if len(r.messages) != 4 {
		t.Fatalf("expected 4 messages (sys+win+win+new user), got %d", len(r.messages))
	}
	if r.messages[1].Role != "user" || r.messages[1].Content != "first" {
		t.Errorf("messages[1] = %+v", r.messages[1])
	}
	if r.messages[2].Role != "assistant" || r.messages[2].Content != "ok reply" {
		t.Errorf("messages[2] = %+v", r.messages[2])
	}
	if r.messages[3].Role != "user" || r.messages[3].Content != "second" {
		t.Errorf("messages[3] = %+v", r.messages[3])
	}
}

func TestSMSHandler_IgnoresNonMessageEvent(t *testing.T) {
	f := newSMSFixture(t, nil, nil)
	body, _ := json.Marshal(map[string]any{
		"data": map[string]any{
			"record_type": "event",
			"event_type":  "message.sent",
			"id":          "evt-msg-sent",
			"payload":     map[string]any{"id": "msg-sent-1"},
		},
	})
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, webhookRequest(f.priv, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	time.Sleep(100 * time.Millisecond)
	if f.hermes.count() != 0 {
		t.Fatal("non-message event reached Hermes")
	}
}

func TestSMSHandler_BadJSONPayload(t *testing.T) {
	f := newSMSFixture(t, nil, nil)
	body := []byte(`{not valid json`)
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, webhookRequest(f.priv, body))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if f.hermes.count() != 0 {
		t.Fatal("unparseable webhook reached Hermes")
	}
}

func TestSMSHandler_RetriesReplySend(t *testing.T) {
	// First Telnyx send returns 503, subsequent ones return 200: the
	// handler retries the reply at most once. We patch the reply schedule
	// to be quick so the test finishes fast.
	orig := replySchedule
	replySchedule = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	defer func() { replySchedule = orig }()

	var attempts int32
	telnyxHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	f := newSMSFixture(t, nil, telnyxHandler)
	f.handler.ReplyRetryBudget = 1 * time.Second

	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, webhookRequest(f.priv, smsBodyEvent("evt-r", "+15551234567", "+15559876543", "hi")))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	waitHermes(t, f.hermes, 1)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&attempts) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&attempts) < 2 {
		t.Fatalf("reply not retried: %d attempts", atomic.LoadInt32(&attempts))
	}
}

func TestReplySendDeadlineCoversWorstCaseJitter(t *testing.T) {
	// The reply-send deadline must fit the full schedule under worst-case
	// ±20% jitter plus a send allowance. The old budget+1s deadline (16s for
	// a 15s schedule) did not: the jittered schedule alone can reach 18s, so
	// the retry budget expired mid-backoff and the reply was dropped before
	// its retries ran.
	budget := 15 * time.Second
	var maxJittered time.Duration
	for _, d := range replySchedule {
		maxJittered += time.Duration(float64(d) * 1.2)
	}
	if d := replySendDeadline(budget); d < maxJittered+time.Second {
		t.Fatalf("replySendDeadline(%s) = %s, want >= %s (worst-case jittered schedule + send allowance)",
			budget, d, maxJittered+time.Second)
	}
}

func TestSMSHandler_ReplySendRunsFullScheduleOnTightBudget(t *testing.T) {
	// Regression guard: with Telnyx failing fast and the retry budget set to
	// exactly the nominal schedule sum (mirroring production's 15s budget vs
	// the 1/2/4/8 = 15s schedule), the full schedule must still run — every
	// retry fires before exhaustion. A deadline with only a token margin, or
	// jitter applied after the remaining-budget cap, drops the reply mid-
	// backoff without the final attempt.
	orig := replySchedule
	replySchedule = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	defer func() { replySchedule = orig }()

	var attempts int32
	telnyxHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
	})
	f := newSMSFixture(t, nil, telnyxHandler)
	f.handler.ReplyRetryBudget = 70 * time.Millisecond // == sum(replySchedule)

	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, webhookRequest(f.priv, smsBodyEvent("evt-sched", "+15551234567", "+15559876543", "hi")))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	waitHermes(t, f.hermes, 1)
	// Wait until the reply-send goroutine has fully exhausted (it bumps
	// ReplySendFailed just before returning) so the deferred restore of the
	// patched replySchedule cannot race its reads.
	deadline := time.Now().Add(3 * time.Second)
	for f.metrics.ReplySendFailed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&attempts); got != int32(1+len(replySchedule)) {
		t.Fatalf("reply-send attempts = %d, want %d (full schedule under tight budget)", got, 1+len(replySchedule))
	}
}

func TestSMSHandler_ReplySendDropLogCarriesContext(t *testing.T) {
	// The drop logs must carry enough to correlate the lost reply back to
	// the request that produced it: request id, Telnyx event id, attempt
	// count, and elapsed/budget. Without the request id, the three "dropped
	// silently" warns are un-correlatable to any turn.
	orig := replySchedule
	replySchedule = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	defer func() { replySchedule = orig }()

	var buf bytes.Buffer
	var attempts int32
	telnyxHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
	})
	f := newSMSFixture(t, nil, telnyxHandler)
	f.handler.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	f.handler.ReplyRetryBudget = 30 * time.Millisecond // == sum(replySchedule)

	rr := httptest.NewRecorder()
	r := webhookRequest(f.priv, smsBodyEvent("evt-drop", "+15551234567", "+15559876543", "hi"))
	r = r.WithContext(reqctx.WithID(r.Context(), "rid-123"))
	f.handler.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	waitHermes(t, f.hermes, 1)
	deadline := time.Now().Add(3 * time.Second)
	for f.metrics.ReplySendFailed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	out := buf.String()
	for _, want := range []string{
		"request_id=rid-123",
		"event_id=evt-drop",
		"attempts=3",
		"elapsed=",
		"budget=30ms",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("drop log missing %q:\n%s", want, out)
		}
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("reply-send attempts = %d, want 3", got)
	}
}

func TestTurnQueue_AtMostOneActivePerSender(t *testing.T) {
	q := NewTurnQueue(2) // 1 active + 2 queued
	t1 := q.Admit("a")
	if t1 == nil {
		t.Fatal("first Admit must succeed")
	}
	t2 := q.Admit("a")
	t3 := q.Admit("a")
	if t2 == nil || t3 == nil {
		t.Fatalf("queued admits must succeed: t2=%v t3=%v", t2, t3)
	}

	// cap+1 = 3 → 4th must overflow.
	if t4 := q.Admit("a"); t4 != nil {
		t.Fatal("4th admit must overflow")
	}

	// Wait for the first ticket to acquire (immediate, it's active),
	// run, then release.
	done1 := make(chan struct{})
	go func() { t1.Wait(); t1.Release(); close(done1) }()
	select {
	case <-done1:
	case <-time.After(time.Second):
		t.Fatal("first ticket Wait/Release did not return")
	}

	// Second and third tickets wake in FIFO order.
	done2 := make(chan struct{})
	go func() { t2.Wait(); t2.Release(); close(done2) }()
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("second ticket did not unblock after release of first")
	}

	done3 := make(chan struct{})
	go func() { t3.Wait(); t3.Release(); close(done3) }()
	select {
	case <-done3:
	case <-time.After(time.Second):
		t.Fatal("third ticket did not unblock")
	}
}

func TestTurnQueue_OverflowAfterDrain(t *testing.T) {
	q := NewTurnQueue(1) // 1 active + 1 queued → max 2 admits succeed
	t1 := q.Admit("a")
	if t1 == nil {
		t.Fatal("first admit must succeed")
	}
	t2 := q.Admit("a")
	if t2 == nil {
		t.Fatal("second admit must succeed (1 queued behind the active)")
	}
	if t3 := q.Admit("a"); t3 != nil {
		t.Fatal("third admit must overflow (cap=1 → max 2)")
	}

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	go func() { t1.Wait(); t1.Release(); close(done1) }()
	go func() { t2.Wait(); t2.Release(); close(done2) }()
	<-done1
	<-done2

	if t4 := q.Admit("a"); t4 == nil {
		t.Fatal("admit after full drain must succeed")
	} else {
		done4 := make(chan struct{})
		go func() { t4.Wait(); t4.Release(); close(done4) }()
		select {
		case <-done4:
		case <-time.After(time.Second):
			t.Fatal("post-drain ticket did not complete")
		}
	}
}

func TestTurnQueue_DifferentSendersIndependent(t *testing.T) {
	q := NewTurnQueue(4)
	t1 := q.Admit("alice")
	t2 := q.Admit("bob")
	if t1 == nil || t2 == nil {
		t.Fatalf("independent sends must both admit: alice=%v bob=%v", t1, t2)
	}
	done1 := make(chan struct{})
	done2 := make(chan struct{})
	go func() { t1.Wait(); t1.Release(); close(done1) }()
	go func() { t2.Wait(); t2.Release(); close(done2) }()
	<-done1
	<-done2
}

func TestTurnQueue_MemoryEvictedWhenDrained(t *testing.T) {
	q := NewTurnQueue(4)
	tk := q.Admit("a")
	done := make(chan struct{})
	go func() { tk.Wait(); tk.Release(); close(done) }()
	<-done

	q.mu.Lock()
	_, hasRunning := q.running["a"]
	_, hasQueued := q.queued["a"]
	q.mu.Unlock()
	if hasRunning || hasQueued {
		t.Fatalf("per-sender entry not evicted after drain: running=%v queued=%v",
			hasRunning, hasQueued)
	}
}

func TestTurnQueue_AdmitIsNonBlocking(t *testing.T) {
	// Admit must always return within milliseconds even when the per-
	// sender queue is full — the HTTP handler thread cannot block here.
	q := NewTurnQueue(2)
	t1 := q.Admit("a")
	t2 := q.Admit("a")
	t3 := q.Admit("a")
	if t1 == nil || t2 == nil || t3 == nil {
		t.Fatalf("3 admits under cap=2 must all succeed (1 active + 2 queued): t1=%v t2=%v t3=%v",
			t1, t2, t3)
	}

	start := time.Now()
	for i := 0; i < 100; i++ {
		_ = q.Admit("a")
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("100 admits must complete quickly, took %s", elapsed)
	}

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	done3 := make(chan struct{})
	go func() { t1.Wait(); t1.Release(); close(done1) }()
	go func() { t2.Wait(); t2.Release(); close(done2) }()
	go func() { t3.Wait(); t3.Release(); close(done3) }()
	<-done1
	<-done2
	<-done3
}
