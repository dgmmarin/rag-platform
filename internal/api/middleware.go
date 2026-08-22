package api

import (
	"log/slog"
	"net/http"

	"github.com/rag-platform/ragctl/internal/obs"
)

// Middleware is the net/http middleware shape used throughout the chain.
type Middleware = func(http.Handler) http.Handler

// chain wraps h with the given middleware so that mws[0] is the OUTERMOST layer
// (runs first) and h is innermost. Composing right-to-left preserves the natural
// reading order at the call site: chain(handler, outer, ..., inner).
func chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// Recover is the panic-recovery middleware (SPEC-07 §1): a panic in any inner
// handler is turned into a 500 error envelope without leaking the panic value or
// a stack trace to the client (fail closed). The panic is logged server-side with
// the request id for diagnosis. It sits just inside the request-id/logging layer
// so every downstream panic is caught and correlated.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					obs.With(r.Context(), log).Error("panic recovered",
						slog.Any("panic", rec),
						slog.String("method", r.Method),
						slog.String("route", r.URL.Path),
					)
					WriteError(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// cors applies a permissive-but-safe CORS policy for the browser admin UI
// (EPIC-11). It echoes no wildcard credentials: the session cookie is SameSite,
// so cross-origin credentialed requests are not the auth path. A preflight
// OPTIONS is answered 204 without reaching the chain.
func cors() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-CSRF-Token, X-Request-Id, Idempotency-Key")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
