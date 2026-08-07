# ADR-0002 — Confirmation of irreversible actions is conversational and Hermes-owned; no PIN gate (MVP)

- Status: Accepted
- Date: 2026-08-03
- Deciders: gateway architect (grilling session)
- Supersedes: the "PIN gate" portion of `hermes-handoff.md` §4 (`internal/authz/pin.go`)
- Cross-reference (2026-08-05): unchanged by [ADR-0006](0006-hermes-openai-api-and-telnyx-mcp.md). The confirmation flow now runs over the turn model, made deterministic by the gateway's rolling-window transcript replay; the gated side-effects are now Hermes's *own* tools (email/calendar MCP), not gateway-executed actions — the gateway executes nothing.

## Context

The handoff doc proposed a PIN/confirmation gate in the gateway (`internal/authz/pin.go`) for irreversible actions (send email, place outbound call).

Analysis showed the proposal did not survive scrutiny:

1. **Mechanically, a gateway-validated PIN is identical to YES/NO confirmation** — both are "compare incoming string to expected value, flip a boolean." Security would come only from where the expected value lives and whether the adversary can obtain it.
2. **Over SMS, no token resists the device-holder adversary.** Whatever the confirmation value is, it must be delivered to and entered on the same phone the adversary controls. A static PIN typed over SMS persists in the phone's sent-message history and is recoverable; a one-time code is printed in the challenge message on the screen. The channel cannot support a strong token.
3. **The gateway cannot formally validate confirmation.** Hermes ultimately decides whether to execute any action. A `confirmed: true` marker added by the gateway would be spoofable by a compromised Hermes and meaningless to a healthy one. Gating Hermes at the Telnyx/gateway layer is not meaningful.
4. **A PIN is not a control for the threat it was vaguely aimed at** (lost/stolen phone). The real controls for that threat are allowlist revocation and line suspension, not tokens.

## Decision

- **No PIN in the MVP.** Confirmation of irreversible/costly actions is conversational and owned by Hermes: Hermes proposes, asks the user to confirm ("Reply YES to send."), the user's reply is an ordinary inbound event forwarded to Hermes, and Hermes decides whether to execute.
- **No secret lives in the gateway.** No pending-confirmation interception, no `internal/authz/pin.go`, no attempt counting, no lockout logic, no PIN expiry.
- **No gateway-side enforcement gate** on outbound actions (`dial`, `outbound_sms`). The gateway trusts Hermes; the confirmation is a UX/accident-prevention layer, not a security boundary.
- **Per-channel capture, single decision-owner.** SMS: text `"YES"`. Voice v1: DTMF `"press 1 to confirm"` (no STT in v1, so speech-based confirmation is not available until the audio bridge exists). In all cases Hermes owns the confirmation decision.
- **Confirmation policy is a Hermes-side config**: a short, explicit list of action classes that get gated (e.g., send email, place outbound call). Kept rare so confirmation does not habituate into reflex.
- **PIN revisited post-MVP** if the lost/stolen-phone use case is prioritized. When revisited, the design constraint from this ADR applies: over this channel a token cannot resist the device-holder; the honest candidates are DTMF-only PIN (never transited or stored as SMS) and out-of-band controls (allowlist revocation / line suspension), the latter being the actual security control.

## Decision rationale

- The removal deletes real complexity: `internal/authz/pin.go`, pending-confirmation state, intercept-before-forward routing, lockout, PIN expiry, and the storage/idempotency coupling those implied.
- Hermes is the only party with the semantic context to decide "this action is costly/irreversible and deserves confirmation" and to interpret a conversational reply.
- Any gateway attempt to *prove* confirmation would require reintroducing a secret — which this ADR's threat analysis showed buys nothing over the channel.

## Consequences

- Simpler gateway; the confirmation control is Hermes's responsibility.
- Confirmation is an accident-prevention/UX control, explicitly not a security boundary.
- Threat model note: the lost/stolen-phone adversary is unaddressed in the MVP. Accepted for now; revisit trigger recorded below.

## Open items

- Hermes-side confirmation policy (which action classes are gated) — Hermes's spec, not the gateway's.
- Lost/stolen phone control (allowlist revocation / line suspension UX, or DTMF-PIN) — revisit trigger for a future ADR.
