package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/riverqueue/river/rivertype"

	"github.com/rag-platform/ragctl/internal/obs"
)

// TestJobMetricsMiddlewareRecordsDurationAndFailure proves the middleware records
// jobs_duration_seconds per kind for every job and increments jobs_failed_total
// only for a terminally-failing one, passing the handler error through unchanged
// (SPEC-10 §2/§5).
func TestJobMetricsMiddlewareRecordsDurationAndFailure(t *testing.T) {
	m := obs.NewMetrics()
	mw := metricsMiddleware{m: m}

	ok := &rivertype.JobRow{ID: 1, Kind: "ingest_document"}
	if err := mw.Work(context.Background(), ok, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("ok job: %v", err)
	}

	boom := errors.New("boom")
	bad := &rivertype.JobRow{ID: 2, Kind: "sync_source"}
	if err := mw.Work(context.Background(), bad, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("failing job returned %v, want the handler error unchanged", err)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"jobs_duration_seconds",
		`kind="ingest_document"`,
		"jobs_failed_total",
		`kind="sync_source"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("job metrics missing %q:\n%s", want, body)
		}
	}
	// The successful kind must NOT have a failure series.
	if strings.Contains(body, `jobs_failed_total{kind="ingest_document"}`) {
		t.Fatalf("ingest_document wrongly counted as failed:\n%s", body)
	}
}

// TestJobMetricsMiddlewareNilMetricsSafe proves a worker built without metrics
// (nil) still runs jobs.
func TestJobMetricsMiddlewareNilMetricsSafe(t *testing.T) {
	mw := metricsMiddleware{m: nil}
	job := &rivertype.JobRow{ID: 3, Kind: "gc_tenant"}
	if err := mw.Work(context.Background(), job, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("nil-metrics job: %v", err)
	}
}
