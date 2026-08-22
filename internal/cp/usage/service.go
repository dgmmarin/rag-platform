package usage

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// defaultWindowDays bounds a range-less read so one request cannot scan a tenant's
// entire history; it matches the "recent usage" the dashboard shows by default.
const defaultWindowDays = 30

// ErrInvalidRange is returned when a caller supplies an inverted [From, To] range.
// Handlers map it to a 400 rather than a 500.
var ErrInvalidRange = errors.New("usage: from is after to")

// Row is one usage_daily row as read back for the read API. Its fields map to the
// usage_daily columns (SPEC-02 §2).
type Row struct {
	TenantID       string    `json:"tenant_id"`
	Day            time.Time `json:"day"`
	Queries        int64     `json:"queries"`
	DocsIngested   int64     `json:"docs_ingested"`
	ChunksEmbedded int64     `json:"chunks_embedded"`
	EmbedTokens    int64     `json:"embed_tokens"`
	LLMInTokens    int64     `json:"llm_in_tokens"`
	LLMOutTokens   int64     `json:"llm_out_tokens"`
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

// ListParams selects a tenant's daily usage rows over an inclusive [From, To] day
// range. TenantID is required — the read is always scoped to one tenant so a
// caller can never fetch every tenant's usage unscoped. From/To are optional; when
// omitted the reader defaults to the last defaultWindowDays.
type ListParams struct {
	TenantID string
	From     time.Time
	To       time.Time
}

// Service reads usage_daily. It is stateless and safe for concurrent use.
type Service struct {
	DB  QueryDB
	now func() time.Time
}

// NewService builds a reader over the given DB.
func NewService(db QueryDB) *Service {
	return &Service{DB: db, now: time.Now}
}

// List returns a tenant's daily usage rows, newest day first, over the requested
// (or default) range. It fails closed if no tenant is given, and rejects an
// inverted range.
func (s *Service) List(ctx context.Context, p ListParams) ([]Row, error) {
	if p.TenantID == "" {
		return nil, fmt.Errorf("usage: tenant is required")
	}

	to := p.To
	if to.IsZero() {
		to = dayOf(s.now())
	}
	from := p.From
	if from.IsZero() {
		from = to.AddDate(0, 0, -defaultWindowDays)
	}
	if from.After(to) {
		return nil, fmt.Errorf("%w: from (%s) after to (%s)", ErrInvalidRange,
			from.Format("2006-01-02"), to.Format("2006-01-02"))
	}

	rows, err := s.DB.Query(ctx,
		`select tenant_id::text, day, queries, docs_ingested, chunks_embedded,
		        embed_tokens, llm_in_tokens, llm_out_tokens
		 from usage_daily
		 where tenant_id = $1 and day >= $2 and day <= $3
		 order by day desc`,
		p.TenantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("usage: list: %w", err)
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.TenantID, &r.Day, &r.Queries, &r.DocsIngested,
			&r.ChunksEmbedded, &r.EmbedTokens, &r.LLMInTokens, &r.LLMOutTokens); err != nil {
			return nil, fmt.Errorf("usage: scan row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: iterate rows: %w", err)
	}
	return out, nil
}
