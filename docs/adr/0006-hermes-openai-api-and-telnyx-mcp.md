# ADR-0006 — Hermes integration: hermes-agent OpenAI-compatible API as an SMS relay; `/actions` contract retired; no MCP server, no third-party sends

- Status: Accepted (revised 2026-08-05 — scope narrowed to SMS; voice deferred to a follow-up ADR; MCP server removed — no third-party SMS)
- Date: 2026-08-05
- Deciders: gateway architect (grilling session)
- Supersedes: `hermes-handoff.md` §3–4 (the `/events` + `/actions` async contract) and [ADR-0001](0001-serviceaccount-auth-for-actions.md) (ServiceAccount-token auth for `/actions`).
- Amends: [ADR-0004](0004-mvp-reliability-posture.md) (retry leg reshaped to idempotency-keyed synchronous turns; session cache replaced by a rolling-window transcript store; reply-send retry added).

## Context

The handoff doc specified a bespoke async contract: the gateway POSTs normalized Events to a Hermes endpoint and receives Actions back on `POST /actions`. **That Hermes service does not exist.** The deployed system (gitops `apps/hermes/`) runs **NousResearch's `hermes-agent`** (`docker.io/nousresearch/hermes-agent:v2026.7.30` via `gateway run`), which exposes an **OpenAI-compatible API** on `hermes-api-svc:8642`, secured by a shared `API_SERVER_KEY` bearer secret, with a dashboard on `:9119` and a `config.yaml` wiring its tools (currently only the `osm-mcp-server` MCP server, Gemini provider, model `gemini-3-flash-preview`).

Research into hermes-agent's API server (`gateway/platforms/api_server.py`, public docs):

- It is a **server-side agent loop** (`AIAgent`/`run_agent`). The OpenAI surface is a front: the agent executes its *own* tools (MCP servers, plugins, skills, local terminal) and streams the final result back.
- `POST /v1/chat/completions` (OpenAI format, **stateless** — the full conversation is carried in the `messages` array), `POST /v1/responses` (stateful via `previous_response_id` or a named `conversation`), `POST /v1/runs` + `GET /v1/runs/{id}/events` (long-running runs with SSE), the Sessions API (`/api/sessions/{id}/chat` — "run one synchronous agent turn", `/chat/stream`), `GET /v1/models`, `GET /v1/capabilities`, `GET /health`.
- **`X-Hermes-Session-Id` is a transcript-scoped label, not a memory mechanism.** Per the docs it names the run/transcript for dashboard display and is echoed back; it "rotates on `/new`" and is explicitly distinct from `X-Hermes-Session-Key` (long-term memory scoping for the Honcho memory provider). Empirically (2026-08-05, deployed build), two different session ids share the same long-term memory — session ids do **not** create isolated memory namespaces. Chat/completions memory exists only in the `messages` array the client sends.
- **Idempotency-Key** is an allowed request header; hermes caches responses by key (documented as 5 minutes) so a retried request returns the cached response instead of re-running the agent loop.
- **MCP is hermes's supported extension seam** (request-scoped custom `tools` are not supported, open issue #57431). Hermes runs its *own* MCP servers (e.g., `osm-mcp-server`) from `config.yaml`. **The gateway hosts no MCP server and is not one of Hermes's tools.**

Therefore the `/actions` callback contract cannot be made to work against the deployed Hermes: hermes-agent neither emits our Events nor consumes our Actions.

**Scope refinement (this revision):** the MVP is **SMS-only** and **single-user** (one allowlisted owner) with **no gateway-executed side-effects and no third-party sends**. The gateway is a pure relay: inbound SMS → one chat/completions turn → the final assistant text is auto-sent back to the caller. The gateway hosts **no MCP server**; Hermes reaches only its own configured tools, and nothing can ask the gateway to spend money. Voice (streaming, STT input, TTS output, interruption, pacing, `/v1/runs`) is deferred to a follow-up ADR.

## Decision

### 1. Turn model

