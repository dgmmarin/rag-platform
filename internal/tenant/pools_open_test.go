package tenant

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestNumPoolsCountsOpenPools proves NumPools reports the resolver's live pool
// count, the value the tenant_pools_open gauge reads (SPEC-10 §2).
func TestNumPoolsCountsOpenPools(t *testing.T) {
	id := ID(uuid.New())
	r := newTestResolver(fakeRegistry{rec: activeRecord(id, StatusActive)})

	if got := r.NumPools(); got != 0 {
		t.Fatalf("NumPools before Open = %d, want 0", got)
	}
	if _, err := r.Open(context.Background(), id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := r.NumPools(); got != 1 {
		t.Fatalf("NumPools after Open = %d, want 1", got)
	}
}
