package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/vranyes/telnyx-hermes-bridge/internal/hermes"
	"github.com/vranyes/telnyx-hermes-bridge/internal/metrics"
	"github.com/vranyes/telnyx-hermes-bridge/internal/phone"
	"github.com/vranyes/telnyx-hermes-bridge/internal/reqctx"
	"github.com/vranyes/telnyx-hermes-bridge/internal/telnyx"
	"github.com/vranyes/telnyx-hermes-bridge/internal/transcript"
)

// Hard-coded reply-send retry schedule (ADR-0006 §2: ~1/2/4/8s with ~15s
// budget). The reply is the turn's deliverable, so unlike the original
// ADR-0004 expensive-action rule, it is retried. A duplicate reply is cheap
// and near-idempotent.
var replySchedule = []time.Duration{
	time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
}

// replySendDeadline bounds one reply-send retry run: the retry budget (~15s,
// the nominal 1/2/4/8 schedule sum) plus the worst-case ±20% jitter over that
// schedule plus a send allowance. A deadline of only the nominal sum (or sum
// plus a token margin) expires mid-backoff — the jittered schedule alone can
// overshoot it — and the reply is dropped before its retries run.
func replySendDeadline(budget time.Duration) time.Duration {
	var jitterHeadroom time.Duration
	for _, d := range replySchedule {
		jitterHeadroom += time.Duration(float64(d) * 0.2)
	}
	return budget + jitterHeadroom + time.Second
}

// SMSHandler receives Telnyx message.received webhooks and turns each one
// into a synchronous Hermes chat/completions turn whose final assistant text
// is auto-sent back to the caller as an SMS (ADR-0006 §1).
//
// Flow: verify -> parse -> event-id dedup -> allowlist -> per-sender queue
// admit. When the sender's in-flight turn finishes and the queue admits
// this one, the handler builds messages (system + rolling window + new
// user), calls Hermes synchronously, and (per outcome) auto-sends the
// reply or the canned fallback. A 200 goes back to Telnyx regardless of
// turn outcome.
type SMSHandler struct {
	Verifier  *telnyx.Verifier
	Dedup     *telnyx.Dedup
	Allowlist AllowlistChecker
	Hermes    *hermes.Client
	Telnyx    *telnyx.Client
	Window    *transcript.Store
	Queue     *TurnQueue
	Metrics   *metrics.Metrics
	Logger    *slog.Logger
	Now       func() time.Time

	// Per-turn system prompt + canned text (ADR-0006 open item #3).
	SystemPrompt      string
	QueueOverflowText string
	SMSFallbackText   string

	// ReplyRetryBudget bounds the reply-SMS retry (ADR-0006 §2).
	ReplyRetryBudget time.Duration
}

// AllowlistChecker lets tests substitute a fake without dragging the authz
// package's transitive imports into the handler.
type AllowlistChecker interface {
	Allowed(number string) bool
}

