package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/rag-platform/ragctl/internal/config"
	"github.com/rag-platform/ragctl/internal/obs"
)

// ObsSettings is the resolved observability configuration a serving command
// needs at startup (STORY-01.6, SPEC-10): the log level and the OTel tracer
// wiring. Tracing is disabled when OTLPEndpoint is empty.
type ObsSettings struct {
	LogLevel     string
	OTLPEndpoint string
	OTLPInsecure bool
	SamplerRatio float64
}

// obsSettingsFromConfig maps a loaded Config to ObsSettings.
func obsSettingsFromConfig(cfg config.Config) ObsSettings {
	return ObsSettings{
		LogLevel:     cfg.LogLevel,
		OTLPEndpoint: cfg.OTLPEndpoint,
		OTLPInsecure: cfg.OTLPInsecure,
		SamplerRatio: cfg.TraceSamplerRatio,
	}
}

// serveShutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests to drain before the listener is forced closed.
const serveShutdownTimeout = 10 * time.Second

// runServer starts the scaffolding HTTP server (/healthz, /readyz, /metrics
// behind the obs middleware) on addr, logging to logw, and blocks until ctx is
// cancelled (SIGINT/SIGTERM) or the listener fails. It configures the global
// OpenTelemetry tracer from o and shuts it down on exit. This is the STORY-01.6
// minimal server; STORY-04.1 replaces it with the full router chain.
func runServer(ctx context.Context, addr string, o ObsSettings, logw io.Writer) error {
	log := obs.Logger("ragctl", obs.ParseLevel(o.LogLevel), logw)

	shutdownTracing, err := obs.SetupTracing(ctx, obs.TracingConfig{
		Service:      "ragctl",
		OTLPEndpoint: o.OTLPEndpoint,
		Insecure:     o.OTLPInsecure,
		SamplerRatio: o.SamplerRatio,
	})
	if err != nil {
		return fmt.Errorf("serve: setup tracing: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
		defer cancel()
		_ = shutdownTracing(sctx)
	}()

	metrics := obs.NewMetrics()
	srv := &http.Server{
		Addr:    addr,
		Handler: obs.NewServeMux(log, metrics),
	}

	// Cancel on SIGINT/SIGTERM so the command shuts down gracefully.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		log.Info("serving", "addr", addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
		defer cancel()
		return srv.Shutdown(sctx)
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	}
}
