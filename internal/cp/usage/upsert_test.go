package usage

import (
	"strings"
	"testing"
)

// The accumulating upsert is the load-bearing contract with SPEC-10 §6: each
// column must be summed (col = col + excluded.col), never overwritten, so a flush
// adds to whatever a prior flush (or another replica) already wrote for the day.
func TestUpsertSQLAccumulatesEveryColumn(t *testing.T) {
	sql := strings.ToLower(upsertSQL)

	if !strings.Contains(sql, "insert into usage_daily") {
		t.Fatalf("upsert must target usage_daily: %s", upsertSQL)
	}
	if !strings.Contains(sql, "on conflict (tenant_id, day) do update") {
		t.Fatalf("upsert must resolve the (tenant_id, day) primary-key conflict: %s", upsertSQL)
	}
	cols := []string{"queries", "docs_ingested", "chunks_embedded", "embed_tokens", "llm_in_tokens", "llm_out_tokens"}
	for _, c := range cols {
		accum := c + " = usage_daily." + c + " + excluded." + c
		if !strings.Contains(sql, accum) {
			t.Fatalf("column %q must accumulate (%q), got: %s", c, accum, upsertSQL)
		}
	}
}
