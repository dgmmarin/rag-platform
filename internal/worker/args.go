// Package worker is the River job worker (EPIC-09 STORY-09.1, ADR-0005,
// SPEC-08 §1). It is the consumer half of the platform's Postgres-backed job
// queue: River runs on the CONTROL-PLANE database, job args carry tenant_id, and
// each job opens a fresh tenant.DB from the resolver (ADR-0003) before dispatching
// to the handler an earlier epic already built (ingest_document → ingestdoc,
// sync_source → a connector's Sync into the ingestion sink).
//
// This file defines the typed River job args for every SPEC-08 §1 kind. Each kind
// is a JobArgs struct whose Kind() is the exact enum value in
// schemas/control_plane.sql, and whose InsertOpts() pins its queue, retry budget
// and uniqueness. Producers (the sources/documents HTTP handlers) enqueue these
// via a River client; today those handlers still write the control-plane `jobs`
// table directly and STORY-09.2 switches them to River + the mirror — see the ADR.
//
// Queues (SPEC-08 §1): ingest / maintenance / platform each get their own worker
// concurrency so a long reindex on `maintenance` cannot starve syncs on `ingest`.
package worker

import (
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Queue names (SPEC-08 §1). Each has independent worker concurrency.
const (
	QueueIngest      = "ingest"
	QueueMaintenance = "maintenance"
	QueuePlatform    = "platform"
)

// syncActiveStates is the ByState window for sync_source uniqueness: "one active
// sync per source while queued/running" (SPEC-08 §1). It deliberately EXCLUDES
// completed/cancelled/discarded so a source can be re-synced after a finished run
// (River's default ByState includes completed, which would wrongly block re-syncs
// until the completed job is reaped).
var syncActiveStates = []rivertype.JobState{
	rivertype.JobStatePending,
	rivertype.JobStateScheduled,
	rivertype.JobStateAvailable,
	rivertype.JobStateRunning,
	rivertype.JobStateRetryable,
}

// IngestDocumentArgs runs the ingest pipeline for one uploaded document
// (SPEC-08 §1: args tenant_id, document_id, raw_ref; queue ingest; unique by
// document; retries 3). The JSON shape mirrors the control-plane jobs.payload
// documents.Service.Ingest writes, so ingestdoc.JobFromPayload can decode it — with
// tenant_id added as a top-level arg (the jobs row keeps tenant_id in a column;
// River keeps it in the args). SourceID+ExternalID form the document identity
// (documents are keyed by (source_id, external_id)); tagging them `river:"unique"`
// makes ByArgs uniqueness "by document".
type IngestDocumentArgs struct {
	TenantID   string `json:"tenant_id"`
	SourceID   string `json:"source_id" river:"unique"`
	ObjectKey  string `json:"object_key"`
	ExternalID string `json:"external_id" river:"unique"`
	Filename   string `json:"filename"`
	MimeType   string `json:"mime_type"`
}

// Kind implements river.JobArgs.
func (IngestDocumentArgs) Kind() string { return "ingest_document" }

// InsertOpts pins the ingest queue, a 3-attempt budget and by-document uniqueness.
func (IngestDocumentArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueIngest,
		MaxAttempts: 3,
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// SyncSourceArgs enumerates one source into the ingestion sink (SPEC-08 §1: args
// tenant_id, source_id, full; queue ingest; unique by source while queued/running;
// retries 3, backoff 1m/5m/30m). Only source_id is tagged unique so a `full` vs
// incremental variant does not slip past the one-active-sync-per-source rule.
type SyncSourceArgs struct {
	TenantID string `json:"tenant_id"`
	SourceID string `json:"source_id" river:"unique"`
	Full     bool   `json:"full"`
}

// Kind implements river.JobArgs.
func (SyncSourceArgs) Kind() string { return "sync_source" }

// InsertOpts pins the ingest queue, a 3-attempt budget and one-active-sync-per-
// source uniqueness (ByArgs on source_id, restricted to the queued/running window).
func (SyncSourceArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueIngest,
		MaxAttempts: 3,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: syncActiveStates},
	}
}

// syncBackoff is the SPEC-08 §1 sync retry schedule (1m, 5m, 30m). The sync worker
// overrides River's default exponential policy with this via NextRetry.
var syncBackoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

// ReindexTenantArgs reindexes a tenant's corpus with a table swap (SPEC-08 §1:
// queue maintenance; unique by tenant; retries 5, resumable cursor). STORY-05.8
// built the resumable Reindexer; wiring the multi-step orchestration is a TODO on
// the worker (see reindexWorker).
type ReindexTenantArgs struct {
	TenantID string `json:"tenant_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (ReindexTenantArgs) Kind() string { return "reindex_tenant" }

// InsertOpts pins the maintenance queue, a 5-attempt budget and per-tenant uniqueness.
func (ReindexTenantArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 5, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// GCTenantArgs runs a tenant's garbage collection (SPEC-08 §1: queue maintenance;
// unique by tenant; retries 1, daily). Wired to documents.TenantStore.CollectGarbage.
type GCTenantArgs struct {
	TenantID string `json:"tenant_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (GCTenantArgs) Kind() string { return "gc_tenant" }

// InsertOpts pins the maintenance queue, a single attempt and per-tenant uniqueness.
func (GCTenantArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// DeleteSourceArgs removes a source's documents/versions/chunks/crawl state
// (SPEC-08 §1: queue maintenance; unique by source; retries 5). The handler is
// STORY-09.6; registered here as a TODO worker so the queue structure is complete.
type DeleteSourceArgs struct {
	TenantID string `json:"tenant_id"`
	SourceID string `json:"source_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (DeleteSourceArgs) Kind() string { return "delete_source" }

// InsertOpts pins the maintenance queue, a 5-attempt budget and per-source uniqueness.
func (DeleteSourceArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 5, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// ProvisionTenantArgs provisions a tenant asynchronously (SPEC-08 §1: queue
// platform; unique by tenant; retries 5). The synchronous provisioner exists
// (STORY-02.3); the async handler is a TODO worker here.
type ProvisionTenantArgs struct {
	TenantID string `json:"tenant_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (ProvisionTenantArgs) Kind() string { return "provision_tenant" }

// InsertOpts pins the platform queue, a 5-attempt budget and per-tenant uniqueness.
func (ProvisionTenantArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueuePlatform, MaxAttempts: 5, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// DeleteTenantArgs tears a tenant down asynchronously (SPEC-08 §1: queue platform;
// unique by tenant; retries 10). The synchronous lifecycle exists (STORY-02.4); the
// async handler is a TODO worker here.
type DeleteTenantArgs struct {
	TenantID string `json:"tenant_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (DeleteTenantArgs) Kind() string { return "delete_tenant" }

// InsertOpts pins the platform queue, a 10-attempt budget and per-tenant uniqueness.
func (DeleteTenantArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueuePlatform, MaxAttempts: 10, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}
