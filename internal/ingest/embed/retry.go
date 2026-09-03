package embed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Backoff bounds shared by every provider (mirrors internal/ingest/sidecar).
const (
	baseBackoff = 200 * time.Millisecond
	maxBackoff  = 5 * time.Second
)

// doer performs one provider HTTP request with retry/backoff honouring
// Retry-After, wrapped in an OpenTelemetry span with W3C trace-context injection
// (the already-installed global propagator — no otelhttp dependency, matching the
// sidecar client). 429 and 5xx are transient (retried); every other non-2xx is
// terminal (returned immediately, never retried). It carries no provider secrets
// into logs or spans — the Authorization header is set by the caller's buildReq
// and never recorded.
type doer struct {
	provider   string
	httpc      *http.Client
	maxRetries int
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

// do runs buildReq (which must produce a fresh *http.Request each attempt, so the
// body can be re-read on retry) and returns the 200 response body.
func (d *doer) do(ctx context.Context, buildReq func(context.Context) (*http.Request, error)) ([]byte, error) {
	ctx, span := d.tracer.Start(ctx, "embed.request", trace.WithAttributes(
		attribute.String("embed.provider", d.provider),
	))
	defer span.End()

	var lastErr error
	for attempt := 0; attempt <= d.maxRetries; attempt++ {
		if attempt > 0 {
			var te *transientError
			errors.As(lastErr, &te)
			if err := sleep(ctx, backoff(attempt, te)); err != nil {
				return nil, err
			}
			span.AddEvent("retry", trace.WithAttributes(attribute.Int("attempt", attempt+1)))
		}
		body, err := d.attempt(ctx, buildReq)
		if err == nil {
			return body, nil
		}
		lastErr = err
		var te *transientError
		if !errors.As(err, &te) {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
	}
	err := fmt.Errorf("embed: %s: giving up after %d attempts: %w", d.provider, d.maxRetries+1, lastErr)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	return nil, err
}

func (d *doer) attempt(ctx context.Context, buildReq func(context.Context) (*http.Request, error)) ([]byte, error) {
	req, err := buildReq(ctx)
	if err != nil {
		return nil, err
	}
	d.propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := d.httpc.Do(req)
	if err != nil {
		// Transport error (incl. context deadline): transient, no Retry-After.
		return nil, &transientError{err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, &transientError{err: fmt.Errorf("embed: %s: read body: %w", d.provider, err)}
		}
		return b, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500: // 429, 5xx
		return nil, &transientError{
			err:        fmt.Errorf("embed: %s: status %d: %s", d.provider, resp.StatusCode, snippet(resp.Body)),
			retryAfter: retryAfter(resp.Header),
		}
	default:
		return nil, fmt.Errorf("embed: %s: status %d: %s", d.provider, resp.StatusCode, snippet(resp.Body))
	}
}

// transientError marks a retryable failure and carries an optional server-hinted
// delay (Retry-After).
type transientError struct {
	err        error
	retryAfter time.Duration
}

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

// backoff returns the delay before the given attempt (1-based): the server's
// Retry-After when present, else capped exponential backoff.
func backoff(attempt int, te *transientError) time.Duration {
	if te != nil && te.retryAfter > 0 {
		return te.retryAfter
	}
	d := baseBackoff << (attempt - 1)
	if d > maxBackoff || d <= 0 {
		return maxBackoff
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryAfter parses a Retry-After header (delta-seconds or HTTP-date) into a
// duration, or 0 when absent/unparseable.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func snippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
