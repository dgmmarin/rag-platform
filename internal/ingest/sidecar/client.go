// Package sidecar is the Go client for the Python parsing sidecar (ADR-0006,
// SPEC-05 §2): it POSTs the heavy formats (PDF/DOCX/PPTX/XLSX) the Go parsers
// cannot read and decodes the response into the same parse.Normalised the Go
// parsers produce, so both halves feed one downstream chunker. The Go
// parse.Registry returns parse.ErrUnsupportedMIME for exactly these formats; the
// sink (STORY-05.6) routes that to this client.
//
// The client applies the SPEC-05 §2 parse budget (120 s timeout), retries
// transient failures with exponential backoff honouring Retry-After, and wraps
// each call in an OpenTelemetry span with W3C trace-context propagation to the
// sidecar (using the already-installed global propagator — no otelhttp dep).
package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/rag-platform/ragctl/internal/ingest/parse"
)

// ErrUnsupportedFormat is returned when the sidecar rejects the MIME (HTTP 415) —
// the worker should not have routed this format here; it is never retried.
var ErrUnsupportedFormat = errors.New("sidecar: unsupported format")

// ErrParseFailed is returned when the sidecar could not parse the document (HTTP
// 422). Per SPEC-05 §2/§8 the worker records metadata.parse_error and skips the
// document; the whole sync does not fail, and the call is not retried.
var ErrParseFailed = errors.New("sidecar: parse failed")

const (
	// DefaultTimeout is the per-request parse budget (SPEC-05 §2: sidecar timeout
	// 120 s). It bounds the whole request including body upload and response read.
	DefaultTimeout = 120 * time.Second
	// defaultMaxRetries is the number of RETRIES (so up to 1+N attempts) on a
	// transient failure (SPEC-05 §8: 3 attempts).
	defaultMaxRetries = 2
	baseBackoff       = 200 * time.Millisecond
	maxBackoff        = 5 * time.Second
)

// Client calls a single parsing sidecar. Safe for concurrent use.
type Client struct {
	baseURL    string
	httpc      *http.Client
	maxRetries int
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

// Option customises a Client.
type Option func(*Client)

// WithHTTPClient overrides the HTTP client (and thus the timeout). The default
// carries DefaultTimeout.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpc = h } }

// WithMaxRetries sets the number of retries after the first attempt.
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = n } }

// New returns a Client for the sidecar at baseURL (e.g. http://parser:8081).
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpc:      &http.Client{Timeout: DefaultTimeout},
		maxRetries: defaultMaxRetries,
		tracer:     otel.Tracer("ingest/sidecar"),
		propagator: otel.GetTextMapPropagator(),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Parse sends one document to the sidecar and returns its Normalised form.
// filename is advisory (sent as the multipart filename); mimeType is the
// canonical upload MIME the sidecar dispatches on. ErrUnsupportedFormat and
// ErrParseFailed are terminal (not retried); transient failures (transport
// errors, 429, 5xx) are retried with backoff up to the configured limit.
func (c *Client) Parse(ctx context.Context, filename, mimeType string, data []byte) (parse.Normalised, error) {
	ctx, span := c.tracer.Start(ctx, "sidecar.parse", trace.WithAttributes(
		attribute.String("parse.mime", mimeType),
		attribute.Int("parse.bytes", len(data)),
	))
	defer span.End()

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			var te *transientError
			errors.As(lastErr, &te)
			if err := sleep(ctx, backoff(attempt, te)); err != nil {
				return parse.Normalised{}, err
			}
		}
		n, err := c.doParse(ctx, filename, mimeType, data)
		if err == nil {
			span.SetAttributes(attribute.Int("parse.blocks", len(n.Blocks)))
			return n, nil
		}
		lastErr = err
		var te *transientError
		if !errors.As(err, &te) {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return parse.Normalised{}, err
		}
		span.AddEvent("retry", trace.WithAttributes(attribute.Int("attempt", attempt+1)))
	}
	err := fmt.Errorf("sidecar: giving up after %d attempts: %w", c.maxRetries+1, lastErr)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	return parse.Normalised{}, err
}

func (c *Client) doParse(ctx context.Context, filename, mimeType string, data []byte) (parse.Normalised, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("mime", mimeType); err != nil {
		return parse.Normalised{}, err
	}
	fw, err := mw.CreateFormFile("file", nonEmpty(filename, "document"))
	if err != nil {
		return parse.Normalised{}, err
	}
	if _, err := fw.Write(data); err != nil {
		return parse.Normalised{}, err
	}
	if err := mw.Close(); err != nil {
		return parse.Normalised{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/parse", &body)
	if err != nil {
		return parse.Normalised{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c.propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := c.httpc.Do(req)
	if err != nil {
		// Transport error (incl. context deadline): transient, no Retry-After.
		return parse.Normalised{}, &transientError{err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
		var n parse.Normalised
		if err := json.NewDecoder(resp.Body).Decode(&n); err != nil {
			return parse.Normalised{}, fmt.Errorf("sidecar: decode response: %w", err)
		}
		return n, nil
	case resp.StatusCode == http.StatusUnsupportedMediaType: // 415
		return parse.Normalised{}, fmt.Errorf("%w: %s", ErrUnsupportedFormat, snippet(resp.Body))
	case resp.StatusCode == http.StatusUnprocessableEntity: // 422
		return parse.Normalised{}, fmt.Errorf("%w: %s", ErrParseFailed, snippet(resp.Body))
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500: // 429, 5xx
		return parse.Normalised{}, &transientError{
			err:        fmt.Errorf("sidecar: status %d: %s", resp.StatusCode, snippet(resp.Body)),
			retryAfter: retryAfter(resp.Header),
		}
	default:
		return parse.Normalised{}, fmt.Errorf("sidecar: status %d: %s", resp.StatusCode, snippet(resp.Body))
	}
}

// Healthz probes the sidecar's liveness endpoint.
func (c *Client) Healthz(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sidecar: healthz status %d", resp.StatusCode)
	}
	return nil
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
