//go:build e2e

// ISSUE-0046 golden path: the tenant_schema_mismatch fleet scan
// (worker.SampleTenantSchemaMismatch) counts active tenants whose recorded
// schema_version is behind the expected tenant migration version, reading the
// control-plane registry only (C-3, no tenant DB opened). It runs against the real
// local control-plane Postgres.
package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/tenant"
	"github.com/rag-platform/ragctl/internal/worker"
)

// gaugeVal scrapes the metrics endpoint and returns the tenant_schema_mismatch value.
var schemaMismatchLine = regexp.MustCompile(`(?m)^tenant_schema_mismatch (\S+)$`)

func schemaMismatchGauge(t *testing.T, m *obs.Metrics) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	got := schemaMismatchLine.FindStringSubmatch(rec.Body.String())
	if got == nil {
		t.Fatalf("tenant_schema_mismatch not exposed:\n%s", rec.Body.String())
	}
	v, err := strconv.ParseFloat(got[1], 64)
	if err != nil {
		t.Fatalf("parse gauge %q: %v", got[1], err)
	}
	return v
}

// TestTenantSchemaMismatchSampler proves the scan counts exactly the active,
// behind-version tenants: a delta of +1 for one active tenant a version behind,
// and no change for an at-version active tenant or a behind but suspended tenant.
func TestTenantSchemaMismatchSampler(t *testing.T) {
	migrateControl(t)
	ctx := context.Background()
	pool := controlPool(t)
	m := obs.NewMetrics()

	expected := currentTenantSchemaVersion(t)

	sample := func() float64 {
		if err := worker.SampleTenantSchemaMismatch(ctx, pool, int64(expected), m); err != nil {
			t.Fatalf("sample: %v", err)
		}
		return schemaMismatchGauge(t, m)
	}

	// Baseline: whatever the shared control plane already holds.
	base := sample()

	// One ACTIVE tenant a version behind — must add exactly 1.
	behindID := tenant.ID(uuid.New())
	insertTenantRows(t, behindID, "mm-behind-"+uuid.NewString()[:8], string(tenant.StatusActive),
		"db_behind", "role_behind", "pw", expected-1)
	if got := sample(); got != base+1 {
		t.Fatalf("after one active-behind tenant: gauge = %v, want %v", got, base+1)
	}

	// An ACTIVE tenant AT the expected version — must not count.
	atID := tenant.ID(uuid.New())
	insertTenantRows(t, atID, "mm-at-"+uuid.NewString()[:8], string(tenant.StatusActive),
		"db_at", "role_at", "pw", expected)
	// A SUSPENDED tenant a version behind — must not count (only active tenants).
	suspID := tenant.ID(uuid.New())
	insertTenantRows(t, suspID, "mm-susp-"+uuid.NewString()[:8], string(tenant.StatusSuspended),
		"db_susp", "role_susp", "pw", expected-1)

	if got := sample(); got != base+1 {
		t.Fatalf("at-version and suspended tenants must not count: gauge = %v, want %v", got, base+1)
	}
}
