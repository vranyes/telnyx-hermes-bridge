package config

import (
	"strings"
	"testing"
)

func setenv(t *testing.T, k, v string) {
	t.Helper()
	t.Setenv(k, v)
}

func minimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TELNYX_API_KEY", "k")
	t.Setenv("TELNYX_PUBLIC_KEY", "p")
	t.Setenv("TELNYX_FROM_NUMBER", "+15559876543")
	t.Setenv("HERMES_BASE_URL", "http://hermes")
	t.Setenv("API_SERVER_KEY", "shared-secret")
	t.Setenv("ALLOWLIST", "+15551234567")
}

func TestLoadDefaults(t *testing.T) {
	minimalEnv(t)
	setenv(t, "ALLOWLIST", " +15551234567, +15559876543 ")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", c.Addr)
	}
	if c.RetryBudget.String() != "1m0s" {
		t.Errorf("RetryBudget = %v, want 60s", c.RetryBudget)
	}
	if c.ReplyRetryBudget.String() != "15s" {
		t.Errorf("ReplyRetryBudget = %v, want 15s", c.ReplyRetryBudget)
	}
	if c.WindowTurns != 3 {
		t.Errorf("WindowTurns = %d, want 3", c.WindowTurns)
	}
	if c.PerSenderQueue != 4 {
		t.Errorf("PerSenderQueue = %d, want 4", c.PerSenderQueue)
	}
	if c.DedupWindow.String() != "5m0s" {
		t.Errorf("DedupWindow = %v, want 5m", c.DedupWindow)
	}
	if c.HermesAPIPath != "/v1/chat/completions" {
		t.Errorf("HermesAPIPath = %q, want /v1/chat/completions", c.HermesAPIPath)
	}
	if len(c.Allowlist) != 2 || c.Allowlist[0] != "+15551234567" {
		t.Errorf("Allowlist = %v, want trimmed entries", c.Allowlist)
	}
}

func TestLoadOverrides(t *testing.T) {
	minimalEnv(t)
	setenv(t, "GATEWAY_ADDR", ":9090")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.Addr != ":9090" {
		t.Errorf("Addr = %q, want :9090", c.Addr)
	}
}

func TestLoadRequired(t *testing.T) {
	for _, missing := range []string{"TELNYX_API_KEY", "TELNYX_PUBLIC_KEY", "TELNYX_FROM_NUMBER", "HERMES_BASE_URL", "API_SERVER_KEY", "ALLOWLIST"} {
		t.Run(missing, func(t *testing.T) {
			minimalEnv(t)
			setenv(t, missing, "")
			if _, err := Load(); err == nil {
				t.Fatalf("Load() with empty %s succeeded, want error", missing)
			}
		})
	}
}

func TestLoadInvalidLogLevel(t *testing.T) {
	minimalEnv(t)
	setenv(t, "LOG_LEVEL", "loud")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("Load() with invalid LOG_LEVEL error = %v, want mention of LOG_LEVEL", err)
	}
}