- **One inbound SMS = one synchronous, non-streaming `POST /v1/chat/completions`.** The gateway allowlists the sender, dedups on the Telnyx event id, and submits the turn. **At most one active turn per sender**; a new inbound SMS while a turn is in flight is queued (in-memory, bounded per-sender at ≈4) rather than run concurrently. Queue overflow sends a canned fallback ("I'll answer one at a time — hang on") so a rapid sender is not typing into the void.
- **The reply is the turn's final assistant text, auto-sent by the gateway** to the originating sender over the Telnyx Messages API. **All conversation output is channel delivery; there is no other delivery target.** The system-prompt rule is unconditional: "your final message is auto-delivered to the caller as an SMS," and it requires **always ending a turn with the reply text**. An **empty final text** (the model did everything in Hermes-internal tools and produced no text) is treated as a soft-behavior miss: the gateway sends nothing and logs it — no canned fallback, which could contradict a turn that actually succeeded. There are no `send_sms`/`outbound_sms` tools, no MCP boundary, no third-party recipient to confuse. Hermes's own tools (email, calendar, etc.) run inside its loop and are its own concern; irreversible actions among them are confirmed conversationally (ADR-0002).
- **Continuity is a gateway-held rolling window.** The gateway keeps a bounded, in-memory, per-sender window and sends it in the `messages` array each request (system + window + new user message). This makes deictic references deterministic — e.g., "draft the email to Bob… yeah, send it" resolves "it" against the assistant's draft replayed as a prior `assistant` message — **without depending on any unverified session-magic** (see §3). **Window contract (grilled 2026-08-05):** the *minimum is the last exchange* — replay exactly the previous assistant reply (the confirmation's antecedent) plus the new user message; additional retained turns are a pure quality knob, default small (K≈2–3). The window holds **pruned** content: user + assistant turns only (canned-fallback text and system turns excluded). **Count-only, no time-trim** — a stale window is superseded by Hermes's long-term memory, and time-trim adds a knob with no measured benefit.
- **Memory ownership split.** Hermes owns long-term understanding: its global, cross-session memory retains facts, preferences, and history (empirically confirmed: facts persist across session ids). The gateway owns the bounded short-term window needed for continuity. A gateway restart drops the window (continuity resets; long-term memory in Hermes survives) — accepted under ADR-0004's posture.

### 2. Turn reliability (amends ADR-0004)

- **Every turn carries `Idempotency-Key: <Telnyx event id>`.** hermes caches responses by key (5 min, documented), so a retried request returns the cached response instead of re-running the agent loop — which would otherwise re-execute Hermes-internal tools (e.g., re-sending an email).
- **Bounded retry within the 60s budget** (ADR-0004 schedule, ~1/2/4/8/15/30s with jitter) on connection errors, timeouts, `429` (hermes `max_concurrent_runs` cap), and 5xx — none of which imply the loop ran. A `Retry-After` header is honored up to the remaining budget; if it exceeds the budget, the fallback fires. Other 4xx is permanent. The budget stays under the 5-minute idempotency cache window.
- **Budget exhaustion → canned SMS fallback** ("Sorry, something went wrong…") sent over the same auto-send path. The 60s deadline (tunable from UX/metrics) is retained from ADR-0004.
- **Duplicate inbound delivery** is double-covered: the gateway's event-id dedup window (ADR-0004), and a duplicate that slips through carries the same `Idempotency-Key` → cached no-op.
- **The reply SMS is retried, not fire-and-forget.** The reply is the turn's deliverable — the only channel through which the user learns the outcome of a turn that also ran Hermes-internal tools — so ADR-0004's "single command, no retry" (written for the expensive-action leg) does not apply to it. A duplicate reply is cheap and near-idempotent, so the reply send is retried on Telnyx error with a bounded schedule (~1/2/4/8s, ~15s). If it still fails, the reply is **dropped silently** — a failure notification would fail on the same broken send — and a Hermes-internal action executed earlier in the turn may go unreported. Accepted under ADR-0004's loss-tolerant posture.

### 3. Session identity

- **`X-Hermes-Session-Id` is not used as a memory mechanism** (docs + empirical test) and is **not sent** (YAGNI, decided 2026-08-05); revisit only if dashboard grouping is ever wanted.
- The gateway is **stateless between turns** on the SMS path: no session store, no TTL routing cache. The only per-sender state is the rolling-window transcript store and the event-id dedup window (both in-memory, ADR-0004 posture). The in-memory session cache's jobs (routing `/actions` callbacks, voice call state) no longer exist.
- Session ids do **not** isolate memory on the deployed build; long-term memory is global. Accepted for the single-user MVP. **The allowlist is a configured set of numbers, defaulting to the one owner.** If a second number is ever added, that sender shares Hermes's global memory — acknowledged; per-sender isolation is a revisit trigger, not designed.

### 4. Auth (replaces ADR-0001)

- **One shared secret: `API_SERVER_KEY`** (bearer on `/v1/chat/completions`). Gateway→Hermes is the only leg between the two services.
- **There is no Hermes→gateway leg.** No MCP server, no `/actions`, no `MCP_TOKEN`, no OIDC/JWKS, no projected ServiceAccount tokens, no `cmd/dummy-oidc`. Nothing in the cluster can ask the gateway to place an outbound SMS or call; the gateway's Telnyx capability is limited to replying to the sender of a verified inbound message.
- **Security posture:** the gateway's public-internet surface is inbound-only (Telnyx webhooks, Ed25519-verified) and its outbound Telnyx calls are bounded to the reply path. The cluster-internal money-spending surface that motivated ADR-0001 and the MCP design **does not exist**, so the short-lived-token machinery is unnecessary. The cost: a static shared secret that needs rotation hygiene. **Rotation ops (decided 2026-08-05):** `API_SERVER_KEY` lives in gitops as a sealed secret; rotating it is a hermes-side change (new value + hermes restart) since the gateway is the only consumer — the same pattern as the existing `TELNYX_API_KEY`. Document, don't build.

### 5. Retired, retained, and deferred

