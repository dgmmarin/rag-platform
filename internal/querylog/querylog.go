// Package querylog persists the query log and its user feedback (FR-RET-09/10,
// STORY-08.8). It fills the QueryLogger seam left by internal/answer (STORY-08.5)
// and serves the feedback endpoint (POST /v1/feedback, query scope) and the admin
// query-log listing (GET /v1/queries, admin scope) of SPEC-07 §2/§2g.
//
// The query_log and query_feedback tables are TENANT CONTENT (C-3): they are
// reached ONLY through a *tenant.DB from the resolver (ADR-0003), never a
// control-plane pool, and carry no tenant_id column (the database boundary is the
// tenant boundary, C-1). Every request resolves its tenant from the authenticated
// principal (FR-ACC-03), never a parameter.
//
// Async logging (FR-RET-09 "asynchronously"): Logger.Log NEVER blocks or fails the
// query response. It spawns a background goroutine that opens a FRESH tenant.DB
// handle from the resolver (pools are cached, so this is cheap) using its OWN
// bounded context — the request context is already cancelled once the response has
// been written. A write failure is logged (without query content, C-4) and
// swallowed; it never reaches the client.
//
// ponytail: best-effort fire-and-forget — ONE goroutine per query, unbounded. The
// known ceiling is goroutine growth under a query flood (no backpressure); the
// upgrade path is a bounded worker pool / channel queue that sheds or blocks when
// full (ADR-0058). Wait() lets a graceful shutdown / test drain the in-flight
// writes.
package querylog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/answer"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// idPrefix is the query response id prefix minted by internal/answer ("q_<uuid>",
// SPEC-06 §6). query_log.id is a uuid column, so the prefix is stripped on write
// and re-applied on read, keeping the API id stable while the stored key stays a
// native uuid (and query_feedback.query_id can FK-reference it).
const idPrefix = "q_"

// defaultWriteTimeout bounds a single background log/feedback write so a slow or
// wedged tenant database cannot leak goroutines forever.
const defaultWriteTimeout = 5 * time.Second

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// Package sentinels. They are internal decision signals the handler maps to the
// SPEC-07 §1 error envelope; callers match with errors.Is / errors.As.
var (
	// ErrQueryNotFound is a feedback write for a query_id that does not exist in
	// this tenant's query_log (the tenant-DB scoping IS the ownership check).
	ErrQueryNotFound = errors.New("querylog: query not found")
	// ErrTenantUnavailable wraps every resolver outcome that means the tenant is
	// not ready to serve. The handler returns tenant_unavailable.
	ErrTenantUnavailable = errors.New("querylog: tenant unavailable")
)

// ValidationError is a 400 with a client-safe message (bad rating, query id, or
// cursor).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) *ValidationError {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// RetrievedChunk is one entry of query_log.retrieved: the chunk id, its score and
// its rank (1-based) in the retrieved set (FR-RET-09). It is the JSON shape stored
// and returned.
type RetrievedChunk struct {
	ChunkID string  `json:"chunk_id"`
	Score   float64 `json:"score"`
	Rank    int     `json:"rank"`
}

// Record is a query_log row to persist (mapped from answer.QueryRecord). Only the
// columns FR-RET-09 requires are populated; the answer text is deliberately not
// carried by the QueryLogger seam and stays null.
type Record struct {
	ID           uuid.UUID
	RequestID    string
	Question     string
	Retrieved    []RetrievedChunk
	Citations    []string // cited chunk ids
	LLMModel     string
	RetrievalMs  int
	GenerationMs int
	InTokens     int
	OutTokens    int
	Grounded     bool
}

// Feedback is a query_feedback upsert (FR-RET-10): a thumbs rating (±1) with an
// optional comment, keyed by the query it rates.
type Feedback struct {
	QueryID uuid.UUID
	Rating  int
	Comment string
}

// Entry is one admin query-log row with its optional feedback (SPEC-07 §2g).
type Entry struct {
	ID           string           `json:"id"`
	Question     string           `json:"question"`
	Grounded     bool             `json:"grounded"`
	Retrieved    []RetrievedChunk `json:"retrieved"`
	Citations    []string         `json:"citations"`
	LLMModel     string           `json:"llm_model,omitempty"`
	RetrievalMs  int              `json:"retrieval_ms"`
	GenerationMs int              `json:"generation_ms"`
	InTokens     int              `json:"in_tokens"`
	OutTokens    int              `json:"out_tokens"`
	CreatedAt    time.Time        `json:"created_at"`
	Feedback     *EntryFeedback   `json:"feedback,omitempty"`
}

