// Package audit implements the control-plane audit log (FR-ADM-05, SPEC-02 §6):
// an append-only record of privileged actions and a platform-admin-only read API.
// Rows are never updated or deleted by the application; details carry non-secret
// metadata only (C-3), never credentials or setting values.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
)

// Event is one audit record to append. Action is required; every other field is
// optional so system/CLI actions (no user actor) and tenant-less platform actions
// are representable. Details is non-secret metadata; nil is stored as {}.
type Event struct {
	TenantID    *string
	ActorUserID *string
	ActorAPIKey *string
	Action      string
	TargetType  *string
	TargetID    *string
	Details     map[string]any
}

// commandTag exposes RowsAffected; pgconn.CommandTag satisfies it.
type commandTag interface {
	RowsAffected() int64
}

// ExecDB is the minimal write surface Record needs. A pgx pool adapter satisfies
// it, so a caller already holding a pool (or a transaction) can append a row.
type ExecDB interface {
	Exec(ctx context.Context, sql string, args ...any) (commandTag, error)
}

// Record appends one audit row. It validates that Action is set and marshals
// Details to a JSON object (nil → {}), then writes. The write is a single
// INSERT so a caller inside a transaction records the action atomically with
// the change it describes.
func Record(ctx context.Context, db ExecDB, e Event) error {
	if e.Action == "" {
		return fmt.Errorf("audit: action is required")
	}
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	blob, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("audit: marshal details for %q: %w", e.Action, err)
	}
	if _, err := db.Exec(ctx,
		`insert into audit_log
		   (tenant_id, actor_user_id, actor_api_key, action, target_type, target_id, details)
		 values ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		e.TenantID, e.ActorUserID, e.ActorAPIKey, e.Action, e.TargetType, e.TargetID, blob,
	); err != nil {
		return fmt.Errorf("audit: write %q: %w", e.Action, err)
	}
	return nil
}
