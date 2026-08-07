# Telnyx-Hermes Bridge

A pure SMS relay between **Telnyx** and **hermes** (the homelab AI agent):
one inbound SMS = one synchronous chat/completions turn against Hermes =
the final assistant text auto-sent back to the caller.

## What this site is

The design documentation and decision records (ADRs) for the bridge:

- [Glossary](glossary.md) — terms used across the docs and ADRs.
- [ADRs](adr/0001-serviceaccount-auth-for-actions.md) — architecture decision
  records covering authentication, the SMS-only MVP scope, reliability
  posture, the deferred voice path, and the Hermes OpenAI-API integration.
- [Privacy Policy](privacy.md) and [Terms & Conditions](terms.md) — the
  legal pages for the product.

## System at a glance (ADR-0006)

- The gateway is a **pure relay**: no `/actions` endpoint, no OIDC/JWKS
  machinery, no ServiceAccount token projection, no voice path, no Telnyx
  MCP server, and no outbound capability beyond replying to a verified
  sender.
- Inbound Telnyx webhooks are validated (Ed25519), allowlisted and deduped,
  then submitted as a **turn** to Hermes's OpenAI-compatible API
  (`/v1/chat/completions`, bearer `API_SERVER_KEY`).
- Per-sender **rolling windows** provide short-term conversational
  continuity; retries use a bounded jittered backoff; the reply SMS is
  auto-sent on success.
- The gateway owns timing and delivery — Hermes owns response generation
  and long-term memory.

See [ADR-0006](adr/0006-hermes-openai-api-and-telnyx-mcp.md) for the
contract, and [glossary.md](glossary.md) for the terminology.
