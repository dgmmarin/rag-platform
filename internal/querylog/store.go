package querylog

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// TenantStore is the production Store over a tenant database. Every method takes
// the resolved *tenant.DB (ADR-0003, C-3): the database boundary is the tenant
// boundary (C-1), so there is no tenant_id filter and no way to reach another
// tenant's rows.
type TenantStore struct{}

// NewTenantStore builds the tenant-schema store.
func NewTenantStore() TenantStore { return TenantStore{} }

// Insert writes one query_log row (FR-RET-09). The answer text is intentionally
// left null — the QueryLogger seam does not carry it. retrieved/citations are
// stored as jsonb.
func (TenantStore) Insert(ctx context.Context, db *tenant.DB, rec Record) error {
	retrieved, err := json.Marshal(rec.Retrieved)
	if err != nil {
		return err
	}
	citations, err := json.Marshal(rec.Citations)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `
		insert into query_log
			(id, request_id, question, retrieved, citations, llm_model,
			 retrieval_ms, generation_ms, in_tokens, out_tokens, grounded)
		values ($1, nullif($2, ''), $3, $4, $5, nullif($6, ''),
			$7, $8, $9, $10, $11)`,
		rec.ID, rec.RequestID, rec.Question, retrieved, citations, rec.LLMModel,
		rec.RetrievalMs, rec.GenerationMs, rec.InTokens, rec.OutTokens, rec.Grounded)
	return err
}

// UpsertFeedback records (or replaces) the feedback for a query (FR-RET-10). It
// first confirms the query exists in THIS tenant's query_log so an unknown id is a
// clean ErrQueryNotFound rather than a foreign-key error; then it upserts on the
// query_id primary key (last write wins).
func (TenantStore) UpsertFeedback(ctx context.Context, db *tenant.DB, fb Feedback) error {
	var exists bool
	if err := db.QueryRow(ctx, `select exists(select 1 from query_log where id = $1)`, fb.QueryID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrQueryNotFound
	}
	_, err := db.Exec(ctx, `
		insert into query_feedback (query_id, rating, comment)
		values ($1, $2, nullif($3, ''))
		on conflict (query_id) do update
			set rating = excluded.rating, comment = excluded.comment, created_at = now()`,
		fb.QueryID, fb.Rating, fb.Comment)
	return err
}

// List returns a page of query_log rows (newest first) with their joined feedback
// (SPEC-07 §2g). It reads limit rows after the keyset cursor (created_at, id).
func (TenantStore) List(ctx context.Context, db *tenant.DB, limit int, cur *Cursor) ([]Entry, error) {
	var curAt, curID any
	if cur != nil {
		curAt = cur.CreatedAt
		curID = cur.ID
	}
	rows, err := db.Query(ctx, `
		select l.id::text, l.question, coalesce(l.grounded, false),
		       l.retrieved, l.citations, coalesce(l.llm_model, ''),
		       coalesce(l.retrieval_ms, 0), coalesce(l.generation_ms, 0),
		       coalesce(l.in_tokens, 0), coalesce(l.out_tokens, 0), l.created_at,
		       f.rating, f.comment, f.created_at
		from query_log l
		left join query_feedback f on f.query_id = l.id
		where ($1::timestamptz is null
		       or l.created_at < $1::timestamptz
		       or (l.created_at = $1::timestamptz and l.id < $2::uuid))
		order by l.created_at desc, l.id desc
		limit $3`,
		curAt, curID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var (
			e         Entry
			id        string
			retrieved []byte
			citations []byte
			fbRating  *int
			fbComment *string
			fbCreated *time.Time
		)
		if err := rows.Scan(&id, &e.Question, &e.Grounded,
			&retrieved, &citations, &e.LLMModel,
			&e.RetrievalMs, &e.GenerationMs, &e.InTokens, &e.OutTokens, &e.CreatedAt,
			&fbRating, &fbComment, &fbCreated); err != nil {
			return nil, err
		}
		e.ID = idPrefix + id
		_ = json.Unmarshal(retrieved, &e.Retrieved)
		_ = json.Unmarshal(citations, &e.Citations)
		if e.Retrieved == nil {
			e.Retrieved = []RetrievedChunk{}
		}
		if e.Citations == nil {
			e.Citations = []string{}
		}
		if fbRating != nil {
			ef := &EntryFeedback{Rating: *fbRating}
			if fbComment != nil {
				ef.Comment = *fbComment
			}
			if fbCreated != nil {
				ef.CreatedAt = *fbCreated
			}
			e.Feedback = ef
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
