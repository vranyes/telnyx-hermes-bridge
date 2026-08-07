package telnyx

import (
	"crypto/ed25519"
	"encoding/base64"
	"strconv"
	"testing"
	"time"
)

func TestVerifier_ValidSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v, err := NewVerifier(base64.StdEncoding.EncodeToString(pub), 5*time.Minute)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	body := []byte(`{"data":{"event_type":"message.received"}}`)
	ts := time.Now().Unix()
	msg := append([]byte(int64Str(ts)), '|')
	msg = append(msg, body...)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))

	if err := v.Verify(int64Str(ts), sig, body); err != nil {
		t.Fatalf("Verify valid: %v", err)
	}
}

func TestVerifier_RejectsTamperedBody(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v, _ := NewVerifier(base64.StdEncoding.EncodeToString(pub), 5*time.Minute)

	body := []byte(`{"data":{"event_type":"message.received"}}`)
	ts := time.Now().Unix()
	msg := append([]byte(int64Str(ts)), '|')
	msg = append(msg, body...)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))

	tampered := []byte(`{"data":{"event_type":"message.sent"}}`)
	if err := v.Verify(int64Str(ts), sig, tampered); err != ErrInvalidSignature {
		t.Fatalf("want ErrInvalidSignature, got %v", err)
	}
}

func TestVerifier_RejectsTamperedTimestamp(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v, _ := NewVerifier(base64.StdEncoding.EncodeToString(pub), 5*time.Minute)

	body := []byte(`{"data":{"event_type":"message.received"}}`)
	ts := time.Now().Unix()
	msg := append([]byte(int64Str(ts)), '|')
	msg = append(msg, body...)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))

	if err := v.Verify(int64Str(ts+1), sig, body); err != ErrInvalidSignature {
		t.Fatalf("want ErrInvalidSignature, got %v", err)
	}
}

func TestVerifier_RejectsStaleTimestamp(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v, _ := NewVerifier(base64.StdEncoding.EncodeToString(pub), time.Minute)

	body := []byte(`{"data":{}}`)
	old := time.Now().Add(-2 * time.Hour).Unix()
	msg := append([]byte(int64Str(old)), '|')
	msg = append(msg, body...)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))

	if err := v.Verify(int64Str(old), sig, body); err != ErrTimestampTooOld {
		t.Fatalf("want ErrTimestampTooOld, got %v", err)
	}
}

func TestVerifier_RejectsFutureTimestamp(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v, _ := NewVerifier(base64.StdEncoding.EncodeToString(pub), time.Minute)

	body := []byte(`{"data":{}}`)
	future := time.Now().Add(2 * time.Hour).Unix()
	msg := append([]byte(int64Str(future)), '|')
	msg = append(msg, body...)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))

	if err := v.Verify(int64Str(future), sig, body); err != ErrTimestampTooOld {
		t.Fatalf("want ErrTimestampTooOld, got %v", err)
	}
}

func TestVerifier_RejectsInvalidTimestamp(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	v, _ := NewVerifier(base64.StdEncoding.EncodeToString(pub), time.Minute)

	body := []byte(`{"data":{}}`)
	if err := v.Verify("not-a-number", "", body); err != ErrInvalidTimestamp {
		t.Fatalf("want ErrInvalidTimestamp, got %v", err)
	}
	if err := v.Verify("", "", body); err != ErrInvalidTimestamp {
		t.Fatalf("want ErrInvalidTimestamp for empty ts, got %v", err)
	}
}

func TestVerifier_ZeroToleranceDisablesReplayWindow(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v, _ := NewVerifier(base64.StdEncoding.EncodeToString(pub), 0)

	body := []byte(`{"data":{}}`)
	old := time.Now().Add(-24 * time.Hour).Unix()
	msg := append([]byte(int64Str(old)), '|')
	msg = append(msg, body...)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))

	if err := v.Verify(int64Str(old), sig, body); err != nil {
		t.Fatalf("Verify with zero tolerance: %v", err)
	}
}

func TestVerifier_ControlledClock(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v, _ := NewVerifier(base64.StdEncoding.EncodeToString(pub), time.Minute)

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	v.now = func() time.Time { return now }

	body := []byte(`{"data":{}}`)
	msg := func(ts int64) []byte {
		m := append([]byte(int64Str(ts)), '|')
		return append(m, body...)
	}
	sig := func(ts int64) string {
		return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg(ts)))
	}

	if err := v.Verify(int64Str(now.Add(-30*time.Second).Unix()), sig(now.Add(-30*time.Second).Unix()), body); err != nil {
		t.Fatalf("in-window signature rejected: %v", err)
	}
	if err := v.Verify(int64Str(now.Add(-2*time.Minute).Unix()), sig(now.Add(-2*time.Minute).Unix()), body); err != ErrTimestampTooOld {
		t.Fatalf("want ErrTimestampTooOld, got %v", err)
	}
}

