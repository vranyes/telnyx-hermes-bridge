# ADR-0005 — Voice path: managed in-call STT + `speak`; DTMF as skeleton; audio bridge deferred

- Status: Accepted. Deferred-in-effect 2026-08-05: the voice path is **not built in the SMS MVP** per [ADR-0006](0006-hermes-openai-api-and-telnyx-mcp.md) §6; this ADR remains the governing document for the voice phase (its references to POSTing Events and the `Action` path are superseded by ADR-0006's turn model and MCP seam).
- Date: 2026-08-03
- Deciders: gateway architect (grilling session)
- Resolves: open question 5 from `hermes-handoff.md` §6 (voice path).

## Context

The handoff proposed two voice options: Telnyx built-in `speak`/`gather` (DTMF-only, no natural speech) or a self-hosted audio bridge (faster-whisper STT + Piper TTS over Telnyx Media Streaming WebSocket), with the bridge deferred until the SMS + basic-voice path works.

Research during the session found that Telnyx now offers **managed in-call speech-to-text**: the `transcription_start` Call Control command, with multiple engines (Telnyx, Google, Deepgram, Azure, xAI, AssemblyAI, Speechmatics, Soniox), delivering `call.transcription` webhooks (interim/final results). This makes natural-speech voice possible with zero self-hosted ML.

## Decision

- **Voice MVP = managed in-call STT + `speak`:**
  - The gateway issues `transcription_start` on `call.answered`, receives `call.transcription` webhooks, normalizes final transcripts into Events, and POSTs them to Hermes.
  - Hermes responds via the normal `Action` path (`speak`, `gather`, `hangup`).
  - Voice transcript and SMS body both arrive as `Event.Content` — **no schema change to the async contract**.
- **Use `transcription_start`, not `gather_using_ai`.** `gather_using_ai` runs Telnyx's own AI to extract structured data and would compete with Hermes for the intelligence. Hermes must receive the raw open transcript.
- **DTMF is a stepping stone, not the product.** The skeleton may begin with `speak`/`gather` before transcription is wired, but DTMF-only is not the voice product.
- **Audio bridge (faster-whisper/Piper) deferred.** Revisit triggers: managed engines fail the latency/accuracy/cost bar for the intended experience, or an offline/privacy requirement emerges.
- **`command_id` is used when issuing Call Control commands** — Telnyx ignores duplicate `command_id`s per `call_control_id`, providing free dedup hardening for any future retry (per ADR-0004's posture no retry is needed in the MVP).

## Rationale

- Option B delivers the product vision (natural conversation on a dumb phone) at per-minute STT cost, consistent with ADR-0004's "no new homelab infra" spine.
- It reuses the existing webhook/command architecture — the gateway issues a command and receives webhooks; no WebSocket, no media streaming, no new deployment.
- Keeps Hermes as the sole intelligence; Telnyx provides only transport (STT/TTS), not understanding.

## Consequences

- `call.transcription` joins the voice webhook surface — the voice handler must parse transcription events.
- STT processing is Telnyx-side: transcripts transit Telnyx's chosen engine. This is a privacy boundary (audio already transits the carrier regardless); accepted, revisit if a stricter privacy requirement emerges.
- Per-minute STT cost — negligible at personal-call volume; verify at build time.
- Interim vs final transcript aggregation is a gateway (or Hermes) concern — only final results should reach Hermes to avoid noise.

## Open items

- Interim/final transcript aggregation policy (gateway-side filter vs Hermes-side).
- Language and STT engine selection — build-time, driven by accuracy/latency/cost measurements.
- Exact `transcription_start` parameters (engine, language, interim results) — build-time.
