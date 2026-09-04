package llm

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Backoff bounds shared by every provider (mirrors internal/ingest/embed and the
// sidecar client).
const (
	baseBackoff = 200 * time.Millisecond
	maxBackoff  = 5 * time.Second
)

// transientError marks a retryable failure and carries an optional server-hinted
// delay (Retry-After). 429 and 5xx (and transport errors) are transient; every
// other non-2xx is terminal and returned immediately so a 400/401 is not retried
// into the ground. The wrapped error never carries request bodies or credentials
// (C-4) — only the sanitised status line and the response snippet.
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

// httpTransient reports whether an HTTP status code is a retryable failure.
func httpTransient(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// snippet reads a bounded prefix of a provider's response body for an error
// message. This is the provider's error payload (never our request), capped so a
// verbose body cannot bloat a log line.
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
