// Package telnyx implements Telnyx webhook verification, webhook payload
// normalization, and the outbound API client.
//
// Webhook signing: Telnyx signs the raw request body with Ed25519 over
// "{timestamp}|{body}" (timestamp is the Unix-seconds value from the
// Telnyx-Timestamp header). The base64 signature arrives in
// Telnyx-Signature-Ed25519. The public key is base64-encoded and comes from
// the Telnyx portal.
package telnyx

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Webhook signature headers set by Telnyx on every delivery.
const (
	SignatureHeader = "Telnyx-Signature-Ed25519"
	TimestampHeader = "Telnyx-Timestamp"
)

var (
	// ErrInvalidTimestamp is returned when the timestamp header is absent or
	// malformed.
	ErrInvalidTimestamp = errors.New("telnyx: invalid webhook timestamp")
	// ErrTimestampTooOld is returned when the timestamp falls outside the
	// configured replay window.
	ErrTimestampTooOld = errors.New("telnyx: webhook timestamp outside tolerance window")
	// ErrInvalidSignature is returned when the Ed25519 signature does not
	// verify.
	ErrInvalidSignature = errors.New("telnyx: invalid webhook signature")
	// ErrInvalidPublicKey is returned when the configured public key is not a
	// valid base64 Ed25519 key.
	ErrInvalidPublicKey = errors.New("telnyx: invalid Ed25519 public key")
)

// Verifier validates the authenticity and freshness of Telnyx webhooks.
type Verifier struct {
	pub       ed25519.PublicKey
	tolerance time.Duration
	now       func() time.Time
}

// NewVerifier builds a Verifier from a base64-encoded Ed25519 public key.
func NewVerifier(pubKeyBase64 string, tolerance time.Duration) (*Verifier, error) {
	if pubKeyBase64 == "" {
		return nil, ErrInvalidPublicKey
	}
	raw, err := base64.StdEncoding.DecodeString(pubKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPublicKey, err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", ErrInvalidPublicKey, len(raw), ed25519.PublicKeySize)
	}
	return &Verifier{
		pub:       ed25519.PublicKey(raw),
		tolerance: tolerance,
		now:       time.Now,
	}, nil
}

// Verify checks the Ed25519 signature over "{timestamp}|{body}" and rejects
// timestamps outside the tolerance window (replay protection).
func (v *Verifier) Verify(timestamp, sigB64 string, body []byte) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrInvalidTimestamp
	}
	delta := v.now().Sub(time.Unix(ts, 0))
	if delta < 0 {
		delta = -delta
	}
	if v.tolerance > 0 && delta > v.tolerance {
		return ErrTimestampTooOld
	}

	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		sig, err = base64.RawStdEncoding.DecodeString(sigB64)
		if err != nil {
			return ErrInvalidSignature
		}
	}
	if len(sig) != ed25519.SignatureSize {
		return ErrInvalidSignature
	}

	msg := make([]byte, 0, len(timestamp)+1+len(body))
	msg = append(msg, timestamp...)
	msg = append(msg, '|')
	msg = append(msg, body...)
	if !ed25519.Verify(v.pub, msg, sig) {
		return ErrInvalidSignature
	}
	return nil
}

// SMSMessage is the normalized content of a message.received webhook.
type SMSMessage struct {
	MessageID string
	From      string
	To        string
	Text      string
}

// Webhook is a normalized inbound webhook. ADR-0006 narrows the SMS-MVP
// surface to message.received only; voice payload parsing is deferred with
// the voice work.
type Webhook struct {
	EventType  string
	EventID    string
	OccurredAt time.Time
	SMS        *SMSMessage
}

type envelope struct {
	Data struct {
		RecordType string          `json:"record_type"`
		EventType  string          `json:"event_type"`
		ID         string          `json:"id"`
		OccurredAt string          `json:"occurred_at"`
		Payload    json.RawMessage `json:"payload"`
	} `json:"data"`
}

type smsPayload struct {
	ID   string `json:"id"`
	From struct {
		PhoneNumber string `json:"phone_number"`
	} `json:"from"`
	To []struct {
		PhoneNumber string `json:"phone_number"`
	} `json:"to"`
	Text string `json:"text"`
}

// Parse decodes a verified webhook body into a normalized Webhook. ADR-0006
// scopes the SMS-MVP gateway to message.received; any other event type
// returns a Webhook whose SMS is nil so the SMS handler can ack-and-ignore.
func Parse(body []byte) (*Webhook, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("telnyx: decode webhook envelope: %w", err)
	}
	if env.Data.EventType == "" {
		return nil, fmt.Errorf("telnyx: webhook missing event_type")
	}

	w := &Webhook{
		EventType: env.Data.EventType,
		EventID:   env.Data.ID,
	}
	if env.Data.OccurredAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, env.Data.OccurredAt); err == nil {
			w.OccurredAt = t
		}
	}

	if env.Data.EventType == "message.received" {
		var p smsPayload
		if err := json.Unmarshal(env.Data.Payload, &p); err != nil {
			return nil, fmt.Errorf("telnyx: decode sms payload: %w", err)
		}
		w.SMS = &SMSMessage{
			MessageID: p.ID,
			From:      p.From.PhoneNumber,
			Text:      p.Text,
		}
		if len(p.To) > 0 {
			w.SMS.To = p.To[0].PhoneNumber
		}
	}
	return w, nil
}
