package telnyx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Client is the outbound Telnyx API client. The SMS relay gateway uses it only
// to send reply SMS to verified inbound senders (ADR-0006 §1 — "no outbound
// capability beyond replying to a verified sender"). Per ADR-0004 the gateway
// issues each command once with no retry; the reply-send retry wrapper lives
// one layer up (the SMS handler) so the budget is observable there (ADR-0006 §2).
type Client struct {
	apiKey     string
	baseURL    string
	fromNumber string
	http       *http.Client
	logger     *slog.Logger
}

// NewClient builds a Telnyx API client.
func NewClient(apiKey, baseURL, fromNumber string, httpc *http.Client, logger *slog.Logger) *Client {
	if httpc == nil {
		httpc = &http.Client{Timeout: 10 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		apiKey:     apiKey,
		baseURL:    strings.TrimRight(baseURL, "/"),
		fromNumber: fromNumber,
		http:       httpc,
		logger:     logger,
	}
}

// SendSMS sends a one-way SMS to to from the gateway's configured sender. The
// caller owns retry (ADR-0006 §2 — reply-send retry lives in the SMS handler).
func (c *Client) SendSMS(ctx context.Context, to, text string) error {
	body, _ := json.Marshal(map[string]any{
		"from": c.fromNumber,
		"to":   to,
		"text": text,
	})
	resp, err := c.do(ctx, http.MethodPost, "/messages", body)
	if err != nil {
		return err
	}
	return requireSuccess(resp, "send sms to "+to)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return c.http.Do(req)
}

func requireSuccess(resp *http.Response, what string) error {
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("telnyx: %s: unexpected status %d", what, resp.StatusCode)
}
