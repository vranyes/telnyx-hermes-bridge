// Package config loads gateway configuration from environment variables.
//
// Only deployment-specific values (Telnyx keys, Hermes URL, API server key,
// allowlist, log level) are env-driven. All tuning constants — retry budgets,
// the rolling-window turn count, the per-sender queue cap, dedup/tolerance
// windows — are hard-coded here per ADR-0006 §1/§2 and do not vary by
// environment.
//
// All secrets come from environment variables so they can be sourced from k3s
// Secrets in the deployment. Nothing is read from disk.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Hard-coded tuning (ADR-0006 §1/§2). These are not knobs to be tuned by
// operators — they are part of the design. If the design changes, the value
// changes here, in code review.
const (
	// retryBudget bounds the Gateway -> Hermes retry window for one
	// synchronous chat/completions turn (ADR-0006 §2). Stays under hermes's
	// 5-minute Idempotency-Key cache so a retried request replays the cached
	// response instead of re-running the agent loop.
	retryBudget = 60 * time.Second
	// replyRetryBudget bounds the reply-SMS send retry (ADR-0006 §2, ~15s).
	// The reply is the turn's deliverable; a lost reply means a turn that ran
	// (possibly executing Hermes-internal tools) goes unreported.
	replyRetryBudget = 15 * time.Second
	// windowTurns is the rolling-window K (ADR-0006 §1, "K≈2–3 default"). The
	// window holds pruned user+assistant turns replayed in the messages array
	// of each chat/completions request to give Hermes short-term continuity.
	windowTurns = 3
	// perSenderQueue caps inbound SMS queued behind one in-flight turn per
	// sender (ADR-0006 §1, "bounded per-sender at ≈4").
	perSenderQueue = 4
	// dedupWindow bounds the Telnyx event-id dedup map (ADR-0006 §2 +
	// ADR-0004). Equal to the Idempotency-Key cache window so a duplicate that
	// slips through the dedup window still carries the same key and hits the
	// cached turn response on Hermes's side.
	dedupWindow = 5 * time.Minute
	// webhookTolerance is the accepted clock skew for webhook timestamps
	// (replay protection).
	webhookTolerance = 5 * time.Minute
	// hermesAPIPath is the chat/completions endpoint on the Hermes OpenAI-
	// compatible API (ADR-0006 §1).
	hermesAPIPath = "/v1/chat/completions"
)

// Hard-coded user-facing text (ADR-0006 §1/§2). Const rather than a default
// env value — these strings are the design's response content.
const (
	// systemPrompt is the per-turn first messages entry carrying the channel
	// context (ADR-0006 open item #3). Unconditional auto-delivery rule, the
	// "always end your turn with the reply text" requirement, and the
	// confirmation flow reference (ADR-0002).
	systemPrompt = "You are a personal AI assistant reached over SMS. " +
		"Your final message in this turn will be auto-delivered to the caller " +
		"as an SMS. Always finish your turn with the reply text you want sent. " +
		"If a turn requires a tool that has side effects the caller cannot see " +
		"until they read the reply, end with one short line summarizing the " +
		"outcome. For irreversible or costly actions, confirm conversationally " +
		"with the caller before acting (e.g. reply YES to confirm)."

	// queueOverflowText is sent when a sender has backed up more than
	// perSenderQueue inbound messages behind the in-flight turn (ADR-0006 §1).
	queueOverflowText = "I'll answer one at a time — hang on"

	// smsFallbackText is auto-sent when a turn fails to reach Hermes within
	// retryBudget, or Hermes rejects it permanently (ADR-0006 §2).
	smsFallbackText = "Sorry, something went wrong. Please try again later."
)

// Config is the full gateway configuration.
type Config struct {
	// Addr is the HTTP listen address.
	Addr string

	// TelnyxAPIKey authenticates outbound Telnyx API calls (reply path).
	TelnyxAPIKey string
	// TelnyxPublicKey is the base64-encoded Ed25519 public key used to
	// verify webhook signatures (from the Telnyx portal).
	TelnyxPublicKey string
	// TelnyxBaseURL overrides the Telnyx API base URL (tests, proxies).
	TelnyxBaseURL string
	// TelnyxFromNumber is the gateway's own sender number — used as "from"
	// on every reply SMS.
	TelnyxFromNumber string

	// HermesBaseURL is where the gateway sends chat/completions turns
	// (e.g. http://hermes-api-svc:8642). The path is hard-coded.
	HermesBaseURL string

	// APIServerKey is the shared bearer secret on Hermes's OpenAI API
	// (ADR-0006 §4). Rotated as a sealed secret; the gateway is its only
	// consumer on this leg.
	APIServerKey string

	// Allowlist is the set of phone numbers permitted to drive the gateway.
	// Required: the gateway fails closed at startup if it is empty.
	Allowlist []string

	// LogLevel is the slog level.
	LogLevel slog.Level

	// Exposed hard-coded tuning (read-only). Tests and the server wiring
	// read them through Config rather than re-importing the package consts.
	HermesAPIPath     string
	RetryBudget       time.Duration
	ReplyRetryBudget  time.Duration
	WindowTurns       int
	PerSenderQueue    int
	DedupWindow       time.Duration
	WebhookTolerance  time.Duration
	SystemPrompt      string
	QueueOverflowText string
	SMSFallbackText   string
}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	c := Config{
		Addr:             env("GATEWAY_ADDR", ":8080"),
		TelnyxAPIKey:     os.Getenv("TELNYX_API_KEY"),
		TelnyxPublicKey:  os.Getenv("TELNYX_PUBLIC_KEY"),
		TelnyxBaseURL:    env("TELNYX_BASE_URL", "https://api.telnyx.com/v2"),
		TelnyxFromNumber: os.Getenv("TELNYX_FROM_NUMBER"),

		HermesBaseURL: os.Getenv("HERMES_BASE_URL"),
		APIServerKey:  os.Getenv("API_SERVER_KEY"),

		Allowlist: csv(os.Getenv("ALLOWLIST")),

		// Hard-coded tuning surfaced as read-only fields.
		HermesAPIPath:     hermesAPIPath,
		RetryBudget:       retryBudget,
		ReplyRetryBudget:  replyRetryBudget,
		WindowTurns:       windowTurns,
		PerSenderQueue:    perSenderQueue,
		DedupWindow:       dedupWindow,
		WebhookTolerance:  webhookTolerance,
		SystemPrompt:      systemPrompt,
		QueueOverflowText: queueOverflowText,
		SMSFallbackText:   smsFallbackText,
	}

	if c.TelnyxAPIKey == "" {
		return c, fmt.Errorf("config: TELNYX_API_KEY is required")
	}
	if c.TelnyxPublicKey == "" {
		return c, fmt.Errorf("config: TELNYX_PUBLIC_KEY is required")
	}
	if c.TelnyxFromNumber == "" {
		return c, fmt.Errorf("config: TELNYX_FROM_NUMBER is required")
	}
	if c.HermesBaseURL == "" {
		return c, fmt.Errorf("config: HERMES_BASE_URL is required")
	}
	if c.APIServerKey == "" {
		return c, fmt.Errorf("config: API_SERVER_KEY is required")
	}
	if len(c.Allowlist) == 0 {
		return c, fmt.Errorf("config: ALLOWLIST is required")
	}

	c.LogLevel = slog.LevelInfo
	if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
		var l slog.Level
		if err := l.UnmarshalText([]byte(lvl)); err != nil {
			return c, fmt.Errorf("config: invalid LOG_LEVEL %q: %w", lvl, err)
		}
		c.LogLevel = l
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func csv(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
