# ADR-0004 — MVP reliability posture: best-effort, loss-tolerant, no durable state (no DB, no broker)

- Status: Accepted. Amended 2026-08-05 by [ADR-0006](0006-hermes-openai-api-and-telnyx-mcp.md) §2: the Gateway → Hermes retry leg is reshaped to idempotency-keyed synchronous turns (60s budget retained), and the SMS-path session cache is replaced by a rolling-window transcript store. Retry budget, dedup window, and in-memory posture unchanged.
- Date: 2026-08-03
- Deciders: gateway architect (grilling session)
- Resolves: open question 1 (Postgres vs SQLite) from `hermes-handoff.md` §6 — the answer is "neither, for MVP."

## Context

The handoff doc proposed a session store with a Postgres or SQLite implementation, plus dedup/idempotency machinery ("at-least-once", "no critical state held only in memory", "idempotent processing").

A requirements-first review (Round 3) asked what the system must actually guarantee under failure before choosing a tool. The declared product posture:

- Message loss is acceptable when infrastructure is unavailable beyond Telnyx's ~5-minute webhook redelivery window.
- Duplicate processing (inbound and outbound) is acceptable.
- The gateway is a fire-and-forget transporter: "Hermes passes the message to the Telnyx gateway and that's that."

## Decision

- **MVP delivery semantics are best-effort across every hop**, with bounded, in-memory retry on the Gateway → Hermes leg:
  - Telnyx → gateway: rely on Telnyx's own redelivery; the gateway applies a small non-durable in-memory dedup window of recent event IDs (lost on restart — accepted).
  - Gateway → Hermes: **bounded in-memory retry with exponential backoff**. Attempt at t=0, then retries after ~1s/2s/4s/8s/15s/30s (capped, with jitter), ~60s total budget. Only non-202 responses, timeouts, and network errors are retried; a 4xx is treated as permanent. The webhook handler returns `200` to Telnyx immediately and retries run as a non-blocking background task with bounded concurrency. Retry state is in-memory only — a pod restart during retry drops the message (accepted per posture). On exhaustion, the fallback path fires: "failed to contact Hermes" text for SMS; canned hold line + hard-deadline hangup for voice (ADR-0003).
  - Hermes → gateway: no action dedup; duplicate actions may execute (accepted).
  - Gateway → Telnyx: single command, no retry; lost or duplicate commands accepted.
- **No database and no message broker in the MVP.** Durability is the only thing either provides, and the requirements explicitly disclaim it.
- **Session state is in-memory** (TTL cache). Hermes owns real conversation memory; the gateway's session table is a routing/state cache.
- **Voice call state is in-memory per call**, carried across webhooks via Telnyx `client_state` (echoed by Telnyx on every event). MVP deploys a **single gateway replica**.
- `internal/session` interface remains; the `postgres.go`/`sqlite.go` implementations are replaced by an in-memory implementation for the MVP.

## Decision rationale

Requirements determine the tool:

- A database serves durable *retained state* (sessions that must survive restarts, audit, config).
- A message broker serves reliable *message flow* (feeding Hermes through its own restarts, backpressure on bursts, decoupling).
- Under loss-tolerant semantics the system's durable needs are near-zero, so neither fires. If durability is ever required, a broker is the better default for this system than a database — the architecture is a pipeline (webhook → event → action), not a records store.

Removing the durable machinery removes: DB dependency and migrations, queueing infrastructure, retry timers, idempotency keys, and the action journal.

## Consequences

- Any single transient failure can drop a user message — not only sustained (>5 min) outages. Accepted under the chosen posture. The bounded Gateway → Hermes retry narrows this window to ~60s of transient Hermes failures before the fallback fires.
- The retry budget **defines the latency envelope**: the SMS fallback deadline ≈ 60s; the voice hard deadline (ADR-0003) aligns to the same budget, with the canned line filling the window. (Handoff open question 2 resolved by the retry budget.)
- A gateway restart drops any in-flight voice call and any in-flight retry (caller hangs up / message dropped). Accepted for MVP.
- Scaling the gateway beyond one replica is blocked until call/session state becomes sticky-routed or durable. Deferred.
- The real controls for the lost/stolen-phone threat (ADR-0002 open item) are unaffected: allowlist revocation / line suspension.

## Open items

- Confirm the sharpened interpretation: best-effort means a single transient failure drops the message, not only sustained outages, except where the bounded Gateway → Hermes retry absorbs it. (Stated as accepted pending explicit confirmation.)
- Backoff micro-parameters (exact N, caps, jitter) — implementation detail, tuned at build time.
- No retry on other hops (Hermes → gateway, gateway → Telnyx) — confirm this remains fire-and-forget.
- Hermes-side handling of duplicate events — Hermes's spec, not the gateway's.
