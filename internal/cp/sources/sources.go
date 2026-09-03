// Package sources is the control-plane sources API subsystem (FR-SRC-01/14,
// SPEC-04, SPEC-07 §2). Sources are registry data and live in the CONTROL PLANE
// (C-3: the control plane holds the source definitions; tenant content never
// does), so this package operates on the control-plane `sources` and `jobs`
// tables via the control-plane pool — it never opens a tenant database. Every
// operation is scoped to the tenant resolved from the authenticated principal
// (FR-ACC-03); there is no tenant_id request parameter.
//
// STORY-04.3 delivers the HTTP surface, the source store, and manual sync /
// delete enqueue against the real `jobs` table (the partial unique index
// `jobs_one_active_sync_per_source` gives the SPEC-07 §2 "409 if one already
// active" for free). Two pieces belong to later epics and are injected as seams:
//   - the connector framework (SPEC-04 §1, EPIC-06 STORY-06.1) validates
//     kind-specific config and runs Connector.Test — the Validator port; nil
//     until EPIC-06 wires it, in which case /test reports the seam envelope and
//     create/update skip kind-specific validation (generic validation still
//     applies).
//   - the River-backed worker that actually executes sync_source/delete_source
//     jobs (ADR-0005, EPIC-09). This package only writes the queued mirror row
//     the worker will consume; the actual dispatch and the FR-SRC-12 cascade
//     removal are EPIC-09 (STORY-09.1/09.6).
package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel errors mapped to SPEC-07 §1 codes by the handlers.
var (
	// ErrNotFound: no such source for the resolved tenant. -> 404 not_found.
	ErrNotFound = errors.New("sources: not found")
	// ErrDuplicateName: (tenant, name) already exists. -> 409 conflict.
	ErrDuplicateName = errors.New("sources: duplicate name")
	// ErrActiveSyncExists: a sync for this source is already queued or running.
	// -> 409 conflict (SPEC-07 §2 "409 if one already active").
	ErrActiveSyncExists = errors.New("sources: an active sync already exists")
	// ErrConnectorUnavailable: the connector framework (EPIC-06) is not wired, so
	// a "test connection" cannot run yet. -> not_found seam envelope.
	ErrConnectorUnavailable = errors.New("sources: connector framework not available")
)

// ValidationError is a client-facing input error carrying a safe message. The
// handlers surface it as 400 validation.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return "sources: " + e.Msg }

func invalid(format string, a ...any) *ValidationError {
	return &ValidationError{Msg: fmt.Sprintf(format, a...)}
}

// validKinds is the source_kind enum (control_plane.sql / SPEC-04 §1). Keep in
// step with the enum; adding a kind means a control-plane migration + this set.
var validKinds = map[string]bool{
	"upload":    true,
	"web_crawl": true,
	"api":       true,
	"s3":        true,
	"sitemap":   true,
}

// apiSettableStatuses are the statuses an admin may set through the API. `deleting`
// and `error` are system-managed (delete flow / sync failures), never client-set.
var apiSettableStatuses = map[string]bool{
	"active": true,
	"paused": true,
}

const maxNameLen = 200

// Source is the public representation of a control-plane sources row. Credentials
// (`credentials_enc`) are deliberately absent: they are never returned by any API
// after creation (FR-SRC-10).
type Source struct {
	ID            string          `json:"id"`
	TenantID      string          `json:"tenant_id"`
	Kind          string          `json:"kind"`
	Name          string          `json:"name"`
	Status        string          `json:"status"`
	Config        json.RawMessage `json:"config"`
	ScheduleCron  *string         `json:"schedule_cron,omitempty"`
	NextRunAt     *time.Time      `json:"next_run_at,omitempty"`
	LastRunAt     *time.Time      `json:"last_run_at,omitempty"`
	LastSuccessAt *time.Time      `json:"last_success_at,omitempty"`
	LastError     *string         `json:"last_error,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// Job is the public representation of a queued control-plane jobs row returned by
// sync/delete. It is the history/mirror view the worker (EPIC-09) will consume.
type Job struct {
	ID       string          `json:"id"`
	TenantID string          `json:"tenant_id"`
	SourceID *string         `json:"source_id,omitempty"`
	Kind     string          `json:"kind"`
	Status   string          `json:"status"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	QueuedAt time.Time       `json:"queued_at"`
}

// Validator is the connector-framework hook (SPEC-04 §1, EPIC-06 STORY-06.1). It
// validates kind-specific config and runs the connector's "test connection". It
// is injected into Service; nil means the framework is not wired yet (the seam).
// EPIC-06 will supply the concrete registry (and extend Test with decrypted
// Credentials, STORY-06.2) without changing this package's HTTP surface.
type Validator interface {
	ValidateConfig(kind string, config json.RawMessage) error
	Test(ctx context.Context, kind string, config json.RawMessage) error
}

// validateKind checks a source kind against the enum.
func validateKind(kind string) error {
	if !validKinds[kind] {
		return invalid("unknown source kind %q", kind)
	}
	return nil
}

// validateName checks a source name is non-blank and within bounds.
func validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return invalid("name is required")
	}
	if len(name) > maxNameLen {
		return invalid("name must be at most %d characters", maxNameLen)
	}
	return nil
}

// validateConfigShape ensures config, when present, is a JSON object (the
// kind-specific connector config; deeper validation is the connector's job).
func validateConfigShape(cfg json.RawMessage) error {
	if len(cfg) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(cfg))
	if !strings.HasPrefix(trimmed, "{") {
		return invalid("config must be a JSON object")
	}
	var obj map[string]any
	if err := json.Unmarshal(cfg, &obj); err != nil {
		return invalid("config must be a JSON object")
	}
	return nil
}
