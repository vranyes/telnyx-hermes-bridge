# Deployment guide (MVP — manifests only; apply manually)

The gateway is built with `ko` (see `ko.yaml`) and pushed to
`ghcr.io/vranyes/telnyx-hermes-bridge/gateway` by the `build-and-push`
GitHub Actions workflow (on every `main` push and `v*` tag), then deployed
behind the existing nftables LB/KubeVIP ingress path. This pass writes
manifests only — no cluster apply.

## 0. Scope (ADR-0006)

The MVP gateway is a **pure SMS relay**: one inbound SMS = one
synchronous Hermes chat/completions turn = final assistant text auto-sent
back to the caller. There is no `/actions` endpoint, no OIDC/JWKS machinery,
no ServiceAccount token projection, no voice path, and no third-party
sends. The gateway's outbound Telnyx surface is limited to replying to a
verified inbound sender. All tuning constants (retry budget, rolling
window K, per-sender queue cap, dedup/tolerance windows) are hard-coded
in `internal/config/config.go` and not operator-configurable.

## 1. Build & push

The `build-and-push` GitHub Actions workflow calls `make gateway-image`
and pushes to `ghcr.io/vranyes/telnyx-hermes-bridge/gateway` (multi-arch
amd64/arm64) on every `main` push (`latest` + short SHA tags) and every
`v*` tag (`latest` + tag name). Locally:

```
make gateway-image
```

Replace the `image:` in `deploy/k8s.yaml` with the pushed registry
reference.

## 2. Configure secrets

Fill the `stringData` in `deploy/k8s.yaml` (or manage via SealedSecrets /
External Secrets as the homelab prefers):

| Env                  | Source                                          |
|----------------------|-------------------------------------------------|
| `TELNYX_API_KEY`     | Telnyx portal — API key (bearer auth)           |
| `TELNYX_PUBLIC_KEY`  | Telnyx portal → Public Key (base64 Ed25519)     |
| `TELNYX_FROM_NUMBER` | Your Telnyx number (E.164) — the SMS sender     |
| `API_SERVER_KEY`     | The shared bearer secret on Hermes's OpenAI API |

The `API_SERVER_KEY` lives as a sealed secret in gitops. Rotating it is a
Hermes-side change (new value + Hermes restart) since the gateway is the
only consumer — the same rotation pattern as the existing
`TELNYX_API_KEY` (ADR-0006 §4).

## 3. Apply

```
kubectl create namespace hermes
kubectl apply -f deploy/k8s.yaml
```

Wire the public ingress through the existing nftables LB / KubeVIP /
Traefik ingress controller (see `homelab-ingress`):

- `POST /webhooks/telnyx/sms` must be reachable from the internet (Telnyx
  delivers here).
- `/healthz`, `/readyz`, `/metrics` should stay cluster-internal.

Set the Telnyx webhook URL in the portal to
`https://<ingress-host>/webhooks/telnyx/sms`. There is no voice webhook
path on the SMS-MVP.

## 4. Verify

- `kubectl get deploy hermes-gateway -n hermes` → 1/1 ready.
- Probes: `curl https://<ingress>/healthz` → `ok`.
- Send a test SMS; watch `kubectl logs` for `sms turn completed`.

## 5. Full environment reference

| Env                  | Default                     | Purpose                                                              |
|----------------------|-----------------------------|----------------------------------------------------------------------|
| `GATEWAY_ADDR`       | `:8080`                     | Listen address                                                       |
| `TELNYX_API_KEY`     | —                           | Required. Outbound Telnyx API auth                                   |
| `TELNYX_PUBLIC_KEY`  | —                           | Required. Base64 Ed25519 webhook key                                 |
| `TELNYX_BASE_URL`    | `https://api.telnyx.com/v2` | API base                                                             |
| `TELNYX_FROM_NUMBER` | —                           | Required. Gateway's SMS sender (replied `from`)                      |
| `HERMES_BASE_URL`    | —                           | Required. Hermes OpenAI API base (e.g. <http://hermes-api-svc:8642>) |
| `API_SERVER_KEY`     | —                           | Required. Shared bearer secret on Hermes's OpenAI API                |
| `ALLOWLIST`          | — (Required)                | Comma-separated E.164 caller allowlist; empty fails startup          |
| `LOG_LEVEL`          | `info`                      | slog level (`debug`, `info`, `warn`, `error`)                        |

Tuning (hard-coded in `internal/config/config.go`, not env-overridable):

| Const              | Value                  | Purpose                                                     |
|--------------------|------------------------|-------------------------------------------------------------|
| `retryBudget`      | 60s                    | Gateway → Hermes turn retry budget (ADR-0006 §2)            |
| `replyRetryBudget` | 15s                    | Reply-SMS send retry budget (ADR-0006 §2)                   |
| `windowTurns`      | 3                      | Rolling-window K in user+assistant turns (ADR-0006 §1)      |
| `perSenderQueue`   | 4                      | Max queued turns per sender behind one active (ADR-0006 §1) |
| `dedupWindow`      | 5m                     | Telnyx event-id dedup window (aligns with Idempotency-Key)  |
| `webhookTolerance` | 5m                     | Webhook timestamp replay window                             |
| `hermesAPIPath`    | `/v1/chat/completions` | OpenAI endpoint on Hermes's API server                      |

## 6. Open items (ADR-0006 §Open items)

- Verify Hermes's `Idempotency-Key` cache on the deployed build
  (`v2026.7.30`): confirm `/v1/chat/completions` responses are cached by
  key for 5 minutes before relying on it for retry safety. If not, retry
  is limited to connection-level failures.
- `max_concurrent_runs` cap behavior (default 10): confirm the 429 shape
  and that the gateway's `Retry-After`-honoring path works.
- The system-prompt text is hard-coded in `internal/config/config.go`;
  the deployed Hermes may ship its own system prompt in its
  `config.yaml`. The gateway's per-turn first `messages` entry carries
  the SMS-channel context (auto-delivery rule, always-end-with-reply
  requirement) regardless.

## 7. Flux wiring (if desired)

The gitops repo points a `Kustomization` at `./apps/hermes`. Either commit
`deploy/k8s.yaml` there (adding it to that path's `kustomization.yaml`),
or keep these manifests in this repo and have the Flux `GitRepository`
source this repo instead. Not applied in this pass.
