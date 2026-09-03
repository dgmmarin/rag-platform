package sidecar

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const okBody = `{"title":"Team Memo","blocks":[{"type":"heading","level":1,"text":"Team Memo"},{"type":"table","text":"| a | b |","rows":[["a","b"]]}]}`

func TestParseSuccess(t *testing.T) {
	var gotMime, gotFile, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		gotMime = r.FormValue("mime")
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("FormFile: %v", err)
		} else {
			b, _ := io.ReadAll(f)
			gotFile = string(b)
		}
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	n, err := New(srv.URL).Parse(context.Background(), "memo.docx", "application/pdf", []byte("PDFDATA"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n.Title != "Team Memo" || len(n.Blocks) != 2 {
		t.Fatalf("decoded = %+v", n)
	}
	if n.Blocks[1].Type != "table" || n.Blocks[1].Rows[0][0] != "a" {
		t.Errorf("table block = %+v", n.Blocks[1])
	}
	if gotMime != "application/pdf" {
		t.Errorf("mime field = %q", gotMime)
	}
	if gotFile != "PDFDATA" {
		t.Errorf("file bytes = %q", gotFile)
	}
	if mt, _, _ := mime.ParseMediaType(gotCT); mt != "multipart/form-data" {
		t.Errorf("content-type = %q", gotCT)
	}
}

func TestParseTerminalStatuses(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{
		{http.StatusUnsupportedMediaType, ErrUnsupportedFormat},
		{http.StatusUnprocessableEntity, ErrParseFailed},
	} {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(tc.code)
			_, _ = io.WriteString(w, `{"error":"x"}`)
		}))
		_, err := New(srv.URL).Parse(context.Background(), "d", "application/pdf", []byte("x"))
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.code, err, tc.want)
		}
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Errorf("status %d: %d calls, want 1 (terminal, no retry)", tc.code, n)
		}
		srv.Close()
	}
}

func TestParseRetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	n, err := New(srv.URL, WithMaxRetries(2)).Parse(context.Background(), "d", "application/pdf", []byte("x"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n.Title != "Team Memo" {
		t.Errorf("title = %q", n.Title)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestParseGivesUp(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := New(srv.URL, WithMaxRetries(2)).Parse(context.Background(), "d", "application/pdf", []byte("x"))
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := atomic.LoadInt32(&calls); got != 3 { // 1 + 2 retries
		t.Errorf("calls = %d, want 3", got)
	}
}

// A huge Retry-After must not be slept through when the context is short: the
// client honours Retry-After but a cancelled context wins.
func TestParseRetryAfterCappedByContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := New(srv.URL, WithMaxRetries(3)).Parse(ctx, "d", "application/pdf", []byte("x"))
	if err == nil {
		t.Fatal("expected context error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waited %v; Retry-After should have been cut short by the context", elapsed)
	}
}

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	if err := New(srv.URL).Healthz(context.Background()); err != nil {
		t.Fatalf("Healthz: %v", err)
	}
}

// With a real sampled tracer and the W3C propagator installed, the client injects
// a traceparent header the sidecar can continue.
func TestParseInjectsTraceContext(t *testing.T) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample())))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(otel.GetTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})

	var traceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent = r.Header.Get("traceparent")
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	// New() captured the previous (no-op) propagator; override it for this call.
	c := New(srv.URL)
	c.propagator = otel.GetTextMapPropagator()
	if _, err := c.Parse(context.Background(), "d", "application/pdf", []byte("x")); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.HasPrefix(traceparent, "00-") {
		t.Errorf("traceparent = %q, want a W3C header", traceparent)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "5")
	if got := retryAfter(h); got != 5*time.Second {
		t.Errorf("delta-seconds = %v, want 5s", got)
	}
	h.Set("Retry-After", http.TimeFormat[:0]) // empty-ish
	h.Set("Retry-After", "")
	if got := retryAfter(h); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
	h.Set("Retry-After", time.Now().Add(2*time.Second).UTC().Format(http.TimeFormat))
	if got := retryAfter(h); got <= 0 || got > 3*time.Second {
		t.Errorf("http-date = %v, want ~2s", got)
	}
}

func TestBackoff(t *testing.T) {
	if d := backoff(1, nil); d != baseBackoff {
		t.Errorf("attempt 1 = %v, want %v", d, baseBackoff)
	}
	if d := backoff(10, nil); d != maxBackoff {
		t.Errorf("attempt 10 = %v, want cap %v", d, maxBackoff)
	}
	if d := backoff(1, &transientError{retryAfter: 7 * time.Second}); d != 7*time.Second {
		t.Errorf("Retry-After = %v, want 7s", d)
	}
}
