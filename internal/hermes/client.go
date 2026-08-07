// Package hermes implements the gateway -> Hermes leg under ADR-0006: one
// synchronous, non-streaming POST /v1/chat/completions per inbound SMS, with
// Idempotency-Key set to the inbound Telnyx event id and bounded in-memory
// retry (ADR-0006 §2, amending ADR-0004's backoff schedule).
//
// The client parses the OpenAI-compatible chat completion response and returns
// the final assistant text. An empty final text is a soft-behavior miss: the
// agent loop ran but produced no reply (e.g. it did everything in its own
// tools), and the caller (the SMS handler) treats that as "send nothing".
//
// Retry policy (ADR-0006 §2):
//   - connection errors, timeouts, 429 (hermes max_concurrent_runs cap),
//     and 5xx are retried with the bounded jittered backoff from ADR-0004.
//   - A Retry-After header is honored up to the remaining budget; if it
//     exceeds the budget, the fallback fires.
//   - other 4xx is permanent.
//   - the budget stays under hermes's 5-minute Idempotency-Key cache window so
//     a retried request replays the cached response rather than re-running
//     the agent loop (which would re-execute Hermes-internal tools).
//
// TODO(adr-0006 open item #1): verify hermes's Idempotency-Key cache on the
// deployed build (v2026.7.30). The retry policy below relies on it; without
// it, retry is limited to connection-level failures.
package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vranyes/telnyx-hermes-bridge/internal/metrics"
	"github.com/vranyes/telnyx-hermes-bridge/internal/reqctx"
)

var (
	// ErrPermanent is wrapped around non-retryable (4xx) responses.
	ErrPermanent = errors.New("hermes: permanent failure")
	// ErrExhausted is returned by Turn when the retry budget is spent.
	ErrExhausted = errors.New("hermes: retry budget exhausted")
	// ErrEmptyReply is returned by Turn when the assistant's final text is
	// empty (the soft-behavior miss case). Not a fallback trigger.
	ErrEmptyReply = errors.New("hermes: empty assistant reply")
)

// retryableError marks errors eligible for retry.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return "hermes: retryable: " + e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// Message is one entry of the OpenAI chat/completions messages array. The
// agent loop is stateless across requests — the gateway must carry the full
// conversation history in messages (ADR-0006 §1).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is the chat/completions request body the gateway POSTs.
type Request struct {
	Messages []Message `json:"messages"`
	// Model is intentionally omitted: hermes's config.yaml wires the model
	// (gemini-3-flash-preview). Forcing a model here would override the
	// operator's choice. (ADR-0006 §1.)
	Stream bool `json:"stream,omitempty"`
}

type chatChoice struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
}

// TurnResult holds the outcome of a synchronous chat/completions call.
type TurnResult struct {
	// Reply is the final assistant text. Empty if Hermes ran tools but
	// produced no text (the soft-miss case).
	Reply string
	// Replayed is true if the response came back cached by Idempotency-Key
	// (observable via the open item #1 verification — the deployed hermes
	// build does not currently advertise this, so the gateway infers it
	// from a stable completion id or similar; left as a stub the caller
	// can later wire).
	Replayed bool
}

// Client POSTs synchronous chat/completions turns to Hermes and applies the
// bounded backoff/retry policy from ADR-0006 §2 / ADR-0004: attempts at
// ~1s/2s/4s/8s/15s/30s with jitter within the 60s budget; connection errors,
// timeouts, 429, 408, and 5xx are retried; 4xx is permanent; Retry-After is
// honored up to the remaining budget.
//
// The client is retry-only: it does NOT send the canned SMS fallback when
// the budget runs out. That lives in the SMS handler
// (internal/handlers/sms_webhook.go runTurn), which knows the user-facing
// reply text and is the single owner of the SMS reply-send path. Keeping
// the fallback seam here caused a duplicate send once the handler also
// took responsibility for it; ADR-0006 §1 says empty reply = send nothing,
// and the canned fallback only fires from the handler's per-outcome
// dispatch (single owner = single send).
type Client struct {
	baseURL  string
	path     string
	apiKey   string
	http     *http.Client
	schedule []time.Duration
	budget   time.Duration
	logger   *slog.Logger
	metrics  *metrics.Metrics
}