// ServeHTTP runs the SMS-MVP pipeline for one webhook delivery.
func (h *SMSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Metrics.WebhooksReceived.Add(1)
	logger := h.Logger.With("request_id", reqctx.ID(r))

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if err := h.Verifier.Verify(
		r.Header.Get(telnyx.TimestampHeader),
		r.Header.Get(telnyx.SignatureHeader),
		body,
	); err != nil {
		h.Metrics.WebhooksRejected.Add(1)
		logger.Warn("sms webhook signature rejected", "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	wb, err := telnyx.Parse(body)
	if err != nil {
		h.Metrics.WebhooksRejected.Add(1)
		logger.Warn("sms webhook parse failed", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if wb.SMS == nil {
		// message.sent / message.finalized / voice events: ack and ignore.
		w.WriteHeader(http.StatusOK)
		return
	}

	if h.Dedup.Seen(wb.EventID) {
		h.Metrics.WebhooksDeduped.Add(1)
		logger.Debug("sms webhook duplicate ignored", "event_id", wb.EventID)
		w.WriteHeader(http.StatusOK)
		return
	}

	if !h.Allowlist.Allowed(wb.SMS.From) {
		h.Metrics.WebhooksRejected.Add(1)
		logger.Warn("sms sender not allowlisted", "from", wb.SMS.From)
		// Ack so Telnyx stops retrying; drop silently.
		w.WriteHeader(http.StatusOK)
		return
	}

	// Per-sender queue (ADR-0006 §1). Admit is non-blocking on the HTTP
	// handler thread (the webhook must ack Telnyx immediately): it checks
	// the per-sender cap and either reserves a slot (returns Ticket)
	// or signals overflow (returns nil so the canned "one at a time"
	// SMS fires instead of dispatching a turn).
	//
	// Keying the queue on phone.Normalize(from) — same as the Window
	// sender key (minus the "sms:" prefix) — so a sender whose number
	// arrives in two formats (e.g. +1 vs 1) does not get two queue
	// slots and a split turn order.
	senderKey := phone.Normalize(wb.SMS.From)
	ticket := h.Queue.Admit(senderKey)
	if ticket == nil {
		h.Metrics.TurnsQueueOverflow.Add(1)
		logger.Info("sms sender queue overflow; canned line", "from", wb.SMS.From, "event_id", wb.EventID)
		go h.sendReply(reqctx.WithID(context.Background(), reqctx.ID(r)), wb.EventID, wb.SMS.From, h.QueueOverflowText)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Run the turn on a goroutine so the webhook ack returns immediately
	// (ADR-0004 posture preserved). The goroutine blocks on Wait() until
	// it is this sender's turn, runs the turn, then Release()s.
	go func() {
		defer ticket.Release()
		// Detach the context so the turn survives the webhook handler
		// returning; carry the request id through for logs.
		turnCtx := reqctx.WithID(context.Background(), reqctx.ID(r))
		ticket.Wait()
		h.runTurn(turnCtx, wb, logger)
	}()

	w.WriteHeader(http.StatusOK)
}

// runTurn executes one synchronous chat/completions turn and acts on its
// outcome (ADR-0006 §1/§2).
func (h *SMSHandler) runTurn(ctx context.Context, wb *telnyx.Webhook, logger *slog.Logger) {
	from := wb.SMS.From
	userText := wb.SMS.Text
	sender := "sms:" + phone.Normalize(from)

	window := h.Window.Window(sender)
	msgs := make([]hermes.Message, 0, 2+len(window)+1)
	msgs = append(msgs, hermes.Message{Role: "system", Content: h.SystemPrompt})
	for _, t := range window {
		msgs = append(msgs, hermes.Message{Role: t.Role, Content: t.Content})
	}
	msgs = append(msgs, hermes.Message{Role: "user", Content: userText})

	result, err := h.Hermes.Turn(ctx, wb.EventID, from, hermes.Request{Messages: msgs})
	switch {
	case err == nil:
		// Success: append both turns then auto-send the reply with retry.
		h.Window.Append(sender, "user", userText)
		h.Window.Append(sender, "assistant", result.Reply)
		logger.Info("sms turn completed", "event_id", wb.EventID, "from", from)
		h.sendReply(ctx, wb.EventID, from, result.Reply)
	case errors.Is(err, hermes.ErrEmptyReply):
		// Soft-behavior miss: agent ran but produced no text. Per
		// ADR-0006 §1, send nothing and log; a canned fallback would
		// contradict a turn that actually succeeded. The user turn is
		// retained; the assistant turn is not (no reply to remember).
		h.Window.Append(sender, "user", userText)
		logger.Info("sms turn soft miss; no reply sent", "event_id", wb.EventID, "from", from)
	case errors.Is(err, hermes.ErrExhausted) || errors.Is(err, hermes.ErrPermanent):
		// Canned SMS fallback over the auto-send path. The user turn is
		// retained; the fallback text is NOT — replaying it as an
		// assistant turn would tell the agent the previous reply was
		// "something went wrong" (ADR-0006 §1 pruned-content rule).
		//
		// Single-owner: the handler is the only site that sends the
		// canned fallback (the hermes client used to also fire one
		// via a Fallback seam and that produced a duplicate send).
		h.Window.Append(sender, "user", userText)
		h.Metrics.TurnsFallbackSent.Add(1)
		logger.Warn("sms turn failed; canned fallback", "event_id", wb.EventID, "from", from, "err", err)
		h.sendReply(ctx, wb.EventID, from, h.SMSFallbackText)
	default:
		// Defensive: unknown error class behaves like permanent failure.
		h.Window.Append(sender, "user", userText)
		h.Metrics.TurnsFallbackSent.Add(1)
		logger.Error("sms turn unknown error; canned fallback", "event_id", wb.EventID, "from", from, "err", err)
		h.sendReply(ctx, wb.EventID, from, h.SMSFallbackText)
	}
}

// sendReply POSTs the reply SMS via Telnyx with bounded retry (ADR-0006 §2).
// On exhaustion the reply is dropped silently — a failure notification would
// fail on the same broken send path — and a Hermes-internal action executed
// earlier in the turn may go unreported (accepted under ADR-0004's loss-
// tolerant posture).
func (h *SMSHandler) sendReply(ctx context.Context, eventID, to, text string) {
	// The deadline must fit the full schedule under worst-case ±20% jitter
	// plus the send attempts; see replySendDeadline.
	ctx, cancel := context.WithTimeout(ctx, replySendDeadline(h.ReplyRetryBudget))
	defer cancel()
	started := time.Now()
	for attempt := 0; ; attempt++ {
		err := h.Telnyx.SendSMS(ctx, to, text)
		if err == nil {
			return
		}
		if attempt >= len(replySchedule) {
			h.logReplyDrop(ctx, "sms reply send exhausted; dropped silently", eventID, to, attempt+1, started, err)
			return
		}
		deadline, _ := ctx.Deadline()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			h.logReplyDrop(ctx, "sms reply send budget exhausted; dropped silently", eventID, to, attempt+1, started, err)
			return
		}
		// Jitter is applied before the remaining-budget cap: a jittered
		// delay capped first would overshoot the deadline, wake the select
		// below on ctx.Done(), and drop the reply mid-backoff without ever
		// making the final attempt.
		delay := replySchedule[attempt]
		delay = time.Duration(float64(delay) * (0.8 + 0.4*rand.Float64()))
		if delay = min(delay, remaining); delay <= 0 {
			h.Metrics.ReplySendFailed.Add(1)
			return
		}
		h.Metrics.ReplySendRetries.Add(1)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			h.logReplyDrop(ctx, "sms reply send canceled; dropped silently", eventID, to, attempt+1, started, ctx.Err())
			return
		}
	}
}

// logReplyDrop records a dropped reply-SMS together with the context needed
// to correlate it back to the webhook/turn that produced it: the request id
// (carried on the detached ctx), the Telnyx event id (also the Hermes
// Idempotency-Key), how many send attempts ran, and how long the retry ran
// relative to the configured budget.
func (h *SMSHandler) logReplyDrop(ctx context.Context, msg, eventID, to string, attempts int, started time.Time, err error) {
	h.Metrics.ReplySendFailed.Add(1)
	h.Logger.Warn(msg,
		"request_id", reqctx.FromContext(ctx),
		"event_id", eventID,
		"to", to,
		"attempts", attempts,
		"elapsed", time.Since(started),
		"budget", h.ReplyRetryBudget,
		"err", err,
	)
}

// TurnQueue gates one in-flight turn per sender; new inbound SMS whose
// sender already has a turn in flight are queued (in-memory, bounded
// per-sender). Beyond the cap Admit returns nil so the SMS handler
// sends the canned "one at a time" line (ADR-0006 §1).
//
// Two-phase contract:
//   - Admit is non-blocking: returns a non-nil *Ticket if the slot fits
//     (caller may then Wait until the slot's their turn), or nil on
//     overflow. This is the only call the HTTP handler thread makes; it
//     must not block.
//   - Wait blocks until the sender's turn.
//   - Release releases the slot and hands the active turn to the next
//     waiter in FIFO order.
//
// State model per sender:
//
//	running[sender] bool      — a turn is currently active
//	queued[sender]  []*Ticket — FIFO queue of waiting tickets
//
// The active turn is whoever most recently advanced past Wait(): the
// first Admit (running already true, ready already closed) or the
// longest-waiting ticket whose ready channel was closed by the prior
// turn's Release. Total slots = (running?1:0) + len(queued); an Admit
// that finds total >= cap+1 overflows.
//
// Each waiter Ticket has a `ready` channel that Release closes to hand
// off the active turn. This avoids the cond-L re-acquire ordering
// subtleties of sync.Cond while preserving strict FIFO order (only the
// head of queued is signalled).
type TurnQueue struct {
	mu      sync.Mutex
	cap     int
	running map[string]bool
	queued  map[string][]*Ticket
}

// Ticket is a held queue slot acquired via Admit. Wait blocks until the
// sender's turn; Release releases the slot.
type Ticket struct {
	queue  *TurnQueue
	sender string
	ready  chan struct{}
}

// Wait blocks until the sender's turn arrives. The first admitted
// ticket for the sender acquires immediately (its ready channel is
// already closed at Admit time); later tickets block until released.
func (t *Ticket) Wait() {
	if t == nil {
		return
	}
	<-t.ready
}

// Release frees the ticket's slot. If waiters remain, hands the active
// turn to the longest-waiting one by closing its ready channel.
//
// Each Ticket is single-use: Release may be called exactly once. The
// SMS webhook handler always calls Release via defer, so this is never
// a problem in practice.
func (t *Ticket) Release() {
	if t == nil || t.queue == nil {
		return
	}
	q := t.queue
	q.mu.Lock()
	queued := q.queued[t.sender]
	if len(queued) == 0 {
		// No waiters behind us; fully drain per-sender state.
		delete(q.running, t.sender)
		delete(q.queued, t.sender)
		q.mu.Unlock()
		return
	}
	// Promote the head of the queue: it becomes the active turn.
	next := queued[0]
	q.queued[t.sender] = append([]*Ticket(nil), queued[1:]...)
	if len(q.queued[t.sender]) == 0 {
		delete(q.queued, t.sender)
	}
	q.mu.Unlock()
	close(next.ready)
}

// NewTurnQueue builds a per-sender gate. cap is the max number of queued
// turns behind the one active turn (per ADR-0006 §1, ≈4).
func NewTurnQueue(cap int) *TurnQueue {
	if cap < 1 {
		cap = 1
	}
	return &TurnQueue{
		cap:     cap,
		running: make(map[string]bool),
		queued:  make(map[string][]*Ticket),
	}
}

// Admit returns a Ticket (non-nil) if the sender has capacity, or nil
// if the per-sender queue is full. Non-blocking; safe to call from the
// HTTP handler thread.
//
// The first admit for a sender marks it running and returns a ticket
// whose Wait returns immediately (ready pre-closed). Subsequent admits
// while one is running become waiters that block in Wait until released.
func (q *TurnQueue) Admit(sender string) *Ticket {
	q.mu.Lock()
	defer q.mu.Unlock()
	running := q.running[sender]
	total := len(q.queued[sender])
	if running {
		total++
	}
	if total >= q.cap+1 {
		return nil
	}
	if !running {
		// First admit: this ticket becomes the active turn.
		q.running[sender] = true
		t := &Ticket{queue: q, sender: sender, ready: make(chan struct{})}
		close(t.ready)
		return t
	}
	// Subsequent admit while one is running: become a waiter.
	t := &Ticket{queue: q, sender: sender, ready: make(chan struct{})}
	q.queued[sender] = append(q.queued[sender], t)
	return t
}
