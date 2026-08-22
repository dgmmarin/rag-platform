// Package api assembles the public HTTP server for the RAG platform: the router,
// the SPEC-07 §1 middleware chain, the JSON error envelope, and the health and
// readiness endpoints. It mounts the auth and control-plane handlers built in
// EPIC-03 behind the appropriate middleware and leaves clear seams for the
// per-tenant resource routes that land in later EPIC-04 stories (04.3–04.6).
//
// The router is deliberately library-agnostic: it composes net/http middleware
// (Go 1.22 ServeMux for routing) and takes every handler and middleware as an
// injected value (Deps), so this package holds no database coupling and the real
// dependencies are constructed once in `ragctl serve` (ADR-0027).
package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/rag-platform/ragctl/internal/obs"
)

// The SPEC-07 §1 error-code vocabulary. Every error the API returns uses one of
// these codes so clients can branch on a stable string rather than the status.
const (
	CodeUnauthorized      = "unauthorized"
	CodeForbidden         = "forbidden"
	CodeNotFound          = "not_found"
	CodeValidation        = "validation"
	CodeRateLimited       = "rate_limited"
	CodeTenantUnavailable = "tenant_unavailable"
	CodeConflict          = "conflict"
	CodeInternal          = "internal"
)

// errorEnvelope is the SPEC-07 §1 error body:
//
//	{"error":{"code":"...","message":"...","request_id":"..."}}
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteError writes the SPEC-07 §1 error envelope with the given status, code and
// message, echoing the request id resolved by the obs middleware so a client can
// correlate the failure with server logs. It never leaks internal detail: callers
// pass a safe, generic message.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{
		Code:      code,
		Message:   message,
		RequestID: obs.RequestIDFromContext(r.Context()),
	}})
}

// CodeForStatus maps an HTTP status to the SPEC-07 §1 error code, used by the
// generic paths (not-found, method-not-allowed, recovery) that only know a
// status. Anything unmapped falls back to "internal".
func CodeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusBadRequest:
		return CodeValidation
	case http.StatusTooManyRequests:
		return CodeRateLimited
	case http.StatusConflict:
		return CodeConflict
	case http.StatusServiceUnavailable:
		return CodeTenantUnavailable
	default:
		return CodeInternal
	}
}

// requestIDContext is a thin test/helper seam over the obs request-id context so
// the envelope test can stamp an id without importing obs internals.
func requestIDContext(ctx context.Context, id string) context.Context {
	return obs.ContextWithRequestID(ctx, id)
}