- **Retired:** `internal/event` Event/Action contract; `internal/handlers/actions.go`; the Hermes Event-POST client's Event path; `internal/authz` OIDC/JWKS; `cmd/dummy-oidc`; the in-memory session cache (SMS path); **the Telnyx MCP server (never built) and the `send_sms`/`outbound_sms` tools**.
- **Retained unchanged:** webhook ingestion + Ed25519 verification, allowlist, dedup window, ADR-0004 in-memory posture, ADR-0002 conversational confirmation (now gating Hermes's own tools, enabled deterministically by the rolling window), canned SMS fallback.
- **Deferred (voice, follow-up ADR):** streaming, run-stop interruption, ADR-0003 pacing reinterpretation, STT input / TTS output of the turn's final text, `/v1/runs`. ADR-0003 and ADR-0005 remain the governing documents for the voice phase; their gateway-side machinery is not built in the SMS MVP. A Telnyx MCP server returns **only if** the owner later wants third-party sends ("text my family") — not before.

## Decision rationale

- **The handoff contract cannot run against the deployed system** — hermes-agent neither implements `/events` nor consumes `/actions`; keeping the contract means forking hermes-agent or building a second bespoke service, both against the goal of not implementing custom functionality.
- **The pure-relay design removes the entire side-effect surface.** Without MCP, the gateway cannot be asked to spend money or message a third party; the cluster-internal money-spending endpoint that motivated ADR-0001 is gone, and with it the OIDC/JWKS machinery and the `MCP_TOKEN` secret. Simplicity of the correct shape: the gateway is a dumb transporter, as the handoff originally intended.
- **Auto-delivery is safe because the caller is the only recipient.** No "Bob" exists to confuse; there is no third-party boundary to enforce, so no prompt-rule failure mode (ADR-0003's "prompt rules are soft" concern) applies to delivery targeting. The one rule — "your final message is delivered to the caller" — has no alternative that would be wrong.
- **SMS-first isolates the novel mechanics.** The SMS path exercises the OpenAI surface end-to-end (turns, idempotency, auto-delivery) while deferring the genuinely hard problems (streaming, interruption, STT/TTS) to a follow-up ADR.
- **The rolling window is the honest continuity mechanism.** Continuity for deictic confirmation cannot depend on the model spontaneously persisting drafts to global memory, nor on `X-Hermes-Session-Id` (verified: label, not memory; no namespace isolation). A bounded window of recent turns in `messages` is deterministic, uses chat/completions as designed, and costs nothing the gateway doesn't already have (it sees both sides of every SMS).
- **Idempotency-Key makes retry safe.** Without it, retrying a turn re-runs the loop and can duplicate a Hermes-internal side-effect (e.g., re-sending an email); with it, retries replay a cached response. The key is free — it's the Telnyx event id already used for inbound dedup.
- **Reply-send retry is cheap insurance on the product leg.** A duplicate reply is harmless; a lost reply is the whole turn failing. The expensive-operation no-retry rule never applied to the reply.

## Consequences

- The gateway gains: a **turn client** (chat/completions + idempotency + bounded retry) and a **rolling-window transcript store** (in-memory, per-sender, count-only). It hosts **no tool surface** and has **no outbound capability beyond replying to a verified sender**.
- The gateway loses: the Event/Action contract, `/actions`, OIDC/JWKS and SA-token machinery, `cmd/dummy-oidc`, the SMS-path session cache, and the Telnyx MCP server.
- One static shared secret (`API_SERVER_KEY`) replaces ADR-0001's short-lived rotated tokens — a new secret class requiring rotation hygiene.
- **Memory ownership is split**: Hermes owns long-term (global, cross-session) memory; the gateway owns a bounded short-term window. A gateway restart drops the window (continuity resets, long-term memory in Hermes survives).
- The 60s turn budget (tunable) sets the SMS fallback deadline, aligned with ADR-0004's latency envelope.
- **Feature ceiling:** no third-party SMS and no outbound calls in the MVP. Both are revisit-triggered, not designed.

## Open items

- **Verify hermes's Idempotency-Key cache on the deployed build** (`v2026.7.30`): confirm chat/completions responses are cached by key for 5 minutes before relying on it for retry safety. If not, retry is limited to connection-level failures and the duplicate-side-effect risk resurfaces.
- **`max_concurrent_runs` cap behavior** (default 10): confirm the 429 shape and the gateway's backoff path (Retry-After honored to budget).
- **System-prompt rules** — Hermes-side config, but the gateway's per-turn system message carries the channel context: the unconditional auto-delivery rule, the "always end your turn with the reply text" requirement (empty-reply handling), and the confirmation flow.
- **Voice follow-up ADR**: streaming, run-stop interruption, ADR-0003 pacing reinterpretation, STT input / TTS output, `/v1/runs` vs `/api/sessions/{id}/chat/stream`, and the sync-block-on-webhook machinery.
- **Revisit triggers (not designed):** third-party sends (Telnyx MCP server returns if the owner wants "text my family"); per-sender memory isolation if a second number is allowlisted.
