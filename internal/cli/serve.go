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
	"github.com/rag-platform/ragctl/internal/crypto"
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

// rateLimiterSweepInterval is how often the rate limiter reclaims idle buckets,
// and usageFlushInterval how often the usage counter flushes (SPEC-10 §6).
const (
	rateLimiterSweepInterval = time.Minute
	usageFlushInterval       = 30 * time.Second
)

// serveConfig carries the resolved inputs runAPIServer needs.
type serveConfig struct {
	Addr       string
	Obs        ObsSettings
	Cfg        config.Config
	ControlURL string
	Cipher     *crypto.Cipher
	Secure     bool
}

// runAPIServer builds the SPEC-07 §1 public router from the control-plane
// services (EPIC-03) and serves it on addr, logging to logw. It runs the
// rate-limiter idle sweep and the usage-counter flush loop for the process
// lifetime and drains them on shutdown, and configures the OTel tracer from the
// obs settings. It blocks until ctx is cancelled (SIGINT/SIGTERM) or the listener
// fails, then shuts the HTTP server down gracefully (STORY-04.1).
func runAPIServer(ctx context.Context, sc serveConfig, logw io.Writer) error {
	log := obs.Logger("ragctl", obs.ParseLevel(sc.Obs.LogLevel), logw)

	shutdownTracing, err := obs.SetupTracing(ctx, obs.TracingConfig{
		Service:      "ragctl",
		OTLPEndpoint: sc.Obs.OTLPEndpoint,
		Insecure:     sc.Obs.OTLPInsecure,
		SamplerRatio: sc.Obs.SamplerRatio,
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

	// Cancel on SIGINT/SIGTERM so the command shuts down gracefully. Background
	// loops share this context so they stop with the server.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app, err := buildAPIServer(ctx, log, metrics, sc.Cfg, sc.ControlURL, sc.Cipher, sc.Secure)
	if err != nil {
		return err
	}
	defer app.Close()

	// Background maintenance loops (SPEC-10 §6, ADR-0026). Run stops when ctx is
	// cancelled; the usage counter drains a final time on shutdown.
	go app.limiter.Run(ctx, rateLimiterSweepInterval)
	go app.usage.Run(ctx, usageFlushInterval)

	srv := &http.Server{Addr: sc.Addr, Handler: app.Handler}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("serving", "addr", sc.Addr)
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