func TestVerifier_RejectsMalformedSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v, _ := NewVerifier(base64.StdEncoding.EncodeToString(pub), time.Minute)

	body := []byte(`{"data":{}}`)
	ts := time.Now().Unix()

	if err := v.Verify(int64Str(ts), "!!!not-base64!!!", body); err != ErrInvalidSignature {
		t.Fatalf("want ErrInvalidSignature for non-base64, got %v", err)
	}
	if err := v.Verify(int64Str(ts), base64.StdEncoding.EncodeToString([]byte("too-short")), body); err != ErrInvalidSignature {
		t.Fatalf("want ErrInvalidSignature for short sig, got %v", err)
	}

	msg := append([]byte(int64Str(ts)), '|')
	msg = append(msg, body...)
	rawSig := base64.RawStdEncoding.EncodeToString(ed25519.Sign(priv, msg))
	if err := v.Verify(int64Str(ts), rawSig, body); err != nil {
		t.Fatalf("Verify with raw base64 signature: %v", err)
	}
}

func TestVerifier_BadPublicKey(t *testing.T) {
	if _, err := NewVerifier("not-base64-!!!", time.Minute); err == nil {
		t.Fatal("want error for invalid public key")
	}
	if _, err := NewVerifier(base64.StdEncoding.EncodeToString([]byte("short")), time.Minute); err == nil {
		t.Fatal("want error for short public key")
	}
	if _, err := NewVerifier("", time.Minute); err == nil {
		t.Fatal("want error for empty public key")
	}
}

func TestParse_SMS(t *testing.T) {
	body := `{"data":{"record_type":"event","event_type":"message.received","id":"evt-1","occurred_at":"2026-08-01T12:00:00Z","payload":{"id":"msg-1","from":{"phone_number":"+15551234567"},"to":[{"phone_number":"+15559876543"}],"text":"hello"}}}`
	w, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if w.EventID != "evt-1" || w.EventType != "message.received" {
		t.Fatalf("bad event meta: %+v", w)
	}
	if w.SMS == nil {
		t.Fatal("SMS nil")
	}
	if w.SMS.From != "+15551234567" || w.SMS.To != "+15559876543" || w.SMS.Text != "hello" {
		t.Fatalf("bad SMS: %+v", w.SMS)
	}
}

func TestParse_NonMessageEventYieldsNoSMS(t *testing.T) {
	// ADR-0006 scopes the gateway to message.received. Any other event type
	// parses but returns a nil SMS so the handler can ack-and-ignore.
	body := `{"data":{"record_type":"event","event_type":"message.sent","id":"evt-x"}}`
	w, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if w.EventType != "message.sent" || w.SMS != nil {
		t.Fatalf("expected null SMS for message.sent: %+v", w)
	}
}

// TestParse_VoiceEventYieldsNoSMS confirms voice events do not enter the
// SMS path; voice is deferred under ADR-0006 §5. Parse does not decode the
// voice payload — the SMS handler drops it.
func TestParse_VoiceEventYieldsNoSMS(t *testing.T) {
	body := `{"data":{"record_type":"event","event_type":"call.initiated","id":"evt-2","payload":{"call_control_id":"cc-1","from":"+15551234567"}}}`
	w, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if w.SMS != nil {
		t.Fatalf("SMS should be nil for voice events, got %+v", w.SMS)
	}
}

func TestParse_MissingEventType(t *testing.T) {
	if _, err := Parse([]byte(`{"data":{}}`)); err == nil {
		t.Fatal("want error for missing event_type")
	}
}

func TestParse_BadPayloads(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bad sms payload", `{"data":{"event_type":"message.received","payload":"not-an-object"}}`},
		{"invalid json", `{`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.body)); err == nil {
				t.Fatal("Parse succeeded, want error")
			}
		})
	}
}

func TestParse_InvalidOccurredAtIgnored(t *testing.T) {
	w, err := Parse([]byte(`{"data":{"record_type":"event","event_type":"message.sent","id":"evt-x","occurred_at":"not-a-time"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !w.OccurredAt.IsZero() {
		t.Fatalf("OccurredAt = %v, want zero for unparseable value", w.OccurredAt)
	}
}

func TestDedup(t *testing.T) {
	now := time.Now()
	d := NewDedup(time.Minute)
	d.now = func() time.Time { return now }

	if d.Seen("a") {
		t.Fatal("first sighting should not be duplicate")
	}
	if !d.Seen("a") {
		t.Fatal("second sighting should be duplicate")
	}
	if d.Seen("b") {
		t.Fatal("new id should not be duplicate")
	}

	now = now.Add(2 * time.Minute)
	if d.Seen("a") {
		t.Fatal("expired id should not be duplicate")
	}
}

func int64Str(v int64) string {
	return strconv.FormatInt(v, 10)
}
