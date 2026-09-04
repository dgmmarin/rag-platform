package webcrawl

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// Page is one row of persisted crawl state for a source (the tenant crawl_pages
// table, SPEC-03 §2). It carries what resumability and conditional fetch need:
// the URL and its normalised key, the BFS depth it was discovered at, whether it
// has been fetched, the last HTTP status, and the ETag/Last-Modified/content-hash
// captured for STORY-07.4's conditional requests.
type Page struct {
	URL           string
	NormalizedURL string
	Depth         int
	Fetched       bool
	Status        int
	ETag          string
	LastModified  string
	ContentHash   []byte
	Err           string
}

// PageStore persists per-source crawl state to make a crawl resumable (SPEC-04 §2,
// AC: "crawl state persisted; resumable"). Load returns everything known for the
// source keyed by normalised URL; Upsert writes one page's state.
//
// The crawler discovers its PageStore as an OPTIONAL capability of the SyncRun
// State object (see crawl.go / SPEC-04 §2): the connector interface (STORY-06.1)
// hands a connector only the generic key/value StateStore, and ADR-0003 forbids a
// connector opening its own pool, so the worker (EPIC-09) backs State with a
// tenant.DB object that also satisfies PageStore. When State does not implement
// PageStore the crawler runs against an in-memory store (correct, non-resumable).
type PageStore interface {
	Load(ctx context.Context) (map[string]Page, error)
	Upsert(ctx context.Context, p Page) error
}

// CrawlState is the SyncRun.State backing for a web-crawl source: the generic
// key/value StateStore the connector interface hands every connector (STORY-06.1),
// plus the crawler's PageStore capability. The worker (EPIC-09) sets an instance as
// SyncRun.State; the crawler discovers the PageStore half by type assertion.
type CrawlState interface {
	connector.StateStore
	PageStore
}

// --- in-memory store (unit tests; non-resumable fallback) -------------------

// memPageStore is an in-memory PageStore. It also satisfies connector.StateStore
// (Get/Set) so a test can pass it directly as SyncRun.State. The crawler's resume
// state lives entirely in the PageStore rows, so Get/Set are unused no-ops.
type memPageStore struct {
	mu    sync.Mutex
	pages map[string]Page
}

func newMemPageStore() *memPageStore { return &memPageStore{pages: map[string]Page{}} }

func (m *memPageStore) Load(_ context.Context) (map[string]Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Page, len(m.pages))
	for k, v := range m.pages {
		out[k] = v
	}
	return out, nil
}

func (m *memPageStore) Upsert(_ context.Context, p Page) error {
	m.mu.Lock()
	m.pages[p.NormalizedURL] = p
	m.mu.Unlock()
	return nil
}

// Get/Set satisfy connector.StateStore; the crawler needs no key/value cursor.
func (m *memPageStore) Get(context.Context, string) (string, bool, error) { return "", false, nil }
func (m *memPageStore) Set(context.Context, string, string) error         { return nil }

// --- tenant.DB-backed store (production; the EPIC-09 wiring) -----------------

// tenantPageStore persists crawl state to the tenant crawl_pages table through a
// resolved *tenant.DB — the only path to tenant data (ADR-0003, C-3). The tenant
// DB never holds a tenant_id column (C-1); source_id is an informational copy of a
// control-plane id (SPEC-03 §2 invariant 4). It also satisfies connector.StateStore
// so the worker can hand it in as SyncRun.State.
type tenantPageStore struct {
	db       *tenant.DB
	sourceID uuid.UUID
}

// NewTenantPageStore builds the crawl_pages-backed CrawlState for one source. The
// worker (EPIC-09) constructs it from the resolved tenant DB and sets it as
// SyncRun.State; the crawler discovers the PageStore capability at run time.
func NewTenantPageStore(db *tenant.DB, sourceID uuid.UUID) CrawlState {
	return &tenantPageStore{db: db, sourceID: sourceID}
}

func (s *tenantPageStore) Load(ctx context.Context) (map[string]Page, error) {
	rows, err := s.db.Query(ctx,
		`select url, normalized_url, depth, last_fetched_at is not null, coalesce(last_status,0),
		        coalesce(etag,''), coalesce(last_modified,''), content_hash, coalesce(error,'')
		   from crawl_pages where source_id = $1`, s.sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Page{}
	for rows.Next() {
		var p Page
		if err := rows.Scan(&p.URL, &p.NormalizedURL, &p.Depth, &p.Fetched, &p.Status,
			&p.ETag, &p.LastModified, &p.ContentHash, &p.Err); err != nil {
			return nil, err
		}
		out[p.NormalizedURL] = p
	}
	return out, rows.Err()
}

func (s *tenantPageStore) Upsert(ctx context.Context, p Page) error {
	// Bind nil for empty optionals so the columns stay NULL (a pending-frontier row
	// carries no status/etag/hash yet). last_fetched_at is set to now() when the
	// page was fetched, else left NULL so resume can tell fetched from pending.
	var status, etag, lastModified, errText any
	if p.Status != 0 {
		status = p.Status
	}
	if p.ETag != "" {
		etag = p.ETag
	}
	if p.LastModified != "" {
		lastModified = p.LastModified
	}
	if p.Err != "" {
		errText = p.Err
	}
	fetchedExpr := "null"
	if p.Fetched {
		fetchedExpr = "now()"
	}
	_, err := s.db.Exec(ctx, `
		insert into crawl_pages
		    (source_id, url, normalized_url, depth, last_fetched_at, last_status, etag, last_modified, content_hash, error)
		values ($1, $2, $3, $4, `+fetchedExpr+`, $5, $6, $7, $8, $9)
		on conflict (source_id, normalized_url) do update set
		    url             = excluded.url,
		    depth           = excluded.depth,
		    last_fetched_at = coalesce(excluded.last_fetched_at, crawl_pages.last_fetched_at),
		    last_status     = coalesce(excluded.last_status, crawl_pages.last_status),
		    etag            = coalesce(excluded.etag, crawl_pages.etag),
		    last_modified   = coalesce(excluded.last_modified, crawl_pages.last_modified),
		    content_hash    = coalesce(excluded.content_hash, crawl_pages.content_hash),
		    error           = excluded.error`,
		s.sourceID, p.URL, p.NormalizedURL, p.Depth, status, etag, lastModified, p.ContentHash, errText)
	return err
}

// Get/Set satisfy connector.StateStore; the crawler needs no key/value cursor.
func (s *tenantPageStore) Get(context.Context, string) (string, bool, error) { return "", false, nil }
func (s *tenantPageStore) Set(context.Context, string, string) error         { return nil }
