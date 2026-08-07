# Glossary

Terms are added as the design is refined. Each entry links decisions where defined.

## A

- **Action** — Retired. Under the async contract, a unit of work Hermes sent to the gateway via `POST /actions` to be executed against Telnyx. The `/actions` endpoint and its auth are deleted; the gateway executes no side-effects. The verbs did not survive as MCP tools — there is no Telnyx MCP server (ADR-0006). See [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md).

## C

- **Call Control** — Telnyx's voice API: webhook-driven (`answer`/`speak`/`gather`/`transfer`/`hangup`), where the app drives call flow by issuing commands and receiving event webhooks. Voice is deferred (see [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md)).
- **Confirmation** — Conversational, Hermes-owned prompt for irreversible/costly actions performed by Hermes's *own* tools ("Reply YES to send."). Accident-prevention control, not a security boundary. Enabled deterministically by the gateway's rolling-window continuity, so "yeah, looks good" is resolved against the prior draft. See [ADR-0002](adr/0002-conversational-confirmation-no-pin.md).

## E

- **Event** — A normalized inbound occurrence derived from a Telnyx webhook (inbound SMS, or voice state change). Under the turn model the SMS Event becomes the turn's user message (gateway→Hermes via `/v1/chat/completions`), no longer POSTed as a standalone contract object. See [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md).

## G

- **Gateway (Go Gateway Service)** — Transport and turn-orchestration plumbing: receives/validates Telnyx webhooks, allowlists and dedups senders, maintains the per-sender rolling window, submits SMS turns to hermes's OpenAI-compatible API, and auto-sends the turn's final text as the reply. A pure relay: no tool surface, no outbound capability beyond replying to a verified sender. Owns timing and delivery, not response generation. See [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md).

## H

- **Hermes** — hermes-agent (NousResearch) running in the homelab: the AI/orchestrator (agent loop) that owns all response generation, tool orchestration, and long-term memory. Reached via its OpenAI-compatible API (`hermes-api-svc:8642`, bearer `API_SERVER_KEY`). See [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md).
- **Hold line (gap-filling)** — Voice pacing via pre-announcement; a voice-path mechanism, deferred with the voice work. See [ADR-0003](adr/0003-voice-canned-fallback.md), [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md).

## I

- **Idempotency-Key** — Per-turn request key the gateway stamps on every `/v1/chat/completions` turn, set to the inbound Telnyx event id. hermes caches responses by key (5 min), so retries replay the cached response instead of re-running the agent loop (which would re-execute Hermes-internal tools, e.g. re-sending an email). See [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md).

## P

- **PIN gate** — Deprecated for MVP. A gateway-validated secret was analyzed as mechanically identical to YES/NO confirmation and unable to resist the device-holder adversary over SMS. Revisit trigger: lost/stolen-phone use case. See [ADR-0002](adr/0002-conversational-confirmation-no-pin.md).
- **Projected ServiceAccount token** — Retired. The short-lived kubelet-rotated bearer token Hermes presented to the gateway's `/actions` endpoint; the endpoint and its OIDC/JWKS machinery are deleted with [ADR-0001](adr/0001-serviceaccount-auth-for-actions.md), superseded by [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md).

## R

- **Rolling window** — The gateway's bounded, in-memory, per-sender transcript of recent turns, replayed in the `messages` array of each chat/completions request to give Hermes short-term conversational continuity. Contract: last-exchange minimum, small K≈2–3 default, pruned to user+assistant turns, count-only (no time-trim). Lost on restart (accepted); long-term memory lives in Hermes. See [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md).

## S

- **Session** — A conversation unit. On the SMS path: one thread per originating sender; Hermes owns long-term memory (global, cross-session on the deployed build), the gateway owns the bounded rolling window. No gateway session store or TTL routing cache on the SMS path. See [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md).

## T

- **Telnyx** — The telephony provider: Messaging API (SMS) and Call Control API (voice), auth via static API key/bearer. Webhooks signed with Ed25519.
- **Telnyx MCP server** — Considered and rejected (ADR-0006): a gateway-hosted MCP server for third-party sends. Not built — the MVP has no third-party sends; revisit only if the owner later wants "text my family."
- **Transcription event** — A `call.transcription` webhook carrying interim/final transcript text; the voice input path. Deferred with the voice work. See [ADR-0005](adr/0005-voice-path-managed-stt.md).
- **Turn** — The fundamental unit of gateway↔Hermes interaction: one synchronous, non-streaming `/v1/chat/completions` request per inbound SMS (rolling window + new message), ending when the final assistant text is returned and auto-sent to the caller as the reply. See [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md).

## W

- **Workload identity** — Retired. Authentication of a workload using its Kubernetes ServiceAccount; used for the deleted `/actions` endpoint. Superseded by a static shared secret (`API_SERVER_KEY`) under [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md).
