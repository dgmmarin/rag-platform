package documents

import (
	"context"
	"time"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// TouchIfUnchanged realises the SPEC-05 §1 "compare (== current version hash?)"
// short-circuit as ONE atomic statement so the sink can skip chunk+embed when a
// document has not changed — a hash match costs no embedding (FR-ING-07).
//
// It touches last_seen_at (so a full-sync Complete will not sweep the document)
// and, if the document had been soft-deleted and reappears byte-identical, it is
// reactivated — the same restore Put performs on a changed reappearance, but
// without re-embedding. It returns unchanged=true only when the document exists,
// has a current version, and that version's content_hash equals hash. A brand-new
// or genuinely-changed document matches no row (unchanged=false) and the caller
// proceeds to chunk → embed → Put.
//
// Reached only through a *tenant.DB from the resolver (ADR-0003, C-3): the
// database boundary is the tenant boundary, so there is no tenant_id filter.
func (TenantStore) TouchIfUnchanged(ctx context.Context, db *tenant.DB, sourceID, externalID string, hash []byte) (bool, error) {
	if !validUUID(sourceID) {
		return false, invalid("source_id must be a UUID")
	}
	if externalID == "" {
		return false, invalid("external_id is required")
	}
	if len(hash) == 0 {
		return false, invalid("content_hash is required")
	}
	tag, err := db.Exec(ctx, `
		update documents d
		set last_seen_at = now(), status = 'active', deleted_at = null
		from document_versions v
		where d.source_id = $1::uuid
		  and d.external_id = $2
		  and v.id = d.current_version
		  and v.content_hash = $3`,
		sourceID, externalID, hash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SoftDeleteUnseen implements Sink.Complete for a FULL sync (SPEC-05 §1/§5): every
// still-active document of the source whose last_seen_at predates the run's
// startedAt was not re-seen this run, so it is marked 'deleted' (retrieval's
// live_chunks view already hides non-active documents). It returns how many rows
// it flipped, which the sink folds into jobs.stats.docs_deleted.
//
// Documents Put/TouchIfUnchanged touched this run carry last_seen_at >= startedAt
// (now() advances past the captured start) and are preserved; the sweep is a
// single statement, so a crash before it runs simply leaves the previous set live
// until the next full sync. It is INCREMENTAL-safe by omission: the sink calls it
// only on a full sync (never on an incremental one).
func (TenantStore) SoftDeleteUnseen(ctx context.Context, db *tenant.DB, sourceID string, startedAt time.Time) (int, error) {
	if !validUUID(sourceID) {
		return 0, invalid("source_id must be a UUID")
	}
	if startedAt.IsZero() {
		return 0, invalid("started_at is required")
	}
	tag, err := db.Exec(ctx, `
		update documents
		set status = 'deleted', deleted_at = now()
		where source_id = $1::uuid
		  and last_seen_at < $2
		  and status <> 'deleted'`,
		sourceID, startedAt)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
