package worker

import (
	"encoding/json"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// TestJobArgsKinds pins every River job kind to the SPEC-08 §1 kind string. The
// enum in schemas/control_plane.sql and the mirror depend on these exact values.
func TestJobArgsKinds(t *testing.T) {
	cases := []struct {
		args river.JobArgs
		kind string
	}{
		{IngestDocumentArgs{}, "ingest_document"},
		{SyncSourceArgs{}, "sync_source"},
		{ReindexTenantArgs{}, "reindex_tenant"},
		{GCTenantArgs{}, "gc_tenant"},
		{DeleteSourceArgs{}, "delete_source"},
		{ProvisionTenantArgs{}, "provision_tenant"},
		{DeleteTenantArgs{}, "delete_tenant"},
	}
	for _, c := range cases {
		if got := c.args.Kind(); got != c.kind {
			t.Errorf("%T.Kind() = %q, want %q", c.args, got, c.kind)
		}
	}
}

// TestJobArgsQueueAndRetries pins the queue and max-attempts of each kind to
// SPEC-08 §1: ingest kinds on the ingest queue, maintenance/platform elsewhere so
// a reindex cannot starve syncs, and the per-kind retry budgets (sync 3, provision
// 5, delete_tenant 10, gc 1).
func TestJobArgsQueueAndRetries(t *testing.T) {
	cases := []struct {
		args        river.JobArgsWithInsertOpts
		queue       string
		maxAttempts int
	}{
		{IngestDocumentArgs{}, QueueIngest, 3},
		{SyncSourceArgs{}, QueueIngest, 3},
		{ReindexTenantArgs{}, QueueMaintenance, 5},
		{GCTenantArgs{}, QueueMaintenance, 1},
		{DeleteSourceArgs{}, QueueMaintenance, 5},
		{ProvisionTenantArgs{}, QueuePlatform, 5},
		{DeleteTenantArgs{}, QueuePlatform, 10},
	}
	for _, c := range cases {
		opts := c.args.InsertOpts()
		if opts.Queue != c.queue {
			t.Errorf("%T queue = %q, want %q", c.args, opts.Queue, c.queue)
		}
		if opts.MaxAttempts != c.maxAttempts {
			t.Errorf("%T max attempts = %d, want %d", c.args, opts.MaxAttempts, c.maxAttempts)
		}
	}
}

// TestIngestArgsCarryTenantID is the AC teeth: job args carry tenant_id so the
// worker can open the right tenant.DB per job. Every kind serialises its tenant id
// through JSON (River stores args as JSON), so a round-trip must preserve it.
func TestArgsCarryTenantIDThroughJSON(t *testing.T) {
	const tid = "11111111-1111-1111-1111-111111111111"
	roundTrip := func(t *testing.T, in river.JobArgs, out river.JobArgs) string {
		t.Helper()
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal %T: %v", in, err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("unmarshal %T: %v", in, err)
		}
		return string(b)
	}

	ing := &IngestDocumentArgs{}
	raw := roundTrip(t, IngestDocumentArgs{TenantID: tid, ObjectKey: "k", SourceID: "s", ExternalID: "e"}, ing)
	if ing.TenantID != tid {
		t.Errorf("ingest tenant id lost: got %q from %s", ing.TenantID, raw)
	}

	sy := &SyncSourceArgs{}
	roundTrip(t, SyncSourceArgs{TenantID: tid, SourceID: "s", Full: true}, sy)
	if sy.TenantID != tid || sy.SourceID != "s" || !sy.Full {
		t.Errorf("sync args lost: %+v", sy)
	}
}

// TestSyncUniquenessAllowsResync: a sync_source job is unique by source while
// queued/running (SPEC-08 §1) but must NOT be blocked by a previously COMPLETED
// sync — a source is synced repeatedly. So ByState must exclude the completed
// state (River's default ByState includes it).
func TestSyncUniquenessAllowsResync(t *testing.T) {
	opts := SyncSourceArgs{}.InsertOpts()
	if !opts.UniqueOpts.ByArgs {
		t.Fatal("sync_source must be unique ByArgs (per source_id)")
	}
	for _, s := range opts.UniqueOpts.ByState {
		if s == rivertype.JobStateCompleted {
			t.Fatal("sync_source ByState must exclude 'completed' so a source can re-sync after a finished run")
		}
	}
	if len(opts.UniqueOpts.ByState) == 0 {
		t.Fatal("sync_source must set an explicit ByState (queued/running window), not the default")
	}
}
