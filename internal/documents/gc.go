package documents

import (
	"context"
	"time"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// This file is the tenant-side SQL for the retention garbage-collection sweep
// (STORY-05.9, SPEC-03 §4, the SPEC-08 `gc_tenant` job). Like the reindex swap
// (reindex.go) it lives beside Put/TouchIfUnchanged because it is the SAME tenant
// content on the SAME tables, reached ONLY through a *tenant.DB (ADR-0003, C-1,
// C-3): the database boundary is the tenant boundary, so there is no tenant_id and
// no cross-tenant path. The River worker that schedules it daily (STORY-09.1) is out
// of scope; CollectGarbage is the operation the worker drives and whose returned
// metrics it emits (SPEC-10). Deliberately NOT on the documents.Store interface — the
// Service/handlers never GC (ISP, mirroring the reindex methods, ADR-0038).

// SPEC-03 §4 retention windows. The three time-based ones are the SPEC's literal
// day figures; the crawl-page rule is stated in syncs, not time (see GCPolicy).
const (
	defaultVersionRetention    = 30 * 24 * time.Hour // non-current document_versions
	defaultDeletedDocRetention = 30 * 24 * time.Hour // status='deleted' documents
	defaultQueryLogRetention   = 90 * 24 * time.Hour // query_log rows
	defaultGCBatchSize         = 1000                // rows removed per statement per class
)

// GCPolicy holds the SPEC-03 §4 retention windows and the delete batch cap. A
// zero-valued time field falls back to the SPEC default, EXCEPT CrawlPageStale.
//
// SPEC-03 §4 phrases the crawl-page rule as "not seen in 3 successful syncs" — a
// count of crawl generations, not a duration. The tenant schema records only
// crawl_pages.last_fetched_at (there is no per-source sync-generation counter, and
// the crawler that would emit one is EPIC-06/07, not yet built), so GC approximates
// "not seen in N syncs" as "not fetched within CrawlPageStale". The driving worker
// (STORY-09.1) knows a source's crawl cadence and supplies 3×interval. A zero
// CrawlPageStale SKIPS the crawl sweep rather than inventing a window — deleting
// frontier state on a guessed threshold is not a policy this story gets to make.
//
// ponytail: crawl-page collection is time-based, not the SPEC's literal 3-sync
// count. Ceiling: a source whose sync cadence changes could over/under-collect
// frontier rows. Upgrade path: add crawl_pages.last_seen_sync + a per-source sync
// counter when the crawler lands, and switch the predicate to a generation delta.
type GCPolicy struct {
	VersionRetention    time.Duration // delete non-current document_versions older than this
	DeletedDocRetention time.Duration // delete status='deleted' documents older than this
	QueryLogRetention   time.Duration // delete query_log rows older than this
	CrawlPageStale      time.Duration // delete crawl_pages unfetched for longer than this (0 = skip)
	BatchSize           int           // max rows removed per statement per class (bounds lock time)
}

// withDefaults fills unset time windows with the SPEC-03 §4 defaults and clamps the
// batch size to a positive cap. CrawlPageStale is intentionally left as-is (0 = skip).
func (p GCPolicy) withDefaults() GCPolicy {
	if p.VersionRetention <= 0 {
		p.VersionRetention = defaultVersionRetention
	}
	if p.DeletedDocRetention <= 0 {
		p.DeletedDocRetention = defaultDeletedDocRetention
	}
	if p.QueryLogRetention <= 0 {
		p.QueryLogRetention = defaultQueryLogRetention
	}
	if p.BatchSize <= 0 {
		p.BatchSize = defaultGCBatchSize
	}
	return p
}

// GCMetrics is rows removed per SPEC-03 §4 retention class, the "metrics on rows
// removed" the story requires. Chunks is the number of chunks removed by cascade
// from the version and deleted-document sweeps — informational, not a class of its
// own, so it is excluded from Total.
type GCMetrics struct {
	OldVersions int64 `json:"old_versions"`
	DeletedDocs int64 `json:"deleted_docs"`
	QueryLogs   int64 `json:"query_logs"`
	CrawlPages  int64 `json:"crawl_pages"`
	Chunks      int64 `json:"chunks"`
}

// Total is the number of top-level rows removed across the four retention classes.
func (m GCMetrics) Total() int64 {
	return m.OldVersions + m.DeletedDocs + m.QueryLogs + m.CrawlPages
}

// CollectGarbage runs the SPEC-03 §4 retention sweep over one tenant and returns the
// rows removed per class. It is idempotent and safe to re-run: a second run with the
// same now removes nothing new. Each class is drained in BatchSize-bounded
// statements so a huge backlog is removed in many small transactions rather than one
// table-locking delete; between batches the lock is released.
//
// now is injected for testability; the worker passes time.Now(). All four cutoffs
// are now - window.
//
// ponytail: one CollectGarbage call drains the full eligible backlog in
// BatchSize-bounded steps. Ceiling: a pathologically large backlog means many
// iterations in a single call. Upgrade path: cap iterations and return a
// "more-remaining" flag for the worker to reschedule across job runs.
func (s TenantStore) CollectGarbage(ctx context.Context, db *tenant.DB, p GCPolicy, now time.Time) (GCMetrics, error) {
	p = p.withDefaults()
	var m GCMetrics

	// Old non-current document versions (+ their chunks by cascade).
	if err := drain(func() (int64, error) {
		rows, chunks, err := s.gcOldVersionsBatch(ctx, db, now.Add(-p.VersionRetention), p.BatchSize)
		m.OldVersions += rows
		m.Chunks += chunks
		return rows, err
	}, p.BatchSize); err != nil {
		return m, err
	}

	// Soft-deleted documents past grace (+ their versions and chunks by cascade).
	if err := drain(func() (int64, error) {
		rows, chunks, err := s.gcDeletedDocsBatch(ctx, db, now.Add(-p.DeletedDocRetention), p.BatchSize)
		m.DeletedDocs += rows
		m.Chunks += chunks
		return rows, err
	}, p.BatchSize); err != nil {
		return m, err
	}

	// Query logs past retention (feedback cascades).
	if err := drain(func() (int64, error) {
		rows, err := s.gcQueryLogBatch(ctx, db, now.Add(-p.QueryLogRetention), p.BatchSize)
		m.QueryLogs += rows
		return rows, err
	}, p.BatchSize); err != nil {
		return m, err
	}

	// Stale crawl pages — only when a window is configured (0 = skip, see GCPolicy).
	if p.CrawlPageStale > 0 {
		if err := drain(func() (int64, error) {
			rows, err := s.gcCrawlPagesBatch(ctx, db, now.Add(-p.CrawlPageStale), p.BatchSize)
			m.CrawlPages += rows
			return rows, err
		}, p.BatchSize); err != nil {
			return m, err
		}
	}

	return m, nil
}

// drain runs step (one bounded batch) repeatedly until a batch removes fewer than
// batchSize rows — i.e. the class is exhausted. A full batch means more may remain.
func drain(step func() (int64, error), batchSize int) error {
	for {
		n, err := step()
		if err != nil {
			return err
		}
		if n < int64(batchSize) {
			return nil
		}
	}
}

// gcOldVersionsBatch deletes up to limit non-current document versions older than
// cutoff and returns (versions removed, chunks removed by cascade). "Non-current" is
// any version no document points at via current_version, so the data-model invariant
// (an active document always has a live current_version, SPEC-03 §2.1) is preserved:
// a current version is never a victim. The chunk count is taken before the delete, in
// the same statement's snapshot, so it counts exactly the rows the version cascade
// removes (chunks.version_id ON DELETE CASCADE).
func (TenantStore) gcOldVersionsBatch(ctx context.Context, db *tenant.DB, cutoff time.Time, limit int) (int64, int64, error) {
	var versions, chunks int64
	err := db.QueryRow(ctx, `
		with victims as (
			select v.id
			from document_versions v
			where v.created_at < $1
			  and not exists (select 1 from documents d where d.current_version = v.id)
			order by v.created_at
			limit $2
		),
		chunk_count as (
			select count(*) as c from chunks where version_id in (select id from victims)
		),
		deleted as (
			delete from document_versions where id in (select id from victims) returning 1
		)
		select (select count(*) from deleted), (select c from chunk_count)`,
		cutoff, limit).Scan(&versions, &chunks)
	return versions, chunks, err
}

// gcDeletedDocsBatch deletes up to limit soft-deleted documents whose deleted_at is
// older than cutoff and returns (documents removed, chunks removed by cascade).
// Deleting the document cascades its versions and chunks (both ON DELETE CASCADE on
// document_id); the deferrable current_version FK holds because the version rows go
// in the same statement.
func (TenantStore) gcDeletedDocsBatch(ctx context.Context, db *tenant.DB, cutoff time.Time, limit int) (int64, int64, error) {
	var docs, chunks int64
	err := db.QueryRow(ctx, `
		with victims as (
			select id
			from documents
			where status = 'deleted' and deleted_at is not null and deleted_at < $1
			order by deleted_at
			limit $2
		),
		chunk_count as (
			select count(*) as c from chunks where document_id in (select id from victims)
		),
		deleted as (
			delete from documents where id in (select id from victims) returning 1
		)
		select (select count(*) from deleted), (select c from chunk_count)`,
		cutoff, limit).Scan(&docs, &chunks)
	return docs, chunks, err
}

// gcQueryLogBatch deletes up to limit query_log rows older than cutoff (feedback
// cascades on query_id) and returns rows removed.
func (TenantStore) gcQueryLogBatch(ctx context.Context, db *tenant.DB, cutoff time.Time, limit int) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, `
		with victims as (
			select id from query_log where created_at < $1 order by created_at limit $2
		),
		deleted as (
			delete from query_log where id in (select id from victims) returning 1
		)
		select count(*) from deleted`,
		cutoff, limit).Scan(&n)
	return n, err
}

// gcCrawlPagesBatch deletes up to limit crawl_pages not fetched since cutoff and
// returns rows removed. Pages with a null last_fetched_at are pending frontier
// entries (never fetched), not abandoned ones, so they are left in place.
func (TenantStore) gcCrawlPagesBatch(ctx context.Context, db *tenant.DB, cutoff time.Time, limit int) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, `
		with victims as (
			select source_id, normalized_url
			from crawl_pages
			where last_fetched_at is not null and last_fetched_at < $1
			order by last_fetched_at
			limit $2
		),
		deleted as (
			delete from crawl_pages c
			using victims v
			where c.source_id = v.source_id and c.normalized_url = v.normalized_url
			returning 1
		)
		select count(*) from deleted`,
		cutoff, limit).Scan(&n)
	return n, err
}
