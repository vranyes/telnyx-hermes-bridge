// Command gateway runs the Hermes Telnyx SMS-relay bridge (ADR-0006).
//
// One inbound SMS = one synchronous chat/completions turn against the
// Hermes OpenAI-compatible endpoint; the turn's final assistant text is
// auto-sent back to the caller over Telnyx. The HTTP surface is:
//
//	POST /webhooks/telnyx/sms     inbound SMS from Telnyx
//	GET  /healthz /readyz /metrics
//
// Voice, /actions, and the OIDC/MCP machinery are retired or deferred
// (ADR-0006 §5).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vranyes/telnyx-hermes-bridge/internal/config"
	"github.com/vranyes/telnyx-hermes-bridge/internal/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	srv, err := server.New(cfg, logger)
	if err != nil {
		logger.Error("startup failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		logger.Error("gateway exited", "err", err)
		os.Exit(1)
	}
}
