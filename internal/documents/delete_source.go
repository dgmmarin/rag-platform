package documents

import (
	"context"
	"fmt"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// DeleteSourceStats reports what a delete_source job removed (FR-SRC-12, SPEC-08 §1).
// The JSON tags land straight into jobs.stats via the worker's stats sink.
type DeleteSourceStats struct {
	Documents  int64 `json:"documents"`
	Chunks     int64 `json:"chunks"`
	CrawlPages int64 `json:"crawl_pages"`
}

// DeleteSource removes ALL of a source's tenant content — documents (with their
// versions and chunks by ON DELETE CASCADE), crawl-page state, connector state and
// products — in one transaction, and returns the counts (FR-SRC-12). Running it again
// is a no-op (idempotent), so the delete_source job is safe to retry.
//
// ponytail: single unbatched DELETEs, so a pathologically large source holds row locks
// for the duration of the transaction. Ceiling: one very large source. Upgrade path:
// batch like CollectGarbage and report a more-remaining flag for the worker to
// reschedule.
func (TenantStore) DeleteSource(ctx context.Context, db *tenant.DB, sourceID string) (DeleteSourceStats, error) {
	var st DeleteSourceStats
	tx, err := db.Begin(ctx)
	if err != nil {
		return st, fmt.Errorf("delete_source: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Chunks cascade with their documents; count them before the delete removes them.
	if err := tx.QueryRow(ctx, `select count(*) from chunks where source_id = $1`, sourceID).Scan(&st.Chunks); err != nil {
		return st, fmt.Errorf("delete_source: count chunks: %w", err)
	}
	// Documents cascade to document_versions and chunks (schemas/tenant.sql FKs). This
	// also nulls products.document_id (ON DELETE SET NULL); products are then removed by
	// source below.
	tag, err := tx.Exec(ctx, `delete from documents where source_id = $1`, sourceID)
	if err != nil {
		return st, fmt.Errorf("delete_source: documents: %w", err)
	}
	st.Documents = tag.RowsAffected()

	tag, err = tx.Exec(ctx, `delete from crawl_pages where source_id = $1`, sourceID)
	if err != nil {
		return st, fmt.Errorf("delete_source: crawl_pages: %w", err)
	}
	st.CrawlPages = tag.RowsAffected()

	if _, err := tx.Exec(ctx, `delete from connector_state where source_id = $1`, sourceID); err != nil {
		return st, fmt.Errorf("delete_source: connector_state: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from products where source_id = $1`, sourceID); err != nil {
		return st, fmt.Errorf("delete_source: products: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return st, fmt.Errorf("delete_source: commit: %w", err)
	}
	return st, nil
}
