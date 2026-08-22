package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const (
	// defaultLimit is the page size when a caller does not specify one.
	defaultLimit = 50
	// maxLimit caps the page size so one request cannot scan an unbounded slice
	// of a busy tenant's history.
	maxLimit = 200
)

// Entry is one audit_log row as read back for the admin API. Details is the
// decoded JSON metadata object.
type Entry struct {
	ID          int64          `json:"id"`
	TenantID    *string        `json:"tenant_id,omitempty"`
	ActorUserID *string        `json:"actor_user_id,omitempty"`
	ActorAPIKey *string        `json:"actor_api_key,omitempty"`
	Action      string         `json:"action"`
	TargetType  *string        `json:"target_type,omitempty"`
	TargetID    *string        `json:"target_id,omitempty"`
	Details     map[string]any `json:"details"`
	CreatedAt   time.Time      `json:"created_at"`
}

// Rows is the minimal multi-row surface the reader scans; pgx.Rows satisfies it.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// QueryDB is the minimal read surface Service needs; a pgx pool adapter satisfies it.
type QueryDB interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
}

// ListParams selects a page of a tenant's audit history. TenantID is required —
// the audit read is always scoped to one tenant (the platform admin names it as a
// query parameter, FR-ADM-05). BeforeID (>0) pages backwards through the
// newest-first ordering: pass the ID of the last entry of the previous page.
type ListParams struct {
	TenantID string
	Limit    int
	BeforeID int64
}

// Service reads the audit log. It is stateless and safe for concurrent use.
type Service struct {
	DB QueryDB
}

// NewService builds a reader over the given DB.
func NewService(db QueryDB) *Service { return &Service{DB: db} }

// List returns a page of a tenant's audit entries, newest first. It fails closed
// if no tenant is given so a caller can never fetch the whole log unscoped.
func (s *Service) List(ctx context.Context, p ListParams) ([]Entry, error) {
	if p.TenantID == "" {
		return nil, fmt.Errorf("audit: tenant is required")
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	rows, err := s.DB.Query(ctx,
		`select id, tenant_id::text, actor_user_id::text, actor_api_key::text,
		        action, target_type, target_id::text, details, created_at
		 from audit_log
		 where tenant_id = $1 and ($2 = 0 or id < $2)
		 order by id desc
		 limit $3`,
		p.TenantID, p.BeforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var details []byte
		if err := rows.Scan(&e.ID, &e.TenantID, &e.ActorUserID, &e.ActorAPIKey,
			&e.Action, &e.TargetType, &e.TargetID, &details, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("audit: scan entry: %w", err)
		}
		if len(details) > 0 {
			if err := json.Unmarshal(details, &e.Details); err != nil {
				return nil, fmt.Errorf("audit: decode details: %w", err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: iterate entries: %w", err)
	}
	return out, nil
}
