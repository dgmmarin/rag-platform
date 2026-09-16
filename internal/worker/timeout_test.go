package worker

import (
	"testing"
	"time"

	"github.com/riverqueue/river"
)

// TestJobTimeoutsExceedRiverDefault guards ISSUE-0062: River's JobTimeoutDefault
// is 1 minute, which cancels a long-running ingestion job mid-flight with
// "context deadline exceeded". The two ingestion workers therefore override
// Timeout() with a budget that fits the work they actually do.
func TestJobTimeoutsExceedRiverDefault(t *testing.T) {
	// A single ingest_document may parse via the sidecar (up to 120s, SPEC-05 §2)
	// before it chunks and embeds, so its budget must clear the 1-minute default
	// AND the sidecar's own 120s ceiling.
	if got := (&ingestWorker{}).Timeout(nil); got <= river.JobTimeoutDefault || got <= 120*time.Second {
		t.Errorf("ingest_document Timeout = %s; want > JobTimeoutDefault (%s) and > sidecar 120s", got, river.JobTimeoutDefault)
	}

	// A sync_source crawls + parses + embeds every document of a source at a
	// politeness rate, so its budget must be well above the 1-minute default.
	if got := (&syncWorker{}).Timeout(nil); got <= river.JobTimeoutDefault {
		t.Errorf("sync_source Timeout = %s; want > JobTimeoutDefault (%s)", got, river.JobTimeoutDefault)
	}

	// Both stay under the client's RescueStuckJobsAfter net (1h default) so a truly
	// stuck job is still rescued rather than pinned forever.
	if got := (&syncWorker{}).Timeout(nil); got >= time.Hour {
		t.Errorf("sync_source Timeout = %s; want < RescueStuckJobsAfter (1h)", got)
	}
}
