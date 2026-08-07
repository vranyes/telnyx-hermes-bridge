# ADR-0003 — Voice pacing: pre-announcement by Hermes; silence-triggered backstop; 60s terminal

- Status: Accepted (revised 2026-08-03). Deferred-in-effect 2026-08-05: the voice path and this ADR's gateway-side machinery are **not built in the SMS MVP** per [ADR-0006](0006-hermes-openai-api-and-telnyx-mcp.md) §6; this ADR remains the governing document for the voice phase (its references to the Event contract's pacing hint and hold Actions are superseded by ADR-0006's retirement of that contract).
- Date: 2026-08-03
- Deciders: gateway architect (grilling session)
- Revision: pacing mechanism changed from "predict-and-interject hold lines" to **pre-announcement**; mid-window backstop resolved (include, silence-triggered).

## Context

The handoff doc (§3) justifies the async gateway↔Hermes pattern by noting that blocking is "bad for live voice (dead air)." But the specified fallback — "send a fallback message" on timeout — is SMS-only, and a Hermes timeout on a live call is still dead air.

Analysis of the tool-use loop: **the model is not consulted while a tool runs; it cannot self-interrupt.** Reliable pacing therefore cannot depend on Hermes *predicting* its own tool latency ("if this will exceed ~5s, interject") — LLM latency prediction is unreliable, and a misprediction yields exactly the dead air the async design exists to avoid.

A reactive "gateway asks Hermes to fill the gap" is also racy — a wedged Hermes will not answer a fill request, and a slow-but-responsive Hermes is about to deliver the real Action.

## Decision

- **Pacing mechanism = pre-announcement (Hermes-side).** Hermes streams a brief line *together with* each tool call, before the tool runs ("Let me check that for you"). No duration prediction is required — the line fires before every tool call, fast or slow; fast tools simply flow into the answer. Specified as a Hermes loop rule: **pace via pre-announcement, never via prediction.**
- **Gateway pacing context.** The Event POST includes a voice pacing hint (`channel: voice`; "speak a brief line before each tool call"), which Hermes implements. The gateway otherwise just executes whatever hold Actions Hermes emits.
- **Mid-window backstop: include, silence-triggered (~30s).** Narrow insurance against the residual cases — Hermes forgot to pre-announce (a soft behavior; models occasionally miss prompt instructions), or is wedged mid-loop. Fires only on genuine silence (the timer resets on any Hermes Action/utterance), so it cannot step on working pacing. One static line ("Still working on it…"), then keep waiting.
- **Terminal: 60s hard deadline** from the last gateway utterance. Static canned line, then hangup. **Hangup only — no SMS follow-up** (decided 2026-08-03). Late Action after the deadline: ignored — the call is terminated.
- The 60s window aligns with the Gateway → Hermes retry budget (ADR-0004).

## Rationale

- Pre-announcement eliminates the prediction-reliability failure by construction: the line is guaranteed to precede the silence.
- Self-interruption is not a primitive of the standard tool-use loop. The alternative (runtime-mediated polling — background slow tools and feed "still running" observations to the model) *can* let Hermes speak mid-wait, but it is runtime-driven, costs extra LLM turns, and adds complexity — deferred.
- The backstop remains because pre-announcement is a soft behavior, not a guarantee.
- Static gateway lines are fixed templates (like health-check text), not "response generation" — acceptable for the gateway to own as a last resort.

## Consequences

- The gateway owns exactly three voice-pacing things: pacing-hint injection (Event contract), one silence-triggered backstop line, one terminal line — plus executing Hermes's hold Actions.
- Worst-case caller silence: ~30s (backstop fires) with terminal at 60s if the backstop line is also followed by continued silence.
- A Hermes-side spec item is now explicit (pre-announcement pacing) — Hermes's loop implementation, out of gateway scope.
- The late-Action race is resolved: ignored after the call terminates.

## Open items

- Exact thresholds (pacing hint wording, backstop 30s, terminal 60s) and line contents — implementation detail.
- Runtime-mediated polling (mid-wait speech from Hermes) — deferred unless the pre-announcement pacing proves insufficient in practice.
