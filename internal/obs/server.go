package obs

import (
	"log/slog"
	"net/http"
)

// NewServeMux assembles the minimal scaffolding HTTP handler: /healthz, /readyz
// and /metrics, all wrapped by the obs Middleware so each request gets a request
// id, a structured log line, and a histogram observation. Optional readiness
// checks are passed through to /readyz.
//
// This is deliberately small — the full router and middleware chain (auth,
// tenant resolution, rate limiting, recovery, CORS) is STORY-04.1. This exists so
// STORY-01.6 has real served behaviour to test.
func NewServeMux(log *slog.Logger, m *Metrics, checks ...Check) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", HealthzHandler())
	mux.Handle("/readyz", ReadyzHandler(checks...))
	mux.Handle("/metrics", m.Handler())
	return Middleware(log, m)(mux)
}
