package obs

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// An API request opens a server span carrying method, route and status — the root of
// the API → retrieval → provider trace (FR-OBS-03, SPEC-10).
func TestMiddlewareRecordsServerSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := apiTracer
	apiTracer = tp.Tracer("api-test")
	t.Cleanup(func() { apiTracer = prev })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := Middleware(log, NewMetrics())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/query", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	attrs := map[string]int64{}
	strs := map[string]string{}
	for _, kv := range spans[0].Attributes() {
		switch kv.Value.Type().String() {
		case "INT64":
			attrs[string(kv.Key)] = kv.Value.AsInt64()
		default:
			strs[string(kv.Key)] = kv.Value.AsString()
		}
	}
	if strs["http.method"] != "GET" || strs["http.route"] != "/v1/query" {
		t.Fatalf("span attrs = %v, want method=GET route=/v1/query", strs)
	}
	if attrs["http.status_code"] != int64(http.StatusTeapot) {
		t.Fatalf("span status_code = %d, want %d", attrs["http.status_code"], http.StatusTeapot)
	}
}