// EntryFeedback is the joined query_feedback for an Entry, or nil when none exists.
type EntryFeedback struct {
	Rating    int       `json:"rating"`
	Comment   string    `json:"comment,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Cursor is the opaque keyset position for the admin list: the (created_at, id) of
// the last returned row (newest-first order).
type Cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// Page is one page of admin query-log entries.
type Page struct {
	Items      []Entry `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// Store is the tenant-content persistence port for query_log / query_feedback. It
// is reached ONLY through a *tenant.DB (ADR-0003, C-3): every method takes the
// resolved handle, so there is no way to touch another tenant's content and no
// tenant_id filter. TenantStore implements it over a live tenant database; unit
// tests use a fake (a *tenant.DB is unforgeable by design, so the SQL is covered
// by the e2e suite).
type Store interface {
	Insert(ctx context.Context, db *tenant.DB, rec Record) error
	// UpsertFeedback writes (or replaces) the feedback for a query. It returns
	// ErrQueryNotFound when the query_id is not in this tenant's query_log.
	UpsertFeedback(ctx context.Context, db *tenant.DB, fb Feedback) error
	// List returns a page (newest-first) of query_log rows with their joined
	// feedback; limit rows starting after cur (nil = first page).
	List(ctx context.Context, db *tenant.DB, limit int, cur *Cursor) ([]Entry, error)
}

// Logger is the async query-log writer (answer.QueryLogger). See the package doc
// for the fire-and-forget design and its ceiling.
type Logger struct {
	Resolver tenant.Resolver
	Store    Store
	Slog     *slog.Logger
	// Timeout bounds a single background write (default defaultWriteTimeout).
	Timeout time.Duration

	wg sync.WaitGroup
}

// Log implements answer.QueryLogger. It maps the record, then persists it on a
// background goroutine so the query response is never blocked or failed by logging.
func (l *Logger) Log(ctx context.Context, rec answer.QueryRecord) {
	if l == nil || l.Resolver == nil || l.Store == nil {
		return
	}
	id, ok := parseQueryID(rec.ID)
	if !ok {
		// A response id we can't key on is unloggable; nothing correlates to it
		// (feedback keys on the same id), so drop it rather than mint a new one.
		l.warn(ctx, "skip: unparseable response id")
		return
	}
	tid, err := uuid.Parse(rec.TenantID)
	if err != nil {
		l.warn(ctx, "skip: missing/invalid tenant")
		return
	}

	// Capture the request id from the (soon-cancelled) request context before the
	// goroutine detaches onto its own context.
	requestID := obs.RequestIDFromContext(ctx)
	stored := mapRecord(id, requestID, rec)

	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer l.recover(tid)

		wctx, cancel := context.WithTimeout(context.Background(), l.timeout())
		defer cancel()

		db, err := l.Resolver.Open(wctx, tenant.ID(tid))
		if err != nil {
			// A logging failure is best-effort: log without content (C-4) and swallow.
			l.logger().WarnContext(wctx, "querylog: open tenant failed", "tenant", tid.String(), "err", err)
			return
		}
		if err := l.Store.Insert(wctx, db, stored); err != nil {
			l.logger().WarnContext(wctx, "querylog: insert failed", "tenant", tid.String(), "err", err)
		}
	}()
}

// Wait blocks until all in-flight background writes finish. Used by graceful
// shutdown (so a final query's log is not lost) and by tests.
func (l *Logger) Wait() { l.wg.Wait() }

func (l *Logger) timeout() time.Duration {
	if l.Timeout > 0 {
		return l.Timeout
	}
	return defaultWriteTimeout
}

func (l *Logger) logger() *slog.Logger {
	if l.Slog != nil {
		return l.Slog
	}
	return slog.Default()
}

func (l *Logger) warn(ctx context.Context, msg string) {
	l.logger().WarnContext(ctx, "querylog: "+msg)
}

// recover swallows a panic in the background write so a logging bug can never take
// down the process; the query response has already been sent.
func (l *Logger) recover(tid uuid.UUID) {
	if r := recover(); r != nil {
		l.logger().Error("querylog: recovered from panic in async write", "tenant", tid.String(), "panic", r)
	}
}