// NewClient builds a Hermes turn client. NewClient does not start any worker
// goroutines — the SMS handler dispatches turns synchronously per sender.
func NewClient(baseURL, path, apiKey string, budget time.Duration, httpc *http.Client, logger *slog.Logger, m *metrics.Metrics) *Client {
	if httpc == nil {
		httpc = &http.Client{Timeout: 5 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	if m == nil {
		m = &metrics.Metrics{}
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		path:     path,
		apiKey:   apiKey,
		http:     httpc,
		schedule: schedule(budget),
		budget:   budget,
		logger:   logger,
		metrics:  m,
	}
}

// Turn submits one synchronous chat/completions turn and returns the final
// assistant text. eventID is the inbound Telnyx event id used as the
// Idempotency-Key so a retried request replays a cached response (ADR-0006 §2).
//
//   - A successful, non-empty reply -> TurnResult{Reply: ...}.
//   - A successful but empty reply -> ErrEmptyReply (soft miss; the caller
//     sends nothing and logs it).
//   - A permanent (4xx) failure -> ErrPermanent.
//   - Retry budget exhaustion -> ErrExhausted. The canned SMS fallback
//     is NOT sent here; the caller (runTurn) owns the reply-send path
//     and dispatches the fallback as part of its per-outcome handling
//     (single-owner rule, see Client doc comment).
func (c *Client) Turn(ctx context.Context, eventID, from string, req Request) (TurnResult, error) {
	ctx, cancel := context.WithTimeout(ctx, c.budget+time.Second)
	defer cancel()
	started := time.Now()

	for attempt := 0; ; attempt++ {
		result, err := c.turnOnce(ctx, eventID, req)
		if err == nil {
			c.metrics.TurnsCompleted.Add(1)
			return result, nil
		}
		if !isRetryable(err) {
			if errors.Is(err, ErrEmptyReply) {
				c.metrics.TurnsReplyEmpty.Add(1)
			} else {
				c.metrics.TurnsPermanentFailed.Add(1)
			}
			return TurnResult{}, err
		}
		retryAfter := retryAfterDelay(err)
		if attempt >= len(c.schedule) {
			return c.exhausted(ctx, eventID, from, err)
		}
		elapsed := time.Since(started)
		if elapsed >= c.budget {
			return c.exhausted(ctx, eventID, from, err)
		}
		delay := c.schedule[attempt]
		if retryAfter > 0 {
			delay = retryAfter
		}
		if delay = min(delay, c.budget-elapsed); delay <= 0 {
			return c.exhausted(ctx, eventID, from, err)
		}
		c.logger.Debug("hermes: retrying turn",
			"event_id", eventID,
			"from", from,
			"request_id", reqctx.FromContext(ctx),
			"attempt", attempt+1,
			"delay", delay,
		)
		c.metrics.TurnsRetried.Add(1)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return c.exhausted(ctx, eventID, from, err)
		}
	}
}

func (c *Client) exhausted(ctx context.Context, eventID, from string, cause error) (TurnResult, error) {
	c.logger.Error("hermes: retry budget exhausted",
		"event_id", eventID,
		"from", from,
		"request_id", reqctx.FromContext(ctx),
		"err", cause,
	)
	return TurnResult{}, ErrExhausted
}

// turnOnce performs a single chat/completions POST and classifies the outcome.
func (c *Client) turnOnce(ctx context.Context, eventID string, req Request) (TurnResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return TurnResult{}, fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.path, bytes.NewReader(body))
	if err != nil {
		return TurnResult{}, fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	// Idempotency-Key: the inbound Telnyx event id. hermes caches responses
	// by key (5 min, documented) so a retried request returns the cached
	// response instead of re-running the agent loop — which would otherwise
	// re-execute Hermes-internal tools (ADR-0006 §2).
	httpReq.Header.Set("Idempotency-Key", eventID)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return TurnResult{}, &retryableError{err: err}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var chat chatResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&chat); err != nil {
			return TurnResult{}, &retryableError{err: fmt.Errorf("decode response: %w", err)}
		}
		c.metrics.TurnsStarted.Add(1)
		if len(chat.Choices) == 0 {
			return TurnResult{}, ErrEmptyReply
		}
		reply := strings.TrimSpace(chat.Choices[0].Message.Content)
		if reply == "" {
			return TurnResult{}, ErrEmptyReply
		}
		return TurnResult{Reply: reply}, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout:
		ra := parseRetryAfter(resp.Header.Get("Retry-After"))
		io.Copy(io.Discard, resp.Body)
		return TurnResult{}, &retryableError{err: &retryAfterErr{status: resp.StatusCode, delay: ra}}
	case resp.StatusCode >= 500:
		io.Copy(io.Discard, resp.Body)
		return TurnResult{}, &retryableError{err: fmt.Errorf("status %d", resp.StatusCode)}
	default:
		io.Copy(io.Discard, resp.Body)
		return TurnResult{}, fmt.Errorf("%w: status %d", ErrPermanent, resp.StatusCode)
	}
}

// retryAfterErr carries a Retry-After delay so the retry loop can honor it
// instead of the jittered backoff schedule (ADR-0006 §2).
type retryAfterErr struct {
	status int
	delay  time.Duration
}

func (e *retryAfterErr) Error() string {
	return fmt.Sprintf("status %d (retry-after %s)", e.status, e.delay)
}

func retryAfterDelay(err error) time.Duration {
	var ra *retryAfterErr
	if errors.As(err, &ra) {
		return ra.delay
	}
	return 0
}

func parseRetryAfter(s string) time.Duration {
	if s == "" {
		return 0
	}
	if secs, err := strconv.Atoi(s); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	// HTTP-date form is rare for hermes; ignore gracefully.
	return 0
}

func isRetryable(err error) bool {
	var r *retryableError
	return errors.As(err, &r)
}

// schedule returns a jittered backoff schedule whose steps target the retry
// budget. The base ~1s/2s/4s/8s/15s/30s targets the 60s budget.
func schedule(budget time.Duration) []time.Duration {
	base := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 30 * time.Second}
	scale := float64(budget) / float64(60*time.Second)
	if scale <= 0 {
		scale = 1
	}
	out := make([]time.Duration, 0, len(base))
	for _, b := range base {
		d := time.Duration(float64(b) * scale)
		d = time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
		out = append(out, d)
	}
	return out
}
