// Package jobs is the control-plane jobs API subsystem (FR-ADM-02, SPEC-07 §2,
// SPEC-08). Jobs are control-plane tracking rows and live in the CONTROL PLANE
// (C-3: the `jobs` table is the history/mirror view of the queue, never tenant
// content), so this package operates on the control-plane `jobs` table via the
// control-plane pool — it never opens a tenant database (ADR-0003). Every
// operation is scoped to the tenant resolved from the authenticated principal
// (FR-ACC-03); there is no tenant_id request parameter.
//
// STORY-04.5 delivers the HTTP surface and the jobs store: list (with status/
// kind/source filters and keyset pagination), get, and cancel. Cancellation maps
// onto the SPEC-08 §4 model against what exists today (no worker yet):
//   - A QUEUED job is cancelled fully and immediately by flipping its mirror row
//     to `cancelled` in one guarded statement (SPEC-08 §4: "River cancels queued
//     jobs immediately"). No worker holds a queued row, so the mirror is
//     authoritative and this is complete now — it satisfies FR-ADM-02.
//   - A RUNNING job's cancel is COOPERATIVE: the worker observes the signal
//     between documents and exits with status `cancelled`, committing nothing
//     partial (SPEC-08 §4). The signal is a River operation and River lands in
//     EPIC-09, so it is an injected Canceller seam (nil today). Until it is
//     wired, cancelling a running job returns the not_found seam envelope
//     (mirroring STORY-04.3 /test and STORY-04.4 upload). The worker-side
//     honouring of the signal is EPIC-09 (STORY-09.1/09.4), and the mirror-row
//     transition running->cancelled is written by the worker middleware
//     (SPEC-08 §3), never by this API. See ADR-0031.
//   - A TERMINAL job (succeeded/failed) cannot be cancelled -> 409 conflict; an
//     already-cancelled job is idempotent (no error).
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// job_status values (schemas/control_plane.sql / SPEC-08). Kept as constants so
// the branch logic and filter validation reference one spelling.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Sentinel errors mapped to SPEC-07 §1 codes by the handlers.
var (
	// ErrNotFound: no such job for the resolved tenant. -> 404 not_found.
	ErrNotFound = errors.New("jobs: not found")
	// ErrNotCancellable: the job is in a terminal state (succeeded/failed). ->
	// 409 conflict.
	ErrNotCancellable = errors.New("jobs: not in a cancellable state")
	// ErrCancelUnavailable: cancelling a RUNNING job needs the River worker
	// (EPIC-09), which is not wired yet. -> not_found seam envelope (mirroring the
	// other EPIC-04 seams).
	ErrCancelUnavailable = errors.New("jobs: running-job cancellation not available")
)

// ValidationError is a client-facing input error carrying a safe message. The
// handlers surface it as 400 validation.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return "jobs: " + e.Msg }

func invalid(format string, a ...any) *ValidationError {
	return &ValidationError{Msg: fmt.Sprintf(format, a...)}
}

// validStatuses is the job_status enum; used to reject an unknown ?status filter.
var validStatuses = map[string]bool{
	StatusQueued: true, StatusRunning: true, StatusSucceeded: true,
	StatusFailed: true, StatusCancelled: true,
}

// validKinds is the job_kind enum (control_plane.sql / SPEC-08 §1); used to
// reject an unknown ?kind filter. Keep in step with the enum.
var validKinds = map[string]bool{
	"sync_source": true, "ingest_document": true, "reindex_tenant": true,
	"provision_tenant": true, "delete_tenant": true, "delete_source": true,
	"gc_tenant": true, "eval_run": true,
}

// Job is the public representation of a control-plane jobs row (the history/mirror
// view of the queue, SPEC-08 §3). It carries the status, timing and statistics
// FR-ADM-02 requires. DurationMS is computed for finished jobs (finished-started);
// a running job leaves it nil (the client derives elapsed time from started_at).
type Job struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenant_id"`
	SourceID    *string         `json:"source_id,omitempty"`
	Kind        string          `json:"kind"`
	Status      string          `json:"status"`
	Attempt     int             `json:"attempt"`
	MaxAttempts int             `json:"max_attempts"`
	Stats       json.RawMessage `json:"stats,omitempty"`
	Error       *string         `json:"error,omitempty"`
	QueuedAt    time.Time       `json:"queued_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
	WorkerID    *string         `json:"worker_id,omitempty"`
	DurationMS  *int64          `json:"duration_ms,omitempty"`
}

// withDuration fills DurationMS when the job has both a start and finish (a
// completed run), so the admin UI can show a duration without a second call
// (FR-ADM-02). A still-running job is left nil.
func (j Job) withDuration() Job {
	if j.StartedAt != nil && j.FinishedAt != nil {
		ms := j.FinishedAt.Sub(*j.StartedAt).Milliseconds()
		j.DurationMS = &ms
	}
	return j
}

// ListFilter narrows a job listing. Empty fields are ignored. Status/Kind are
// validated against the enums before reaching SQL.
type ListFilter struct {
	Status   string
	Kind     string
	SourceID string
}

// Canceller signals a RUNNING job to cancel cooperatively (SPEC-08 §4). It is the
// River integration seam (ADR-0005, EPIC-09 STORY-09.1/09.4): River requests the
// cancel and the worker middleware finalises the mirror row (SPEC-08 §3). It is
// nil until EPIC-09 wires it, so a running-job cancel fails closed as a seam.
type Canceller interface {
	Cancel(ctx context.Context, tenantID, jobID string) error
}
