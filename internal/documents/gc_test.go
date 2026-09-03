package documents

import (
	"testing"
	"time"
)

// The SQL sweeps are exercised by the e2e (test/e2e/gc_e2e_test.go) against a real
// tenant DB — a *tenant.DB is unforgeable, so the DML is not unit-mockable. Here we
// pin the pure policy/metrics logic the worker relies on.

func TestGCPolicyDefaults(t *testing.T) {
	// A zero policy fills the three SPEC-03 §4 day windows and the batch cap, but
	// leaves CrawlPageStale zero (the SPEC states it in syncs, not time — see gc.go).
	got := GCPolicy{}.withDefaults()
	if got.VersionRetention != defaultVersionRetention {
		t.Errorf("VersionRetention = %v, want %v", got.VersionRetention, defaultVersionRetention)
	}
	if got.DeletedDocRetention != defaultDeletedDocRetention {
		t.Errorf("DeletedDocRetention = %v, want %v", got.DeletedDocRetention, defaultDeletedDocRetention)
	}
	if got.QueryLogRetention != defaultQueryLogRetention {
		t.Errorf("QueryLogRetention = %v, want %v", got.QueryLogRetention, defaultQueryLogRetention)
	}
	if got.BatchSize != defaultGCBatchSize {
		t.Errorf("BatchSize = %d, want %d", got.BatchSize, defaultGCBatchSize)
	}
	if got.CrawlPageStale != 0 {
		t.Errorf("CrawlPageStale = %v, want 0 (skip when unset)", got.CrawlPageStale)
	}

	// Explicit values are preserved, not overwritten by defaults.
	set := GCPolicy{
		VersionRetention:    time.Hour,
		DeletedDocRetention: 2 * time.Hour,
		QueryLogRetention:   3 * time.Hour,
		CrawlPageStale:      4 * time.Hour,
		BatchSize:           7,
	}
	if got := set.withDefaults(); got != set {
		t.Errorf("withDefaults mutated an explicit policy: %+v -> %+v", set, got)
	}

	// A negative batch size is a caller error, not a request for "unbounded"; it
	// falls back to the default so a bad config can never issue an unbounded delete.
	if got := (GCPolicy{BatchSize: -5}).withDefaults(); got.BatchSize != defaultGCBatchSize {
		t.Errorf("negative BatchSize = %d, want default %d", got.BatchSize, defaultGCBatchSize)
	}
}

func TestGCMetricsTotal(t *testing.T) {
	m := GCMetrics{OldVersions: 1, DeletedDocs: 2, QueryLogs: 4, CrawlPages: 8, Chunks: 99}
	// Total counts the four retention classes; Chunks is a cascade side effect, not a
	// class of its own, so it is excluded from the class total.
	if got := m.Total(); got != 15 {
		t.Errorf("Total = %d, want 15 (chunks excluded)", got)
	}
	if (GCMetrics{}).Total() != 0 {
		t.Error("empty metrics Total should be 0")
	}
}