// mapRecord maps the answer.QueryRecord seam onto the query_log column shape
// (FR-RET-09). retrieved carries {chunk_id, score, rank}; citations the cited
// chunk ids; usage/model the timings, tokens and model.
func mapRecord(id uuid.UUID, requestID string, rec answer.QueryRecord) Record {
	retrieved := make([]RetrievedChunk, len(rec.RetrievedChunkIDs))
	for i, cid := range rec.RetrievedChunkIDs {
		rc := RetrievedChunk{ChunkID: cid, Rank: i + 1}
		if i < len(rec.RetrievedScores) {
			rc.Score = rec.RetrievedScores[i]
		}
		retrieved[i] = rc
	}
	citations := rec.CitationChunkIDs
	if citations == nil {
		citations = []string{}
	}
	return Record{
		ID:           id,
		RequestID:    requestID,
		Question:     rec.Question,
		Retrieved:    retrieved,
		Citations:    citations,
		LLMModel:     rec.Model,
		RetrievalMs:  int(rec.Usage.RetrievalMs),
		GenerationMs: int(rec.Usage.GenerationMs),
		InTokens:     rec.Usage.InTokens,
		OutTokens:    rec.Usage.OutTokens,
		Grounded:     rec.Grounded,
	}
}

// Service serves the feedback endpoint and the admin query-log listing. It owns
// the resolver (the only source of a tenant.DB, ADR-0003) and the tenant-content
// Store. It is stateless and safe for concurrent use.
type Service struct {
	Resolver tenant.Resolver
	Store    Store
}

// open resolves the tenant to its *tenant.DB, mapping the resolver's lifecycle
// outcomes to the package sentinel so the handler never leaks internal detail.
func (s *Service) open(ctx context.Context, tid tenant.ID) (*tenant.DB, error) {
	db, err := s.Resolver.Open(ctx, tid)
	if err != nil {
		switch {
		case errors.Is(err, tenant.ErrTenantUnavailable),
			errors.Is(err, tenant.ErrTenantNotFound),
			errors.Is(err, tenant.ErrSchemaOutdated):
			return nil, ErrTenantUnavailable
		default:
			return nil, fmt.Errorf("querylog: open tenant: %w", err)
		}
	}
	return db, nil
}

// Feedback records a user's rating for a query (FR-RET-10). rating must be ±1
// (matching the query_feedback check constraint). The write is a last-write-wins
// upsert keyed by query_id (its primary key); an unknown query_id is
// ErrQueryNotFound. Ownership is enforced structurally: the write goes to the
// tenant's own database, so a query_id from another tenant simply is not found.
func (s *Service) Feedback(ctx context.Context, tid tenant.ID, queryID string, rating int, comment string) error {
	if rating != 1 && rating != -1 {
		return invalid("rating must be 1 (thumbs up) or -1 (thumbs down)")
	}
	id, ok := parseQueryID(queryID)
	if !ok {
		return invalid("query_id must be a query id")
	}
	db, err := s.open(ctx, tid)
	if err != nil {
		return err
	}
	if err := s.Store.UpsertFeedback(ctx, db, Feedback{QueryID: id, Rating: rating, Comment: comment}); err != nil {
		if errors.Is(err, ErrQueryNotFound) || errors.Is(err, tenant.ErrReadOnly) {
			return err
		}
		return fmt.Errorf("querylog: upsert feedback: %w", err)
	}
	return nil
}

// List returns a page of the tenant's query log with joined feedback, newest
// first (FR-RET-09 admin visibility). It fetches limit+1 to detect a further page.
func (s *Service) List(ctx context.Context, tid tenant.ID, limit int, cursor string) (Page, error) {
	var cur *Cursor
	if cursor != "" {
		c, err := decodeCursor(cursor)
		if err != nil {
			return Page{}, invalid("invalid cursor")
		}
		cur = c
	}
	limit = clampLimit(limit)

	db, err := s.open(ctx, tid)
	if err != nil {
		return Page{}, err
	}
	rows, err := s.Store.List(ctx, db, limit+1, cur)
	if err != nil {
		return Page{}, fmt.Errorf("querylog: list: %w", err)
	}
	page := Page{Items: rows}
	if len(rows) > limit {
		page.Items = rows[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: strings.TrimPrefix(last.ID, idPrefix)})
	}
	if page.Items == nil {
		page.Items = []Entry{}
	}
	return page, nil
}

// clampLimit applies the default and maximum page sizes.
func clampLimit(n int) int {
	if n <= 0 {
		return defaultListLimit
	}
	if n > maxListLimit {
		return maxListLimit
	}
	return n
}

// parseQueryID strips the optional "q_" response-id prefix and parses the uuid.
func parseQueryID(id string) (uuid.UUID, bool) {
	u, err := uuid.Parse(strings.TrimPrefix(id, idPrefix))
	if err != nil {
		return uuid.Nil, false
	}
	return u, true
}
