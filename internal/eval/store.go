package eval

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// ErrNotFound is an unknown eval case (or a non-UUID id).
var ErrNotFound = errors.New("eval: case not found")

// Store is the tenant-content persistence port for eval_cases. Like the
// documents Store (ADR-0003, C-3) it is reached ONLY through a *tenant.DB: every
// method takes the resolved handle, so there is no way to touch another tenant's
// cases and no tenant_id filter — the database boundary is the tenant boundary
// (C-1). TenantStore implements it over a live tenant database; the SQL is
// exercised by the e2e suite (a *tenant.DB is unforgeable, so it is not mockable
// — that is the point). The struct mapping (scanCase) is unit-tested directly.
type Store interface {
	Create(ctx context.Context, db *tenant.DB, in CaseInput) (Case, error)
	Get(ctx context.Context, db *tenant.DB, id string) (Case, error)
	List(ctx context.Context, db *tenant.DB, limit int) ([]Case, error)
	Update(ctx context.Context, db *tenant.DB, id string, in CaseInput) (Case, error)
	// Delete removes a case; existed reports whether it was present (idempotent).
	Delete(ctx context.Context, db *tenant.DB, id string) (existed bool, err error)
	// Import applies validated inputs in one transaction: a row with an ID upserts,
	// a row without one inserts. All-or-nothing, so a mid-import failure writes
	// nothing.
	Import(ctx context.Context, db *tenant.DB, inputs []CaseInput) (ImportResult, error)
}

// TenantStore is the production Store over a tenant database.
type TenantStore struct{}

// NewTenantStore builds the tenant-schema store.
func NewTenantStore() TenantStore { return TenantStore{} }

// caseColumns is the shared eval_cases projection. expected_doc_ids is cast to
// text[] so it scans into a []string.
const caseColumns = `id::text, question, expected_answer, expected_doc_ids::text[], tags, created_at`

// rowScanner is the minimal pgx.Row surface scanCase needs; a plain interface so
// the mapping is unit-testable with a fake row.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanCase maps one eval_cases row into a Case. A NULL expected_answer scans to a
// nil *string; empty arrays scan to nil slices.
func scanCase(row rowScanner) (Case, error) {
	var (
		c    Case
		ans  *string
		docs []string
		tags []string
	)
	if err := row.Scan(&c.ID, &c.Question, &ans, &docs, &tags, &c.CreatedAt); err != nil {
		return Case{}, err
	}
	c.ExpectedAnswer = ans
	c.ExpectedDocIDs = docs
	c.Tags = tags
	return c, nil
}

// Create inserts a new case with a generated id and returns the stored row.
func (TenantStore) Create(ctx context.Context, db *tenant.DB, in CaseInput) (Case, error) {
	row := db.QueryRow(ctx, `
		insert into eval_cases (question, expected_answer, expected_doc_ids, tags)
		values ($1, $2, $3::uuid[], $4)
		returning `+caseColumns,
		in.Question, in.ExpectedAnswer, docIDsParam(in.ExpectedDocIDs), tagsParam(in.Tags))
	return scanCase(row)
}

// Get returns one case or ErrNotFound.
func (TenantStore) Get(ctx context.Context, db *tenant.DB, id string) (Case, error) {
	if !validUUID(id) {
		return Case{}, ErrNotFound
	}
	row := db.QueryRow(ctx, `select `+caseColumns+` from eval_cases where id = $1`, id)
	c, err := scanCase(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Case{}, ErrNotFound
	}
	return c, err
}

// List returns cases newest-first, capped at limit (or maxListCases).
func (TenantStore) List(ctx context.Context, db *tenant.DB, limit int) ([]Case, error) {
	if limit <= 0 || limit > maxListCases {
		limit = maxListCases
	}
	rows, err := db.Query(ctx,
		`select `+caseColumns+` from eval_cases order by created_at desc, id desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Case
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Update replaces the mutable fields of an existing case and returns it, or
// ErrNotFound when the id is unknown.
func (TenantStore) Update(ctx context.Context, db *tenant.DB, id string, in CaseInput) (Case, error) {
	if !validUUID(id) {
		return Case{}, ErrNotFound
	}
	row := db.QueryRow(ctx, `
		update eval_cases
		set question = $2, expected_answer = $3, expected_doc_ids = $4::uuid[], tags = $5
		where id = $1
		returning `+caseColumns,
		id, in.Question, in.ExpectedAnswer, docIDsParam(in.ExpectedDocIDs), tagsParam(in.Tags))
	c, err := scanCase(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Case{}, ErrNotFound
	}
	return c, err
}

// Delete removes a case. It is idempotent: a re-delete reports existed=false.
func (TenantStore) Delete(ctx context.Context, db *tenant.DB, id string) (bool, error) {
	if !validUUID(id) {
		return false, nil
	}
	tag, err := db.Exec(ctx, `delete from eval_cases where id = $1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Import applies inputs in one transaction (ADR-0069): a row with an ID upserts
// (insert with that id, or update on conflict), a row without one inserts a fresh
// case. On any error the whole transaction rolls back, so an import is atomic.
func (TenantStore) Import(ctx context.Context, db *tenant.DB, inputs []CaseInput) (ImportResult, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	var res ImportResult
	for _, in := range inputs {
		if in.ID == "" {
			if _, err := tx.Exec(ctx, `
				insert into eval_cases (question, expected_answer, expected_doc_ids, tags)
				values ($1, $2, $3::uuid[], $4)`,
				in.Question, in.ExpectedAnswer, docIDsParam(in.ExpectedDocIDs), tagsParam(in.Tags)); err != nil {
				return ImportResult{}, err
			}
			res.Created++
			continue
		}
		// Upsert by explicit id: xmax = 0 on the returned row means the row was
		// inserted (created), otherwise it was updated.
		var inserted bool
		err := tx.QueryRow(ctx, `
			insert into eval_cases (id, question, expected_answer, expected_doc_ids, tags)
			values ($1, $2, $3, $4::uuid[], $5)
			on conflict (id) do update
			set question = excluded.question,
			    expected_answer = excluded.expected_answer,
			    expected_doc_ids = excluded.expected_doc_ids,
			    tags = excluded.tags
			returning (xmax = 0)`,
			in.ID, in.Question, in.ExpectedAnswer, docIDsParam(in.ExpectedDocIDs), tagsParam(in.Tags)).
			Scan(&inserted)
		if err != nil {
			return ImportResult{}, err
		}
		if inserted {
			res.Created++
		} else {
			res.Updated++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ImportResult{}, err
	}
	return res, nil
}

// docIDsParam / tagsParam normalise nil slices to empty ones so the NOT NULL
// DEFAULT '{}' columns never receive a NULL and the pgx text[]/uuid[] encoding is
// unambiguous.
func docIDsParam(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func tagsParam(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}
