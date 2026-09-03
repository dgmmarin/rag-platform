package jobs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// Cursor is the opaque keyset position for List pagination: the (queued_at, id)
// of the last returned row. It is serialised into the SPEC-07 §1 next_cursor.
type Cursor struct {
	QueuedAt time.Time `json:"q"`
	ID       string    `json:"i"`
}

// Store is the control-plane persistence port for jobs. PoolDB implements it over
// the control-plane pool; tests use an in-memory fake. All methods are
// tenant-scoped (the tenant id is always an argument, never derived from the row)
// so a caller can never reach another tenant's jobs (FR-ACC-03).
type Store interface {
	List(ctx context.Context, tenantID string, flt ListFilter, limit int, cur *Cursor) ([]Job, error)
	Get(ctx context.Context, tenantID, id string) (Job, error)
	// CancelQueued flips a job from 'queued' to 'cancelled' in one guarded
	// statement. changed is false when the job is not (or no longer) queued or
	// does not exist; the service disambiguates with a re-read.
	CancelQueued(ctx context.Context, tenantID, id string) (job Job, changed bool, err error)
}

// ListParams selects a tenant's jobs with optional filters and keyset pagination.
type ListParams struct {
	TenantID string
	Filter   ListFilter
	Limit    int
	Cursor   string
}

// Page is the SPEC-07 §1 pagination envelope: {items, next_cursor}.
type Page struct {
	Items      []Job  `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Service is the jobs domain logic. It holds the Store and an optional Canceller
// (nil = the EPIC-09 River seam). It is stateless and safe for concurrent use.
type Service struct {
	Store     Store
	Canceller Canceller
}

// NewService builds a jobs service over the given store. Canceller is left nil
// (River lands in EPIC-09); set it to enable cooperative cancellation of running
// jobs.
func NewService(store Store) *Service { return &Service{Store: store} }

// List returns a tenant's jobs newest-first (by queued_at) with a next_cursor. It
// fails closed when no tenant is given, validates the status/kind filters, and
// rejects a malformed cursor.
func (s *Service) List(ctx context.Context, p ListParams) (Page, error) {
	if p.TenantID == "" {
		return Page{}, fmt.Errorf("jobs: tenant is required")
	}
	if p.Filter.Status != "" && !validStatuses[p.Filter.Status] {
		return Page{}, invalid("unknown status %q", p.Filter.Status)
	}
	if p.Filter.Kind != "" && !validKinds[p.Filter.Kind] {
		return Page{}, invalid("unknown kind %q", p.Filter.Kind)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	var cur *Cursor
	if p.Cursor != "" {
		c, err := decodeCursor(p.Cursor)
		if err != nil {
			return Page{}, invalid("invalid cursor")
		}
		cur = c
	}

	// Fetch one extra row to know whether a further page exists.
	rows, err := s.Store.List(ctx, p.TenantID, p.Filter, limit+1, cur)
	if err != nil {
		return Page{}, fmt.Errorf("jobs: list: %w", err)
	}
	page := Page{Items: rows}
	if len(rows) > limit {
		page.Items = rows[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(Cursor{QueuedAt: last.QueuedAt, ID: last.ID})
	}
	for i := range page.Items {
		page.Items[i] = page.Items[i].withDuration()
	}
	if page.Items == nil {
		page.Items = []Job{}
	}
	return page, nil
}

// Get returns one job scoped to the tenant, or ErrNotFound.
func (s *Service) Get(ctx context.Context, tenantID, id string) (Job, error) {
	if tenantID == "" {
		return Job{}, fmt.Errorf("jobs: tenant is required")
	}
	j, err := s.Store.Get(ctx, tenantID, id)
	if err != nil {
		return Job{}, err
	}
	return j.withDuration(), nil
}

// Cancel cancels a job (FR-ADM-02, SPEC-07 §2, SPEC-08 §4). A queued job is
// cancelled immediately (fully effective today — the mirror row is authoritative
// when no worker holds it). A running job is a cooperative signal the worker
// honours (SPEC-08 §4); until River is wired (EPIC-09) that returns the
// ErrCancelUnavailable seam. A terminal job returns ErrNotCancellable (409); an
// already-cancelled job is an idempotent no-op. See ADR-0031.
func (s *Service) Cancel(ctx context.Context, tenantID, id string) (Job, error) {
	if tenantID == "" {
		return Job{}, fmt.Errorf("jobs: tenant is required")
	}
	job, err := s.Store.Get(ctx, tenantID, id)
	if err != nil {
		return Job{}, err // ErrNotFound
	}
	if job.Status == StatusQueued {
		updated, changed, err := s.Store.CancelQueued(ctx, tenantID, id)
		if err != nil {
			return Job{}, fmt.Errorf("jobs: cancel: %w", err)
		}
		if changed {
			return updated.withDuration(), nil
		}
		// ponytail: narrow race — the job left 'queued' between the read and the
		// guarded update (only possible once EPIC-09's worker can claim jobs; no
		// worker exists today). Re-read the truth and fall through.
		job, err = s.Store.Get(ctx, tenantID, id)
		if err != nil {
			return Job{}, err
		}
	}
	return s.cancelNonQueued(ctx, tenantID, job)
}

// cancelNonQueued handles a job that is not (or no longer) queued.
func (s *Service) cancelNonQueued(ctx context.Context, tenantID string, job Job) (Job, error) {
	switch job.Status {
	case StatusCancelled:
		return job.withDuration(), nil // already cancelled — idempotent
	case StatusRunning:
		if s.Canceller == nil {
			return Job{}, ErrCancelUnavailable
		}
		if err := s.Canceller.Cancel(ctx, tenantID, job.ID); err != nil {
			return Job{}, fmt.Errorf("jobs: signal cancel: %w", err)
		}
		// The mirror-row transition running->cancelled is the worker's job
		// (SPEC-08 §3); the row is still running until the worker exits between
		// documents. The handler returns 202 (cancellation requested).
		return job.withDuration(), nil
	default: // succeeded, failed
		return Job{}, ErrNotCancellable
	}
}

// encodeCursor serialises a Cursor to an opaque base64url token.
func encodeCursor(c Cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses a base64url cursor token.
func decodeCursor(s string) (*Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c Cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
