# ADR-0001 — Authenticate Hermes→Gateway `/actions` with ServiceAccount workload identity

- Status: Superseded 2026-08-05 by [ADR-0006](0006-hermes-openai-api-and-telnyx-mcp.md) §5 — `/actions` and its SA-token/OIDC machinery are retired, replaced by two static shared secrets (`API_SERVER_KEY`, `MCP_TOKEN`). Retained here for the record.
- Date: 2026-08-03
- Deciders: gateway architect (grilling session)
- Amended: 2026-08-04 (removed the dev-mode bypass; `/actions` auth is required at startup)

## Context

The handoff doc (`hermes-handoff.md` §4) exposes `POST /actions` (Hermes → gateway) with no authentication specified. This endpoint can trigger the system's most privileged primitives: outbound SMS and outbound calls placed from the user's numbers, at the user's cost.

The original proposal was to rely on Kubernetes NetworkPolicies as the sole trust boundary for this endpoint. Rejected because:

1. Default k3s CNI (flannel) does not enforce Kubernetes NetworkPolicies — policies would be accepted but silently ignored.
2. The handoff doc contemplates co-locating gateway and Hermes initially; a shared pod removes any network boundary.
3. Risk inversion: the inbound money-triggering path (Telnyx webhooks) is Ed25519-verified, while the outbound money-spending path (`/actions`) would be trusted only at the network layer.

Guiding principle: cluster-specific configuration must not determine the implementation. The auth mechanism should depend on open standards, not on Kubernetes internals.

## Decision

Hermes authenticates to the gateway by presenting a **projected ServiceAccount token** (mounted at `/var/run/secrets/kubernetes.io/serviceaccount/token`) as an HTTP bearer token on `POST /actions`.

The gateway verifies the token **locally against the cluster's OIDC issuer JWKS**:

1. Resolve the issuer's discovery document (`https://<issuer>/.well-known/openid-configuration`) for `jwks_uri`.
2. Fetch and cache the JWKS.
3. Validate the JWT: signature, `exp`, `iss` matching the configured issuer, and `aud` containing the pinned audience.

**Audience pinning:** Hermes's projected token requests a dedicated audience `hermes-gateway`; the gateway rejects tokens without it. This ensures a token valid for the gateway is not simultaneously valid for the kube-apiserver.

**Authorization:** the authenticated ServiceAccount (`system:serviceaccount:<ns>:<name>`) must match an explicit allowlist (only Hermes's SA).

**Legacy tokens rejected:** long-lived `kubernetes.io/service-account-token` secret tokens (no expiry, no audience) are rejected by design.

**Dev-mode bypass:** none (amended 2026-08-04). `/actions` authentication is
required at startup: the gateway refuses to start without issuer/audience/
subject configuration, and no bypass flag exists. Out-of-cluster development
runs against a local dummy OIDC issuer (`cmd/dummy-oidc`) that serves
discovery, JWKS, and dev tokens.

**Cluster prerequisite (deployment checklist item, not implementation):** k3s must run the apiserver with `service-account-issuer` set to an https URL (via `kube-apiserver-arg`) so the OIDC discovery document and JWKS are published.

## Decision rationale

Why local JWKS verification instead of the TokenReview API:

- TokenReview couples the implementation to the Kubernetes apiserver API and requires RBAC (`create tokenreviews`) on the gateway's ServiceAccount, plus a per-request control-plane round trip on every action.
- JWKS verification depends only on OIDC/JWT standards. The same verification code works against any issuer in any cluster, and can run outside the cluster entirely if the issuer URL is resolvable.
- No per-request apiserver dependency; verification fails closed on apiserver outage (no control-plane dependency at runtime).

Why ServiceAccount identity instead of a static shared secret:

- Tokens are short-lived (~1h), auto-rotated by the kubelet, and bound to the pod's ServiceAccount.
- No static secret to manage or rotate manually.
- Works regardless of CNI/NetworkPolicy enforcement.

## Consequences

- The gateway must reach the issuer's discovery/JWKS endpoints (in-cluster: `https://kubernetes.default.svc`).
- The trust unit is the Hermes pod, not a process within it (any process in the pod can present its token).
- If the kube-apiserver is down, `/actions` fails closed → the gateway's fallback path triggers.
- Changing the issuer later invalidates all existing SA tokens (cluster-wide pod restart).
- NetworkPolicies are retained as defense-in-depth on the `/actions` endpoint, no longer the primary control.
- Enabling the issuer on k3s invalidates existing SA tokens once at rollout.

## Open items

- None outstanding (the dev-mode bypass shape was resolved 2026-08-04 by removing the bypass and adding a dummy issuer).
