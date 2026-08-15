// Command api serves the payment application API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zabilal/gigmile-facility/internal/platform/config"
	"github.com/zabilal/gigmile-facility/internal/platform/log"
	"github.com/zabilal/gigmile-facility/internal/store/postgres"
	transport "github.com/zabilal/gigmile-facility/internal/transport/http"
)

// shutdownGrace bounds in-flight work on shutdown. An acknowledged but
// uncommitted payment is a credit the provider thinks it delivered.
const shutdownGrace = 20 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := log.New(cfg.LogLevel)
	slog.SetDefault(logger)

	// SIGINT/SIGTERM cancel this context, which unwinds the shutdown sequence.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	store, err := postgres.New(startupCtx, cfg.DatabaseURL, cfg.MaxPoolConns)
	if err != nil {
		return err
	}
	defer store.Close()

	server := transport.NewHTTPServer(
		transport.NewServer(store, logger, cfg).Handler(), cfg)

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("listening",
			"addr", cfg.HTTPAddr,
			"apply_mode", cfg.ApplyMode,
			"environment", cfg.Environment,
			"max_pool_conns", cfg.MaxPoolConns)

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining", "grace", shutdownGrace)
	}

	// A fresh context: the signal cancelled the one above, and shutdown needs
	// its own budget rather than a dead deadline.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelDrain()

	if err := server.Shutdown(drainCtx); err != nil {
		// Forced close cut off in-flight requests, each possibly an
		// acknowledged payment we did not record.
		logger.Error("graceful shutdown timed out; forcing close", "error", err)
		_ = server.Close()
		return err
	}

	logger.Info("shutdown complete")
	return nil
}
