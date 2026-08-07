package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/vranyes/telnyx-hermes-bridge/internal/authz"
	"github.com/vranyes/telnyx-hermes-bridge/internal/config"
	"github.com/vranyes/telnyx-hermes-bridge/internal/handlers"
	"github.com/vranyes/telnyx-hermes-bridge/internal/hermes"
	"github.com/vranyes/telnyx-hermes-bridge/internal/metrics"
	"github.com/vranyes/telnyx-hermes-bridge/internal/telnyx"
	"github.com/vranyes/telnyx-hermes-bridge/internal/transcript"
)

// Server owns the gateway's dependencies and HTTP lifecycle.
type Server struct {
	cfg     config.Config
	logger  *slog.Logger
	metrics *metrics.Metrics
	handler http.Handler
}

// New builds the gateway. An error here means the process cannot serve
// safely.
func New(cfg config.Config, logger *slog.Logger) (*Server, error) {
	verifier, err := telnyx.NewVerifier(cfg.TelnyxPublicKey, cfg.WebhookTolerance)
	if err != nil {
		return nil, err
	}

	m := &metrics.Metrics{}
	dedup := telnyx.NewDedup(cfg.DedupWindow)
	allowlist := authz.NewAllowlist(cfg.Allowlist, logger)
	telnyxClient := telnyx.NewClient(
		cfg.TelnyxAPIKey, cfg.TelnyxBaseURL,
		cfg.TelnyxFromNumber, nil, logger,
	)
	hermesClient := hermes.NewClient(
		cfg.HermesBaseURL, cfg.HermesAPIPath, cfg.APIServerKey,
		cfg.RetryBudget, nil, logger, m,
	)

	// NOTE: the canned SMS fallback on retry exhaustion is dispatched by
	// the SMS handler's runTurn (internal/handlers/sms_webhook.go), the
	// single owner of the reply-send path. The hermes client returns
	// ErrExhausted and the handler converts it into the fallback SMS.
	// Earlier revisions had a Fallback seam on the client that produced
	// a duplicate send; removed.

	window := transcript.New(cfg.WindowTurns)
	queue := handlers.NewTurnQueue(cfg.PerSenderQueue)

	smsHandler := &handlers.SMSHandler{
		Verifier:          verifier,
		Dedup:             dedup,
		Allowlist:         allowlist,
		Hermes:            hermesClient,
		Telnyx:            telnyxClient,
		Window:            window,
		Queue:             queue,
		Metrics:           m,
		Logger:            logger,
		Now:               time.Now,
		SystemPrompt:      cfg.SystemPrompt,
		QueueOverflowText: cfg.QueueOverflowText,
		SMSFallbackText:   cfg.SMSFallbackText,
		ReplyRetryBudget:  cfg.ReplyRetryBudget,
	}

	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/telnyx/sms", smsHandler)
	mux.Handle("GET /healthz", http.HandlerFunc(handlers.Healthz))
	mux.Handle("GET /readyz", http.HandlerFunc(handlers.Readyz))
	mux.Handle("GET /metrics", m.Handler())

	handler := withRecovery(logger, withRequestID(withLogging(logger, mux)))

	return &Server{
		cfg:     cfg,
		logger:  logger,
		metrics: m,
		handler: handler,
	}, nil
}

// Run serves HTTP until ctx is canceled, then drains gracefully.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	httpServer := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		// WriteTimeout covers the synchronous SMS turn + reply-send retry
		// worst case (60s turn + 15s reply + slack). The handler dispatches
		// the turn on a goroutine so the HTTP response itself returns right
		// after the dispatch, not after the turn — this is just a backstop.
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("gateway listening", "addr", s.cfg.Addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.logger.Info("gateway shutting down")
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	}
}
